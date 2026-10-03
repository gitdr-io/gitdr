package s3_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	smithy "github.com/aws/smithy-go"

	"gitdr.io/gitdr/internal/dest"
)

// byCall answers the lock question, a listing and a retention read each with its own handler.
func byCall(lock, list, retention http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch {
		case q.Has("object-lock") && lock != nil:
			lock(w, r)
		case q.Get("list-type") == "2" && list != nil:
			list(w, r)
		case q.Has("retention") && retention != nil:
			retention(w, r)
		default:
			w.WriteHeader(http.StatusTeapot)
		}
	}
}

// signInPage is what a proxy, a captive portal or a misconfigured endpoint answers with. It is
// well-formed enough that an XML parser reads it without complaint.
const signInPage = `<!DOCTYPE html><html><head><title>Sign in</title></head><body><h1>Sign in to continue</h1></body></html>`

func htmlAnswer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

const (
	noLockConfiguration = `<Error><Code>ObjectLockConfigurationNotFoundError</Code>` +
		`<Message>Object Lock configuration does not exist for this bucket</Message></Error>`
	noSuchBucket = `<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist</Message></Error>`
	emptyListing = `<ListBucketResult><Name>b</Name><KeyCount>0</KeyCount><IsTruncated>false</IsTruncated></ListBucketResult>`
)

// A bucket that says it has no Object Lock configuration is believed once it is known to exist.
//
// MinIO gives that answer about a bucket that does not exist, so gitdr reported a bucket nobody
// had created as "not immutable", which is an earned negative it had not earned. One listing
// tells the two apart, and its answer is the store's own: NoSuchBucket for a bucket that is not
// there.
func TestVerifyWormAsksWhetherTheBucketExists(t *testing.T) {
	for _, tc := range []struct {
		name    string
		list    http.HandlerFunc
		verdict dest.WormVerdict
		code    string // the code of the refusal behind an unknown verdict, "" for none
		details string
	}{
		{"the bucket is there", xmlAnswer(http.StatusOK, emptyListing),
			dest.VerdictNotImmutable, "", "bucket has no Object Lock configuration"},
		{"the bucket is not there", xmlAnswer(http.StatusNotFound, noSuchBucket),
			dest.VerdictUnknown, "NoSuchBucket", "could not verify immutability: the bucket answered NoSuchBucket"},
		{"the key may not list it", xmlAnswer(http.StatusForbidden, `<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`),
			dest.VerdictUnknown, "AccessDenied", "could not verify immutability: the bucket answered AccessDenied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newReaderStore(t, byCall(xmlAnswer(http.StatusNotFound, noLockConfiguration), tc.list, nil))
			st, err := newReaderBackend(t, store.URL).VerifyWorm(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if st.Verdict != tc.verdict || st.Details != tc.details {
				t.Errorf("VerifyWorm = %q, %q; want %q, %q", st.Verdict.Wire(), st.Details, tc.verdict.Wire(), tc.details)
			}
			var api smithy.APIError
			switch {
			case tc.code == "" && st.Refusal != nil:
				t.Errorf("Refusal = %v behind an earned negative", st.Refusal)
			case tc.code != "" && (!errors.As(st.Refusal, &api) || api.ErrorCode() != tc.code):
				t.Errorf("Refusal = %v, want the store's %s", st.Refusal, tc.code)
			}
		})
	}

	t.Run("a locked bucket is not listed", func(t *testing.T) {
		locked := xmlAnswer(http.StatusOK, `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`)
		store := newReaderStore(t, byCall(locked, xmlAnswer(http.StatusOK, emptyListing), nil))
		st, err := newReaderBackend(t, store.URL).VerifyWorm(context.Background())
		if err != nil || !st.Verdict.Immutable() {
			t.Fatalf("VerifyWorm = %+v, %v", st, err)
		}
		for _, q := range store.seen() {
			if strings.Contains(q, "list-type") {
				t.Errorf("a locked bucket was listed: %s", q)
			}
		}
	})
}

