package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"

	"gitdr.io/gitdr/internal/config"
	"gitdr.io/gitdr/internal/dest"
	s3backend "gitdr.io/gitdr/internal/dest/s3"
	"gitdr.io/gitdr/internal/source"
)

// doctorSchema names the document `doctor --output json` prints. SPEC §11, gitdr.doctor/v1.
const doctorSchema = "gitdr.doctor/v1"

// doctorResponseLimit is the most doctor reads of any one answer from an S3 store. failure() says
// "1 MiB" in words, so the two change together.
const doctorResponseLimit = 1 << 20

type checkResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`

	// The worm check's answer as data, on that check alone. Its three fields are always there on
	// it, null when there is nothing to report, and absent from every other check.
	*WormAnswer
	// Observed is the retention check's answer as data, on that check alone: present, absent,
	// unreadable or none.
	Observed string `json:"observed,omitempty"`
}

// WormAnswer is what the worm check found, for a reader that must not parse Detail. Exported only
// because encoding/json will not fill an unexported embedded pointer when it decodes one.
type WormAnswer struct {
	// Verdict is the manifest's wormVerdict word for what the store answered, or nil when the
	// check ended in an error instead of an answer.
	Verdict *string `json:"verdict"`
	// Mode is the lock's mode when it is a word gitdr knows, or nil.
	Mode *string `json:"mode"`
	// Code says why the store did not answer the question: its own error code, shaped, or one of
	// the codes errorCode gives a failure that never reached it. Nil when it answered.
	Code *string `json:"code"`
}

// What the retention check's observed field holds. gitdr.doctor/v1.
const (
	observedPresent    = "present"    // the store returned a retention for an object here
	observedAbsent     = "absent"     // the store said an object here holds none
	observedUnreadable = "unreadable" // the listing or the read failed, or the store would not say
	observedNone       = "none"       // the first page of the listing held no object to look at
)

// runDoctor runs read-only preflight checks: tooling, config, source auth, and the WORM lock. It
// writes nothing to the destination.
//
// -only destination runs the destination's checks and nothing else, so it needs no source, no git
// and no git-lfs: a bucket can be checked before anything is connected to back up into it.
func runDoctor(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	common := registerCommon(fs)
	only := fs.String("only", "", `run one group of checks: "destination" checks the bucket alone, with no source, git or git-lfs`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	destOnly, err := doctorScope(fs, *only)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	cfg, log, err := common.load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}

	var checks []checkResult
	add := func(name string, ok bool, detail string) {
		checks = append(checks, checkResult{Name: name, OK: ok, Detail: detail})
	}

	if !destOnly {
		if _, err := exec.LookPath("git"); err != nil {
			add("git", false, "not found on PATH")
		} else {
			add("git", true, "found")
		}
		if _, err := exec.LookPath("git-lfs"); err != nil {
			add("git-lfs", true, "not found (LFS objects will be skipped)") // optional, not a failure
		} else {
			add("git-lfs", true, "found")
		}
	}

	validate := cfg.Validate
	if destOnly {
		validate = cfg.ValidateDestination // no source: the bucket is checked on its own
	}
	if err := validate(); err != nil {
		add("config", false, err.Error())
		return emitDoctor(common.output, checks)
	}
	add("config", true, "valid")

	if !destOnly {
		if cfg.Encryption.Enabled {
			if _, err := resolveEncryptionKey(cfg); err != nil {
				add("encryption key", false, err.Error())
			} else {
				add("encryption key", true, "valid 32-byte key")
			}
		}

		if src, err := buildSource(cfg, log); err != nil {
			add("source", false, err.Error())
		} else {
			c := checkSource(ctx, src)
			add(c.Name, c.OK, c.Detail)
		}
	}

	if dst, err := doctorDest(ctx, cfg, log); err != nil {
		add("destination", false, err.Error())
	} else {
		checks = append(checks, checkDestination(ctx, dst, cfg.WORM.Require, log)...)
	}

	return emitDoctor(common.output, checks)
}

// doctorScope reads -only. Not given, every check runs; "destination" runs the destination's
// alone. Anything else is a usage error, an empty value given on purpose included, since that is
// a caller asking for a group of checks that does not exist.
func doctorScope(fs *flag.FlagSet, only string) (destOnly bool, err error) {
	given := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "only" {
			given = true
		}
	})
	switch {
	case only == "destination":
		return true, nil
	case !given:
		return false, nil
	default:
		return false, fmt.Errorf(`doctor: -only takes "destination", not %q`, only)
	}
}

// doctorDest builds the destination the way every command does, then caps each answer it reads
// from an S3 store at doctorResponseLimit. doctor is what gets pointed at an endpoint nobody has
// checked yet, and the SDK otherwise reads all of an answer, however long it is.
func doctorDest(ctx context.Context, cfg *config.Config, log *slog.Logger) (dest.Destination, error) {
	dst, err := buildDest(ctx, cfg, log)
	if err != nil {
		return nil, err
	}
	if b, ok := dst.(*s3backend.Backend); ok {
		b.LimitResponses(doctorResponseLimit)
	}
	return dst, nil
}

// checkDestination asks a destination what it locks and, when it says it locks, reads the
// retention on an object already there. It never writes, and it reads one page of the listing.
//
// No check's detail carries text the store wrote, except an error code of the shape
// dest.ShapedCode allows. The store's own words go to the log, which is stderr.
func checkDestination(ctx context.Context, dst dest.Destination, require bool, log *slog.Logger) []checkResult {
	worm, st := checkWorm(ctx, dst, require, log)
	checks := []checkResult{worm}

	/*
	 * And what is actually on an object, which is a different question from what the bucket
	 * says about itself. A store can report Object Lock enabled, accept a write, and apply
	 * nothing.
	 *
	 * An object already under the prefix, never a canary: on a compliance-locked bucket a
	 * probe object is undeletable litter for the whole retention window, by construction. So
	 * this says nothing on an empty destination, which is honest - there is nothing there to
	 * look at yet.
	 *
	 * This is where the *confirming* direction belongs. A backup only ever lowers its verdict
	 * from this observation, because one retained object proves the store implements the
	 * headers and nothing about the rest; doctor is a deliberate diagnostic rather than a
	 * claim in a signed document, so here it may say what it saw.
	 */
	if st.Verdict.Immutable() {
		if c, ok := checkRetention(ctx, dst, require, log); ok {
			checks = append(checks, c)
		}
	}
	return checks
}

// checkWorm is the worm check, and the status it read, for the retention check to go on from.
func checkWorm(ctx context.Context, dst dest.Destination, require bool, log *slog.Logger) (checkResult, dest.WormStatus) {
	c := checkResult{Name: "worm", WormAnswer: &WormAnswer{}}
	st, err := dst.VerifyWorm(ctx)
	if err != nil {
		code := errorCode(err)
		log.Warn("doctor: could not verify the destination's immutability", "code", code, "err", err)
		c.OK, c.Code = !require, &code
		c.Detail = "could not verify immutability: " + failure(code)
		return c, dest.WormStatus{}
	}

	verdict := st.Verdict.Wire()
	c.Verdict, c.Mode = &verdict, knownMode(st.Mode)
	if st.Refusal != nil {
		code := errorCode(st.Refusal)
		log.Warn("doctor: the destination would not say what it locks", "code", code, "err", st.Refusal)
		c.Code = &code
	}
	switch {
	case st.Verdict.Immutable():
		c.OK, c.Detail = true, "immutable, "+st.Details
	case require:
		c.OK, c.Detail = false, "NOT immutable ("+st.Details+"); worm.require is set, backup would fail"
	// Unknown is not a quieter version of absent, so it does not borrow its words. What a
	// reader needs here is that gitdr could not see the answer and where to go instead.
	case st.Verdict == dest.VerdictUnknown:
		c.OK, c.Detail = true, "could not read immutability ("+st.Details+"); check with the provider, backup warns and proceeds"
	default:
		c.OK, c.Detail = true, "NOT immutable ("+st.Details+"), WORM recommended; backup warns and proceeds"
	}
	return c, st
}

// knownMode is a lock mode as doctor reports it: a word gitdr knows, or nil. On S3 the mode is
// whatever the store wrote, so one gitdr does not know is reported as none rather than repeated.
func knownMode(mode string) *string {
	switch mode {
	case "COMPLIANCE", "GOVERNANCE", "RETENTION", "IMMUTABILITY":
		return &mode
	}
	return nil
}

// checkRetention reads the retention on one object already under the destination, and reports
// false when the destination cannot be asked.
func checkRetention(ctx context.Context, dst dest.Destination, require bool, log *slog.Logger) (checkResult, bool) {
	observer, canObserve := dst.(dest.RetentionObserver)
	lister, canList := dst.(dest.PageLister)
	if !canObserve || !canList {
		return checkResult{}, false
	}

	c := checkResult{Name: "retention", OK: true}
	switch key, more, err := anyObject(ctx, lister); {
	case err != nil:
		code := errorCode(err)
		log.Warn("doctor: could not list the destination", "code", code, "err", err)
		c.Observed = observedUnreadable
		c.Detail = "could not list the destination to find an object to check: " + failure(code)
	case key == "" && more:
		// A page with nothing on it that says there is more. Neither S3 nor Azure promises a full
		// page, and doctor does not read a second one.
		c.Observed = observedNone
		c.Detail = "the first page of the listing named no object and doctor reads no further, so there is no object to check"
	case key == "":
		c.Observed, c.Detail = observedNone, "nothing written here yet, so there is no object to check"
	default:
		got, until, err := observer.ObserveRetention(ctx, key)
		switch got {
		case dest.RetentionPresent:
			c.Observed, c.Detail = observedPresent, "an object here is held until "+until.Format(time.RFC3339)
		case dest.RetentionAbsent:
			// The bucket said it locks and the object holds nothing. This is the failure
			// the whole gate exists to prevent, and until now nothing could see it.
			c.Observed, c.OK = observedAbsent, !require
			c.Detail = "this bucket reports object lock and an object here carries no retention"
		default:
			c.Observed, c.Detail = observedUnreadable, "could not read the retention on an object here"
			answered := err == nil
			if err != nil {
				code := errorCode(err)
				log.Warn("doctor: could not read the retention on an object", "key", key, "code", code, "err", err)
				c.Detail += ": " + failure(code)
				answered = storeAnswered(code)
			}
			// Only where the store itself declined: after a dropped connection it is no advice.
			if answered {
				c.Detail += "; on S3 this needs s3:GetObjectRetention, which a create-only credential will not have"
			}
		}
	}
	return c, true
}

// tokenChecker is a source whose token somebody else minted. Asking it for a header only reads
// the token back, which proves nothing about whether GitHub will take it, so doctor has it make
// a request instead.
type tokenChecker interface {
	UsesTokenFile() bool
	CheckToken(ctx context.Context) error
}

// checkSource tests the credential of a source that was built.
func checkSource(ctx context.Context, src source.Source) checkResult {
	if tc, ok := src.(tokenChecker); ok && tc.UsesTokenFile() {
		if err := tc.CheckToken(ctx); err != nil {
			return checkResult{Name: "source auth", OK: false, Detail: err.Error()}
		}
		return checkResult{Name: "source auth", OK: true, Detail: "token file read and accepted by GitHub"}
	}
	if ga, ok := src.(source.GitAuther); ok {
		if _, err := ga.GitAuthHeader(ctx); err != nil {
			return checkResult{Name: "source auth", OK: false, Detail: err.Error()}
		}
		return checkResult{Name: "source auth", OK: true, Detail: "installation token minted"}
	}
	return checkResult{Name: "source", OK: true, Detail: "built"}
}

func emitDoctor(output string, checks []checkResult) int {
	ok := true
	for _, c := range checks {
		if !c.OK {
			ok = false
		}
	}
	if output == "json" {
		b, _ := json.MarshalIndent(struct {
			Schema string        `json:"schema"`
			OK     bool          `json:"ok"`
			Checks []checkResult `json:"checks"`
		}{doctorSchema, ok, checks}, "", "  ")
		fmt.Println(string(b))
	} else {
		for _, c := range checks {
			status := "ok"
			if !c.OK {
				status = "FAIL"
			}
			fmt.Printf("[%s] %s, %s\n", status, c.Name, c.Detail)
		}
	}
	if !ok {
		return 1
	}
	return 0
}

// An object already under the configured prefix, from the first page of the listing, or "" when
// that page names none; more says the store reported further pages.
//
// Any object answers the question, which is whether retention is landing on writes to this
// bucket at all. `dest.Object` carries no timestamp, and picking "the newest" would mean
// inferring one from the key format - an assumption this does not need to make.
//
// One page, never the whole listing: on a large bucket the walk is thousands of requests for an
// answer the first page already holds, and against an endpoint that answers every page with
// another one it never ends.
//
// Deliberately not a write. Anything gitdr puts on a compliance-locked bucket to look at is
// undeletable for the whole retention window, and a diagnostic that leaves litter behind is one
// people stop running.
func anyObject(ctx context.Context, lister dest.PageLister) (key string, more bool, err error) {
	objs, more, err := lister.ListPage(ctx, "", 1)
	if err != nil {
		return "", false, err
	}
	for _, o := range objs {
		if o.Key != "" {
			return o.Key, more, nil
		}
	}
	return "", more, nil
}
