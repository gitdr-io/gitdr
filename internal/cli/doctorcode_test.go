package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	smithy "github.com/aws/smithy-go"
	"google.golang.org/api/googleapi"

	"gitdr.io/gitdr/internal/dest"
)

// timedOut is a network error that says it timed out, as a dial or a TLS handshake does.
type timedOut struct{}

func (timedOut) Error() string   { return "i/o timeout" }
func (timedOut) Timeout() bool   { return true }
func (timedOut) Temporary() bool { return true }

// Each failure gets the one code gitdr.doctor/v1 names it with, wrapped the way the SDKs wrap it.
func TestErrorCodeNamesTheFailure(t *testing.T) {
	sdk := func(err error) error {
		return &smithy.OperationError{ServiceID: "S3", OperationName: "GetObjectLockConfiguration", Err: err}
	}
	get := func(err error) error { return &url.Error{Op: "Get", URL: "https://s3.example.com/b", Err: err} }
	dial := func(err error) error { return get(&net.OpError{Op: "dial", Net: "tcp", Err: err}) }

	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"an answer cut off at the cap", sdk(&smithy.DeserializationError{Err: fmt.Errorf("failed to decode response body, %w", dest.ErrResponseTooLarge)}), "too-large"},
		{"an error answer cut off at the cap", sdk(&smithy.DeserializationError{Err: fmt.Errorf("failed to copy error response body, %w", dest.ErrResponseTooLarge)}), "too-large"},
		{"an S3 error", sdk(&smithy.GenericAPIError{Code: "AccessDenied", Message: "Access Denied"}), "AccessDenied"},
		{"an S3 error whose code is not shaped like one", sdk(&smithy.GenericAPIError{Code: "<b>Access Denied</b>"}), "unnamed"},
		{"an S3 error with no code", sdk(&smithy.GenericAPIError{}), "unnamed"},
		{"an Azure error", fmt.Errorf("azure: container properties: %w", &azcore.ResponseError{ErrorCode: "AuthorizationFailure", StatusCode: 403}), "AuthorizationFailure"},
		{"an Azure error without a code", &azcore.ResponseError{StatusCode: 403}, "unnamed"},
		{"a Cloud Storage error", fmt.Errorf("gcs: bucket attrs: %w", &googleapi.Error{Code: 403, Errors: []googleapi.ErrorItem{{Reason: "forbidden"}}}), "forbidden"},
		{"a Cloud Storage 404 the library rewrote", fmt.Errorf("gcs: bucket attrs: %w", storage.ErrBucketNotExist), "notFound"},
		{"a deadline", sdk(fmt.Errorf("request canceled: %w", context.DeadlineExceeded)), "timeout"},
		{"a run stopped by a signal", sdk(context.Canceled), "timeout"},
		{"a dial that timed out", sdk(dial(timedOut{})), "timeout"},
		{"a host name that does not resolve", sdk(dial(&net.DNSError{Err: "no such host", Name: "s3.invalid", IsNotFound: true})), "dns"},
		{"a refused connection", sdk(dial(syscall.ECONNREFUSED)), "connect"},
		{"a reset connection", sdk(get(syscall.ECONNRESET)), "connect"},
		{"a connection closed before any answer", sdk(get(io.EOF)), "connect"},
		{"an answer cut short", sdk(&smithy.DeserializationError{Err: io.ErrUnexpectedEOF}), "connect"},
		{"a certificate from nobody gitdr trusts", sdk(get(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}})), "tls"},
		{"a certificate for another host", sdk(get(x509.HostnameError{Host: "s3.example.com"})), "tls"},
		{"a TLS alert from the endpoint", sdk(get(&net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")})), "tls"},
		{"HTTPS to a port that speaks HTTP", sdk(get(http.ErrSchemeMismatch)), "tls"},
		{"an endpoint that does not speak TLS", sdk(get(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"})), "tls"},
		{"a page that is not XML", sdk(&smithy.DeserializationError{Err: errors.New("failed to decode response body, XML syntax error on line 1")}), "not-s3"},
		{"a page the answer check refused", sdk(fmt.Errorf("%w: HTTP 200 and a document whose root is html", dest.ErrNotStorageAPI)), "not-s3"},
		// An empty page ends in EOF, and the refusal outranks it, or it would read as a dropped connection.
		{"an empty page the answer check refused", sdk(errors.Join(dest.ErrNotStorageAPI, io.EOF)), "not-s3"},
		// As the Azure backend marks a token its credential chain could not get, under the
		// policy's own wrapping. Ahead of anything in the SDK's error: nothing was sent.
		{"an Azure credential the chain found nowhere", fmt.Errorf("azure: container properties: %w",
			fmt.Errorf("%w: %w", dest.ErrNoCredentials, azidentity.NewCredentialUnavailableError("DefaultAzureCredential: failed to acquire a token"))), "no-credentials"},
		{"an Azure credential the identity provider refused", fmt.Errorf("azure: read container immutability policy: %w",
			fmt.Errorf("%w: %w", dest.ErrNoCredentials, &azidentity.AuthenticationFailedError{})), "no-credentials"},
		{"anything nobody named", errors.New("something else"), "not-s3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorCode(tc.err); got != tc.want {
				t.Errorf("errorCode(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// Every code doctor gives on its own has words beside it, and the words carry the code, so the
// detail and the code cannot say two different things.
func TestEveryCodeHasItsWords(t *testing.T) {
	for _, code := range []string{codeDNS, codeConnect, codeTLS, codeTimeout, codeTooLarge, codeNotS3, codeNoCredentials} {
		if storeAnswered(code) {
			t.Errorf("%s is counted as the store's own code", code)
		}
		if got := failure(code); got == "the store answered "+code || !strings.Contains(got, "("+code+")") {
			t.Errorf("failure(%q) = %q, which does not explain it", code, got)
		}
	}
	if !storeAnswered("AccessDenied") || failure("AccessDenied") != "the store answered AccessDenied" {
		t.Error("a store's code is not reported as the store's answer")
	}
}
