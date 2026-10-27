package source

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// A source's API client follows redirects, and two rules decide which. Both judge each hop in the
// transport, before it is sent, rather than in the client's CheckRedirect: an error from
// CheckRedirect comes back together with the redirect's response, and client-go retries a GET that
// failed with a response, sending it six times over twelve seconds for every refusal. An error from
// the transport comes back alone, and fails the request at once.

// RedirectError is a redirect a source refused to follow. It names the two ends by scheme and host
// alone: where a redirect points is whoever sent it's to choose, and the rest of that URL, its
// query string above all, can carry anything, a credential included.
type RedirectError struct {
	From   string // the scheme and host redirected from, or "" when the response does not say
	To     string // the scheme and host redirected to
	Reason string
}

func (e *RedirectError) Error() string {
	if e.From == "" {
		return fmt.Sprintf("refusing a redirect to %s: %s", e.To, e.Reason)
	}
	return fmt.Sprintf("refusing a redirect from %s to %s: %s", e.From, e.To, e.Reason)
}

// ScrubRedirect returns err, or, when err is a redirect a source refused, the refusal alone. Go's
// client wraps a transport's error in a url.Error quoting the URL it was for, path and query string
// included, and for a refused hop that URL is the one the redirect chose. A source passes every
// error from its API client through this before it adds its own words.
func ScrubRedirect(err error) error {
	if refused, ok := errors.AsType[*RedirectError](err); ok {
		return refused
	}
	return err
}

// KeepTLS returns next with one rule added: a redirect from an https URL to one that is not https
// fails before anything is sent. Without it a redirect to plain http sends the request, and any
// credential on it, in the clear, and reads an answer anyone on the way could have written.
func KeepTLS(next http.RoundTripper) http.RoundTripper { return keepTLS{next: next} }

type keepTLS struct{ next http.RoundTripper }

func (t keepTLS) RoundTrip(req *http.Request) (*http.Response, error) {
	// Go's client sets Response on each hop of a redirect, and only there, and the transport sets
	// the Request that response answered. A response that names no request is taken to have come
	// over https, the safe way to be wrong.
	if req.URL.Scheme != "https" && req.Response != nil {
		if prev := req.Response.Request; prev == nil || prev.URL == nil || prev.URL.Scheme == "https" {
			return refuse(req, "it leaves TLS")
		}
	}
	return t.next.RoundTrip(req)
}

// StayOnAPI returns next with the other rule: a hop of a redirect to anything but the scheme and
// host of api fails before anything is sent. None of the endpoints a source calls redirects off its
// API. A redirect that does is the API's server, or something in front of it, pointing somewhere
// gitdr has no reason to trust, and the answer from there would be read as the API's: a list of
// repositories whose clone URLs name that host, where git then sends the token.
//
// Scheme and host are compared exactly, port included, the way a source decides which requests
// carry its credential. A host spelled another way is another host, the safe way to be wrong, and
// an api with no host matches nothing.
func StayOnAPI(api *url.URL, next http.RoundTripper) http.RoundTripper {
	return stayOnAPI{scheme: api.Scheme, host: api.Host, next: next}
}

type stayOnAPI struct {
	scheme, host string
	next         http.RoundTripper
}

func (t stayOnAPI) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Response != nil && (t.host == "" || req.URL.Scheme != t.scheme || req.URL.Host != t.host) {
		return refuse(req, "it leaves the API")
	}
	return t.next.RoundTrip(req)
}

// refuse fails the hop req for reason, without sending it.
func refuse(req *http.Request, reason string) (*http.Response, error) {
	// A RoundTripper closes the body on every path, the ones that fail included.
	if req.Body != nil {
		_ = req.Body.Close()
	}
	from := ""
	if prev := req.Response.Request; prev != nil && prev.URL != nil {
		from = origin(prev.URL)
	}
	return nil, &RedirectError{From: from, To: origin(req.URL), Reason: reason}
}

// origin is u's scheme and host, all of a URL a refusal names.
func origin(u *url.URL) string { return u.Scheme + "://" + u.Host }