// An answer that is not the storage API's is no answer, and never the earned negative. Handed a
// web page where the lock configuration belongs, the SDK read it as a configuration with nothing
// in it, and gitdr said "Object Lock not enabled". A 404 page became the code NotFound, which the
// SDK makes up from the status when no document names one.
func TestVerifyWormTakesNoPageForAnAnswer(t *testing.T) {
	for _, tc := range []struct {
		name string
		lock http.HandlerFunc
	}{
		{"an HTML page with a 200", htmlAnswer(http.StatusOK, signInPage)},
		{"an HTML page with a 404", htmlAnswer(http.StatusNotFound, signInPage)},
		{"an empty 200", xmlAnswer(http.StatusOK, "")},
		{"another call's document", xmlAnswer(http.StatusOK, `<ListBucketResult><Name>b</Name></ListBucketResult>`)},
		{"a 404 with no document", xmlAnswer(http.StatusNotFound, "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newReaderStore(t, byCall(tc.lock, xmlAnswer(http.StatusOK, emptyListing), nil))
			st, err := newReaderBackend(t, store.URL).VerifyWorm(context.Background())
			if err == nil {
				t.Errorf("VerifyWorm = %q (%s) from an answer that is not S3; want an error", st.Verdict.Wire(), st.Details)
			}
		})
	}

	// The shapes S3 answers in still decide, the negative included.
	for _, tc := range []struct {
		name    string
		lock    http.HandlerFunc
		verdict dest.WormVerdict
	}{
		{"a configuration with Object Lock off", xmlAnswer(http.StatusOK, `<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></ObjectLockConfiguration>`), dest.VerdictNotImmutable},
		{"a configuration after an XML declaration", xmlAnswer(http.StatusOK, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+
			`<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`), dest.VerdictImmutable},
		{"an S3 refusal", xmlAnswer(http.StatusNotImplemented, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>NotImplemented</Code></Error>`), dest.VerdictUnknown},
		// S3 can send an error with a 200, and the SDK reads an Error document as one whatever the
		// status (s3 internal/customizations/handle_200_error.go). A refusal, then, not a page.
		{"an S3 error document with a 200", xmlAnswer(http.StatusOK, `<Error><Code>InternalError</Code></Error>`), dest.VerdictUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newReaderStore(t, byCall(tc.lock, xmlAnswer(http.StatusOK, emptyListing), nil))
			st, err := newReaderBackend(t, store.URL).VerifyWorm(context.Background())
			if err != nil || st.Verdict != tc.verdict {
				t.Errorf("VerifyWorm = %q, %v; want %q", st.Verdict.Wire(), err, tc.verdict.Wire())
			}
		})
	}
}

// The retention read and the listing give the same refusal to a page. A retention read is how a
// backup confirms its first object is held, and absent there lowers the manifest's verdict.
func TestRetentionAndListingTakeNoPageForAnAnswer(t *testing.T) {
	t.Run("a retention", func(t *testing.T) {
		store := newReaderStore(t, byCall(nil, nil, htmlAnswer(http.StatusOK, signInPage)))
		got, _, err := newReaderBackend(t, store.URL).ObserveRetention(context.Background(), "k")
		if got != dest.RetentionNotChecked || err == nil {
			t.Errorf("ObserveRetention = %q, %v from a page; want not-checked and an error", got, err)
		}
	})
	t.Run("a retention S3 says is not there", func(t *testing.T) {
		store := newReaderStore(t, byCall(nil, nil, xmlAnswer(http.StatusNotFound,
			`<Error><Code>NoSuchObjectLockConfiguration</Code></Error>`)))
		if got, _, err := newReaderBackend(t, store.URL).ObserveRetention(context.Background(), "k"); got != dest.RetentionAbsent || err != nil {
			t.Errorf("ObserveRetention = %q, %v; want absent, the earned negative", got, err)
		}
	})
	t.Run("a listing", func(t *testing.T) {
		store := newReaderStore(t, byCall(nil, htmlAnswer(http.StatusOK, signInPage), nil))
		objs, more, err := newReaderBackend(t, store.URL).ListPage(context.Background(), "", 1)
		if err == nil {
			t.Errorf("ListPage = %+v, %v from a page; want an error", objs, more)
		}
	})
}
