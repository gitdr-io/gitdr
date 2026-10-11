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

// A bucket that does not exist is marked so, whichever call says it: the listing behind MinIO's
// "no lock configuration", or the lock question itself, which AWS answers with NoSuchBucket. The
// mark is dest.ErrNoSuchBucket, with the store's error inside it for its code. A refusal that
// says anything else carries no mark, so the run warns about it and goes on as before.
func TestVerifyWormMarksABucketThatDoesNotExist(t *testing.T) {
	refused := xmlAnswer(http.StatusForbidden, `<Error><Code>AccessDenied</Code><Message>Access Denied</Message></Error>`)
	for _, tc := range []struct {
		name       string
		lock, list http.HandlerFunc
		missing    bool
		code       string
	}{
		{"no lock configuration, and the listing finds no bucket", xmlAnswer(http.StatusNotFound, noLockConfiguration),
			xmlAnswer(http.StatusNotFound, noSuchBucket), true, "NoSuchBucket"},
		{"the lock question finds no bucket", xmlAnswer(http.StatusNotFound, noSuchBucket), nil, true, "NoSuchBucket"},
		{"no lock configuration, and the key may not list", xmlAnswer(http.StatusNotFound, noLockConfiguration), refused, false, "AccessDenied"},
		{"the lock question refused", refused, nil, false, "AccessDenied"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newReaderStore(t, byCall(tc.lock, tc.list, nil))
			st, err := newReaderBackend(t, store.URL).VerifyWorm(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if st.Verdict != dest.VerdictUnknown {
				t.Errorf("verdict %q (%s), want unknown", st.Verdict.Wire(), st.Details)
			}
			if got := errors.Is(st.Refusal, dest.ErrNoSuchBucket); got != tc.missing {
				t.Errorf("Refusal %v: marked as a missing bucket = %v, want %v", st.Refusal, got, tc.missing)
			}
			var api smithy.APIError
			if !errors.As(st.Refusal, &api) || api.ErrorCode() != tc.code {
				t.Errorf("Refusal = %v, want the store's %s inside it", st.Refusal, tc.code)
			}
		})
	}
}

// What Google Cloud Storage answers the lock question through its S3 interoperability endpoint,
// storage.googleapis.com with an HMAC key, as two real buckets answered it on 2026-10-11. The
// code, the message and the configuration are Google's. The refusal's status is the 404 Google
// documents for a bucket without object retention.
const (
	// A bucket created with Object Retention Lock.
	googleRetentionLock = `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`
	// A bucket under a Bucket Lock retention policy, unlocked and of 120 s, without Object
	// Retention Lock.
	googleNoRetentionLock = `<Error><Code>ObjectLockConfigurationNotFound</Code>` +
		`<Message>Object Lock configuration does not exist.</Message></Error>`
)

// Through S3, Google shows Object Retention Lock and cannot show Bucket Lock, so its no is not an
// earned negative.
//
// Google's ?object-lock reports Object Retention Lock and nothing else, and Google documents that
// the XML API cannot view a bucket's retention policy, which is what Bucket Lock is. A bucket under
// a Bucket Lock policy and without Object Retention Lock answered ObjectLockConfigurationNotFound.
// That says one feature is off and nothing about the policy that may hold the bucket, so it reads
// unknown with Google's code in Details, and never not-immutable, even where a listing shows the
// bucket is there. It gets there because the code is not AWS's: AWS's no ends in Error, and it is
// believed of a bucket that lists (TestVerifyWormAsksWhetherTheBucketExists). --require-worm fails
// on unknown, and the native gcs destination reads the retention policy instead.
//
// A bucket with Object Retention Lock answered Enabled and no default rule. That immutable is
// earned: Google held the COMPLIANCE retention sent with an object, as Locked, and read it back. It
// has no mode, because Mode is what the bucket said, and it named none.
func TestVerifyWormCannotSeeGoogleBucketLockSoItsNoIsNotEarned(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lock    http.HandlerFunc
		verdict dest.WormVerdict
		details string
		code    string // the code of the refusal behind an unknown verdict, "" for none
	}{
		{"Object Retention Lock, which S3 can see", xmlAnswer(http.StatusOK, googleRetentionLock),
			dest.VerdictImmutable, "Object Lock enabled", ""},
		{"Bucket Lock only, which S3 cannot see", xmlAnswer(http.StatusNotFound, googleNoRetentionLock),
			dest.VerdictUnknown, "could not verify immutability: the bucket answered ObjectLockConfigurationNotFound", "ObjectLockConfigurationNotFound"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The listing answers as a bucket that is there, the condition under which AWS's no is
			// believed. Google's has to stay unknown on its own terms.
			store := newReaderStore(t, byCall(tc.lock, xmlAnswer(http.StatusOK, emptyListing), nil))
			st, err := newReaderBackend(t, store.URL).VerifyWorm(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if st.Verdict == dest.VerdictNotImmutable {
				t.Errorf("verdict not-immutable (%s): nothing Google answers through S3 earns that, since the S3 API cannot see Bucket Lock", st.Details)
			}
			if st.Verdict != tc.verdict || st.Details != tc.details || st.Mode != "" {
				t.Errorf("VerifyWorm = %q, %q, mode %q; want %q, %q, no mode", st.Verdict.Wire(), st.Details, st.Mode, tc.verdict.Wire(), tc.details)
			}
			var api smithy.APIError
			switch {
			case tc.code == "" && st.Refusal != nil:
				t.Errorf("Refusal = %v on an answer", st.Refusal)
			case tc.code != "" && (!errors.As(st.Refusal, &api) || api.ErrorCode() != tc.code):
				t.Errorf("Refusal = %v, want Google's %s", st.Refusal, tc.code)
			}
			if errors.Is(st.Refusal, dest.ErrNoSuchBucket) {
				t.Errorf("Refusal %v is marked as a missing bucket", st.Refusal)
			}
		})
	}
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
