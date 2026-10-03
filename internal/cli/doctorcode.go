package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"syscall"

	"cloud.google.com/go/storage"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	smithy "github.com/aws/smithy-go"
	"google.golang.org/api/googleapi"

	"gitdr.io/gitdr/internal/dest"
)

// The codes doctor gives a failure that carries no error code from the store, because the store
// never answered or its answer could not be read. gitdr.doctor/v1: a reader switches on these, so
// they are added to and never renamed.
const (
	codeDNS      = "dns"       // the endpoint's host name did not resolve
	codeConnect  = "connect"   // no connection, or it broke before the answer was complete
	codeTLS      = "tls"       // the TLS handshake or the certificate was refused
	codeTimeout  = "timeout"   // no answer in time, or the run was stopped first
	codeTooLarge = "too-large" // an answer ran past doctorResponseLimit
	codeNotS3    = "not-s3"    // an answer that is not the storage API's, or a failure none of these names

	// codeConfig is on the config check alone: the config could not be read or parsed.
	codeConfig = "config"
)

// errorCode says what a failed store call was, in a word that is safe to print: the store's own
// error code when it answered with one, through dest.ShapedCode, or one of the codes above.
//
// The order matters. A capped answer comes first, since what it cut short could have held
// anything, and then an answer that was not the storage API's. The store's code comes before the
// network's words, since an answer is the better fact. A context ending comes before the network
// as well, since a dial or a lookup cut short by it reports itself as their own failure.
func errorCode(err error) string {
	if errors.Is(err, dest.ErrResponseTooLarge) {
		return codeTooLarge
	}
	if errors.Is(err, dest.ErrNotStorageAPI) {
		return codeNotS3
	}
	if code, ok := storeCode(err); ok {
		return dest.ShapedCode(code)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return codeTimeout
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return codeDNS
	}
	if isTLS(err) {
		return codeTLS
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return codeTimeout
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return codeConnect
	}
	return codeNotS3
}

// storeCode is the error code a store answered with, as the store wrote it, when err carries an
// answer: S3's Code, Azure's x-ms-error-code, the reason in a Cloud Storage error. It can be
// empty, and dest.ShapedCode makes that unnamed.
func storeCode(err error) (string, bool) {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode(), true
	}
	var az *azcore.ResponseError
	if errors.As(err, &az) {
		return az.ErrorCode, true
	}
	var gcs *googleapi.Error
	if errors.As(err, &gcs) {
		for _, item := range gcs.Errors {
			if item.Reason != "" {
				return item.Reason, true
			}
		}
		return "", true
	}
	// The Cloud Storage library turns a 404 into one of these and drops the answer. notFound is
	// the reason Cloud Storage gives a 404.
	if errors.Is(err, storage.ErrBucketNotExist) || errors.Is(err, storage.ErrObjectNotExist) {
		return "notFound", true
	}
	return "", false
}

// isTLS reports a refused handshake or certificate, rather than a failed connection.
func isTLS(err error) bool {
	var (
		verify   *tls.CertificateVerificationError
		header   tls.RecordHeaderError
		alert    tls.AlertError
		unknown  x509.UnknownAuthorityError
		hostname x509.HostnameError
		invalid  x509.CertificateInvalidError
		op       *net.OpError
	)
	switch {
	case errors.As(err, &verify), errors.As(err, &header), errors.As(err, &alert),
		errors.As(err, &unknown), errors.As(err, &hostname), errors.As(err, &invalid),
		errors.Is(err, http.ErrSchemeMismatch):
		return true
	case errors.As(err, &op):
		// crypto/tls reports an alert, sent or received, as a net.OpError with one of these.
		return op.Op == "remote error" || op.Op == "local error"
	}
	return false
}

// storeAnswered reports whether code came from the store, as opposed to naming a failure that
// never reached it.
func storeAnswered(code string) bool {
	switch code {
	case codeDNS, codeConnect, codeTLS, codeTimeout, codeTooLarge, codeNotS3:
		return false
	}
	return true
}

// failure says in words what a code means, with the code beside the words so the two cannot
// drift apart.
func failure(code string) string {
	switch code {
	case codeDNS:
		return "the endpoint's host name did not resolve (dns)"
	case codeConnect:
		return "the connection to the endpoint failed (connect)"
	case codeTLS:
		return "the TLS handshake with the endpoint failed (tls)"
	case codeTimeout:
		return "no answer came in time (timeout)"
	case codeTooLarge:
		return "an answer ran past 1 MiB, the most doctor reads of one (too-large)"
	case codeNotS3:
		return "the endpoint did not answer as the storage API does (not-s3)"
	}
	return "the store answered " + code
}
