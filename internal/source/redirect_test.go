package source

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// countingTransport stands in for the network: it counts the requests it is handed and answers
// each with 200.
type countingTransport struct{ n int }

func (c *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	c.n++
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

// hop is a request to rawURL, a hop of a redirect from the URL from: "" for a first request, and
// "?" for a hop whose redirect response names no request.
func hop(t *testing.T, rawURL, from string) (*http.Request, *closeRecorder) {
	t.Helper()
	body := &closeRecorder{Reader: strings.NewReader("{}")}
	req, err := http.NewRequest(http.MethodPost, rawURL, body)
	if err != nil {
		t.Fatal(err)
	}
	switch from {
	case "":
	case "?":
		req.Response = &http.Response{StatusCode: http.StatusFound}
	default:
		prev, err := http.NewRequest(http.MethodGet, from, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Response = &http.Response{StatusCode: http.StatusFound, Request: prev}
	}
	return req, body
}

// assertHop sends req through tr and checks the outcome: sent, when want is nil, or refused as
// want, before anything reached the network.
func assertHop(t *testing.T, tr http.RoundTripper, req *http.Request, body *closeRecorder, next *countingTransport, want *RedirectError) {
	t.Helper()
	resp, err := tr.RoundTrip(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if want == nil {
		if err != nil || next.n != 1 {
			t.Fatalf("RoundTrip = %v with %d requests sent, want it sent", err, next.n)
		}
		return
	}
	refused, ok := errors.AsType[*RedirectError](err)
	if !ok {
		t.Fatalf("RoundTrip = %v, want a refusal", err)
	}
	if *refused != *want {
		t.Errorf("refusal = %+v, want %+v", *refused, *want)
	}
	if next.n != 0 {
		t.Errorf("%d requests were sent", next.n)
	}
	if !body.closed {
		t.Error("the request body was not closed")
	}
}

// Which hops KeepTLS lets through: everything but a redirect that leaves https, and a refused one
// never reaches the network. Each hop is judged by the one before it, so a chain that starts over
// plain http and goes to https may not go back.
func TestKeepTLSRefusesARedirectThatLeavesHTTPS(t *testing.T) {
	for _, tc := range []struct {
		name, url, from string
		refused         *RedirectError
	}{
		{name: "a first request over https", url: "https://api.example/x"},
		{name: "a first request over plain http", url: "http://api.example/x"},
		{name: "https to https on another host", url: "https://other.example/x", from: "https://api.example/x"},
		{name: "http to https", url: "https://api.example/x", from: "http://api.example/x"},
		{name: "http to http", url: "http://other.example/x", from: "http://api.example/x"},
		{name: "https to http on another host", url: "http://other.example/x?sig=1", from: "https://api.example/x",
			refused: &RedirectError{From: "https://api.example", To: "http://other.example", Reason: "it leaves TLS"}},
		{name: "https to http on the same host", url: "http://api.example/x", from: "https://api.example/x",
			refused: &RedirectError{From: "https://api.example", To: "http://api.example", Reason: "it leaves TLS"}},
		{name: "https to another scheme", url: "ftp://other.example/x", from: "https://api.example/x",
			refused: &RedirectError{From: "https://api.example", To: "ftp://other.example", Reason: "it leaves TLS"}},
		{name: "http from a response that names no request", url: "http://other.example/x", from: "?",
			refused: &RedirectError{To: "http://other.example", Reason: "it leaves TLS"}},
		{name: "https from a response that names no request", url: "https://other.example/x", from: "?"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := &countingTransport{}
			req, body := hop(t, tc.url, tc.from)
			assertHop(t, KeepTLS(next), req, body, next, tc.refused)
		})
	}
}

// Which hops StayOnAPI lets through: those to the API's own scheme and host, compared exactly, and
// no other. It judges hops only; a first request is the client's to aim.
func TestStayOnAPIRefusesARedirectOffTheAPI(t *testing.T) {
	https := &url.URL{Scheme: "https", Host: "api.example"}
	for _, tc := range []struct {
		name, url, from string
		api             *url.URL
		refused         *RedirectError
	}{
		{name: "a first request anywhere", api: https, url: "https://other.example/x"},
		{name: "a hop on the API", api: https, url: "https://api.example/repositories/1", from: "https://api.example/repos/o/r"},
		{name: "a hop to another host", api: https, url: "https://other.example/landed?sig=1", from: "https://api.example/x",
			refused: &RedirectError{From: "https://api.example", To: "https://other.example", Reason: "it leaves the API"}},
		{name: "a hop to a host that starts with the API host", api: https, url: "https://api.example.net/x", from: "https://api.example/x",
			refused: &RedirectError{From: "https://api.example", To: "https://api.example.net", Reason: "it leaves the API"}},
		{name: "a hop to the API host on another port", api: https, url: "https://api.example:8443/x", from: "https://api.example/x",
			refused: &RedirectError{From: "https://api.example", To: "https://api.example:8443", Reason: "it leaves the API"}},
		{name: "a hop to the API host spelled in capitals", api: https, url: "https://API.EXAMPLE/x", from: "https://api.example/x",
			refused: &RedirectError{From: "https://api.example", To: "https://API.EXAMPLE", Reason: "it leaves the API"}},
		{name: "a hop to the API host over plain http", api: https, url: "http://api.example/x", from: "https://api.example/x",
			refused: &RedirectError{From: "https://api.example", To: "http://api.example", Reason: "it leaves the API"}},
		{name: "a hop from a plain http API to https", api: &url.URL{Scheme: "http", Host: "api.example"}, url: "https://api.example/x", from: "http://api.example/x",
			refused: &RedirectError{From: "http://api.example", To: "https://api.example", Reason: "it leaves the API"}},
		{name: "a hop from a response that names no request", api: https, url: "https://other.example/x", from: "?",
			refused: &RedirectError{To: "https://other.example", Reason: "it leaves the API"}},
		{name: "any hop, for an API with no host", api: &url.URL{}, url: "https://api.example/x", from: "https://api.example/x",
			refused: &RedirectError{From: "https://api.example", To: "https://api.example", Reason: "it leaves the API"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := &countingTransport{}
			req, body := hop(t, tc.url, tc.from)
			assertHop(t, StayOnAPI(tc.api, next), req, body, next, tc.refused)
		})
	}
}

// What a source reports for a refused redirect: the refusal alone, out of the url.Error Go's client
// wraps it in, which quotes the URL the redirect chose. Every other error passes unchanged.
func TestScrubRedirectReportsTheRefusalAlone(t *testing.T) {
	refused := &RedirectError{From: "https://api.example", To: "https://other.example", Reason: "it leaves the API"}
	wrapped := fmt.Errorf("the API client: %w", &url.Error{Op: "Get", URL: "https://other.example/landed?sig=secret", Err: refused})
	if got := ScrubRedirect(wrapped); got != error(refused) {
		t.Errorf("ScrubRedirect(%v) = %v, want the refusal alone", wrapped, got)
	}
	other := &url.Error{Op: "Get", URL: "https://api.example/x", Err: errors.New("connection refused")}
	if got := ScrubRedirect(other); got != error(other) {
		t.Errorf("ScrubRedirect changed an error that is not a refusal: %v", got)
	}
	if ScrubRedirect(nil) != nil {
		t.Error("ScrubRedirect(nil) is not nil")
	}
	if got, want := refused.Error(), "refusing a redirect from https://api.example to https://other.example: it leaves the API"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if got, want := (&RedirectError{To: "http://other.example", Reason: "it leaves TLS"}).Error(), "refusing a redirect to http://other.example: it leaves TLS"; got != want {
		t.Errorf("Error() with no From = %q, want %q", got, want)
	}
}
