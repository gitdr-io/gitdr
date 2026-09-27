package github

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"unicode"

	"gitdr.io/gitdr/internal/redact"
)

// An installation token somebody else minted, read from a file.
//
// The App's private key can mint a token for every installation of the App, so a run that holds
// it can reach every organisation that installed it, and a run needs read access to one. A caller
// that runs backups for many organisations, like a hosted scheduler, mints a read-only token for
// the one installation, writes it here, and replaces the file before the token's hour is up. The
// run never sees the key.
//
// Nothing here is cached. The file is read again before every API request and before every git
// command, so a replacement written during the run reaches the next request, and a file that has
// gone missing fails the request that needed it rather than one an hour later.

// maxTokenFileBytes bounds a read. An installation token is well under a hundred bytes, so a
// larger file is not one, and reading it all would only let a wrong path fill memory.
const maxTokenFileBytes = 4096

// tokenFile is a file holding one installation token.
type tokenFile struct{ path string }

// read returns the token in the file, read now.
//
// Whitespace around the token is ignored, so a file written by `echo` works. What remains must be
// non-empty and printable ASCII with no spaces, 0x21 to 0x7E: an installation token is, and the
// value goes into an HTTP header, where a newline would start a second header.
//
// Every error names the path and what is wrong with the file, and nothing of what is in it. Every
// one also starts with source.github.tokenPath, the key an operator sets, and a caller that has
// to tell an engine that reads token files from one that does not looks for that string in the
// binary.
func (f tokenFile) read() (redact.Secret, error) {
	// Stat before open: opening a named pipe blocks until something writes to it, and a run
	// stuck there would sit on the path forever instead of failing.
	info, err := os.Stat(f.path)
	if err != nil {
		return "", f.unreadable(err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("source.github.tokenPath: the token file %q is not a regular file", f.path)
	}
	fh, err := os.Open(f.path)
	if err != nil {
		return "", f.unreadable(err)
	}
	raw, err := io.ReadAll(io.LimitReader(fh, maxTokenFileBytes+1))
	defer clear(raw) // the token leaves here as a Secret, and this copy is not kept either
	if cerr := fh.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", f.unreadable(err)
	}
	if len(raw) > maxTokenFileBytes {
		return "", fmt.Errorf("source.github.tokenPath: the token file %q is larger than %d bytes, which no installation token is",
			f.path, maxTokenFileBytes)
	}

	tok := bytes.TrimSpace(raw)
	if len(tok) == 0 {
		return "", fmt.Errorf("source.github.tokenPath: the token file %q is empty", f.path)
	}
	// Where the refused byte sits in the file, which is enough to find it and says nothing of
	// what it is.
	lead := len(raw) - len(bytes.TrimLeftFunc(raw, unicode.IsSpace))
	for i, b := range tok {
		if b < 0x21 || b > 0x7e {
			return "", fmt.Errorf("source.github.tokenPath: the token file %q holds a byte a token cannot, at offset %d: "+
				"an installation token is printable ASCII with no spaces", f.path, lead+i)
		}
	}
	return redact.Secret(tok), nil
}

// unreadable wraps an error from reading the file. The path is named once, by this message, and
// the underlying error keeps only its reason, so errors.Is still finds fs.ErrNotExist.
func (f tokenFile) unreadable(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return fmt.Errorf("source.github.tokenPath: cannot read the token file %q: %w", f.path, err)
}

// tokenTransport authenticates API requests with the token in the token file.
//
// Only a request over https to the API host carries it: api.github.com, or the host of
// source.baseURL. Anything else goes out bare, a redirect to another host included. Go's client
// already drops the Authorization header on a redirect to another domain, but only when that
// header was on the request it was handed, and this transport adds the header after that point.
// So the rule has to live here, and it is the same rule for the first request and every hop.
//
// The file is read on every request that carries the token, and a read that fails fails the
// request before anything is sent.
type tokenTransport struct {
	base http.RoundTripper
	file tokenFile
	// host is compared with the request's host exactly, port included, the way both were
	// parsed from the same base URL. A different spelling of the same host goes out bare,
	// which is the safe way to be wrong.
	host string
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A RoundTripper must not modify the request it was given.
	out := req.Clone(req.Context())
	if req.URL.Scheme != "https" || req.URL.Host != t.host {
		out.Header.Del("Authorization")
		return t.base.RoundTrip(out)
	}
	tok, err := t.file.read()
	if err != nil {
		// A RoundTripper closes the body on every path, the ones that fail included.
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	out.Header.Set("Authorization", "Bearer "+tok.Reveal())
	return t.base.RoundTrip(out)
}
