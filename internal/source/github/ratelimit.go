package github

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"time"

	ghinstallation "github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v90/github"

	"gitdr.io/gitdr/internal/source"
)

// GitHub's rate limits, and its transient 5xx.
//
// go-github reports a rate limit and does nothing about it. A primary limit comes back as a
// *github.RateLimitError carrying the reset, a secondary one as a *github.AbuseRateLimitError
// carrying Retry-After when GitHub sent one. And once the client has seen the primary limit spent,
// it refuses every further request until the reset without sending it. So with no wait, the first
// refusal fails the request that met it and every request after it: on a large organisation, the
// metadata of every repository still to come.

const (
	// rateLimitWaits bounds how many times one request waits for a limit to lift. One is the
	// normal case. More means something else is spending the same limit, and past this the
	// request fails rather than waiting for ever.
	rateLimitWaits = 5
	// unnamedWait is the wait for a limit GitHub gave no time for: at least a minute, its
	// documentation says, and longer each time the limit is met again.
	unnamedWait = time.Minute
	// rateLimitSlack is added to every wait for a limit. The reset is in whole seconds, and the
	// clock here is not GitHub's.
	rateLimitSlack = time.Second
	// serverErrorAttempts bounds the tries of a request GitHub answers with a 5xx, waiting
	// serverErrorBackoff before the first retry and doubling it before each one after.
	serverErrorAttempts = 4
	serverErrorBackoff  = time.Second
)

// call sends one API request through fn, waiting out a rate limit and retrying a 5xx.
//
// A limit is waited for only when the context's deadline leaves room for the wait. When it does
// not, the request fails now, and the error names the reset: a wait that ends past the deadline
// only delays the same failure.
//
// After a wait, fn is given a context carrying github.BypassRateLimitCheck. go-github remembers a
// spent limit against its own clock and would refuse the retry without sending it; the wait just
// served is the answer to that memory, and whether the limit has lifted is GitHub's to say. A
// retry it refuses again is waited for again, up to rateLimitWaits.
//
// A limit it gives up on, and a 5xx that outlasts its tries, are marked source.ErrTransient: the
// pipeline then stores nothing for the repository, since the next run will probably get through.
func (s *Source) call(ctx context.Context, fn func(context.Context) error) error {
	reqCtx := ctx
	waits, serverErrors := 0, 0
	for {
		err := fn(reqCtx)
		if err == nil {
			return nil
		}
		now := s.now()
		if reset, limited := limitLifts(err, now, waits); limited {
			if waits == rateLimitWaits {
				return source.Transient(fmt.Errorf("rate limited after %d waits for the limit to lift: %w", waits, err))
			}
			waits++
			// Never from the past: a reset that has gone by on this clock and not on GitHub's would
			// otherwise send the retries back to back.
			resume := reset
			if resume.Before(now) {
				resume = now
			}
			resume = resume.Add(rateLimitSlack + jitter())
			if deadline, ok := ctx.Deadline(); ok && resume.After(deadline) {
				return source.Transient(fmt.Errorf("rate limited until %s, past the deadline of %s: %w",
					reset.UTC().Format(time.RFC3339), deadline.UTC().Format(time.RFC3339), err))
			}
			s.logger.Warn("github rate limit reached; waiting for it to lift",
				"until", reset.UTC().Format(time.RFC3339), "err", err)
			if serr := s.sleep(ctx, resume.Sub(now)); serr != nil {
				return source.Transient(fmt.Errorf("rate limited until %s; stopped waiting: %w", reset.UTC().Format(time.RFC3339), serr))
			}
			reqCtx = context.WithValue(ctx, github.BypassRateLimitCheck, true)
			continue
		}
		if !isServerError(err) {
			return err
		}
		if serverErrors+1 == serverErrorAttempts {
			return source.Transient(err)
		}
		wait := serverErrorBackoff<<serverErrors + jitter()
		serverErrors++
		s.logger.Warn("github server error; retrying", "in", wait.Round(time.Millisecond), "err", err)
		if serr := s.sleep(ctx, wait); serr != nil {
			return source.Transient(fmt.Errorf("stopped retrying: %w: %w", serr, err))
		}
	}
}

// limitLifts reports whether err is GitHub refusing a request for a rate limit, and when, on this
// host's clock, the limit lifts: the reset of a primary limit, Retry-After for a secondary one, and
// when GitHub named neither, unnamedWait from now, doubled for each wait already spent on the
// request.
func limitLifts(err error, now time.Time, waits int) (time.Time, bool) {
	unnamed := now.Add(unnamedWait << waits)
	if e, ok := errors.AsType[*github.RateLimitError](err); ok {
		reset := e.Rate.Reset.Time
		if reset.IsZero() {
			return unnamed, true
		}
		// GitHub names the reset on its own clock. Measured from the Date of the refusal that named
		// it, the wait does not depend on how far this host's clock is from GitHub's; a host a few
		// seconds fast otherwise retries before the reset every time, and runs out of waits. A
		// refusal go-github made itself, without asking, has no Date and is measured on this clock.
		if date := responseDate(e.Response); !date.IsZero() {
			return now.Add(reset.Sub(date)), true
		}
		return reset, true
	}
	if e, ok := errors.AsType[*github.AbuseRateLimitError](err); ok {
		if e.RetryAfter != nil && *e.RetryAfter > 0 {
			return now.Add(*e.RetryAfter), true
		}
		return unnamed, true
	}
	return time.Time{}, false
}

// responseDate is the Date of resp, or the zero time when it has none.
func responseDate(resp *http.Response) time.Time {
	if resp == nil {
		return time.Time{}
	}
	date, err := http.ParseTime(resp.Header.Get("Date"))
	if err != nil {
		return time.Time{}
	}
	return date
}

// isServerError reports whether err is GitHub answering with a 5xx: to the API request itself, or
// in App mode to the request for the installation token, which the transport makes inside the API
// request's round trip and reports as its own error rather than as go-github's.
func isServerError(err error) bool {
	var resp *http.Response
	if e, ok := errors.AsType[*github.ErrorResponse](err); ok {
		resp = e.Response
	} else if e, ok := errors.AsType[*ghinstallation.HTTPError](err); ok {
		resp = e.Response
	}
	return resp != nil && resp.StatusCode >= http.StatusInternalServerError
}

// jitter spreads the requests that waited together, so they do not all return in the same instant.
// Under a second, from crypto/rand though nothing here is secret: semgrep refuses math/rand.
func jitter() time.Duration {
	var b [8]byte
	_, _ = rand.Read(b[:]) // never fails since Go 1.24
	return time.Duration(binary.BigEndian.Uint64(b[:]) % uint64(time.Second))
}

// sleepContext waits for d, or until ctx is done.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
