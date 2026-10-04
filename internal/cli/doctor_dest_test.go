package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	smithy "github.com/aws/smithy-go"

	"gitdr.io/gitdr/internal/dest"
)

// readOnlyDest answers doctor's reads from its fields, and fails the test the moment anything
// writes, walks the whole listing, or reads an object's contents.
type readOnlyDest struct {
	t *testing.T

	worm    dest.WormStatus
	wormErr error

	page    []dest.Object
	more    bool
	pageErr error
	pages   int

	held    dest.RetentionObservation
	until   time.Time
	heldErr error
}

func (d *readOnlyDest) VerifyWorm(context.Context) (dest.WormStatus, error) { return d.worm, d.wormErr }

func (d *readOnlyDest) PutImmutable(context.Context, string, io.Reader, int64, dest.Retention) (dest.PutResult, error) {
	d.t.Fatal("doctor called PutImmutable: it writes nothing to a bucket, no probe object and no canary")
	return dest.PutResult{}, nil
}

func (d *readOnlyDest) List(context.Context, string) ([]dest.Object, error) {
	d.t.Fatal("doctor called List, which walks every page of the listing; one page is all it reads")
	return nil, nil
}

func (d *readOnlyDest) Get(context.Context, string) (io.ReadCloser, error) {
	d.t.Fatal("doctor called Get: it reads an object's retention, never the object")
	return nil, nil
}

func (d *readOnlyDest) ListPage(_ context.Context, _ string, limit int) ([]dest.Object, bool, error) {
	d.pages++
	if limit != 1 {
		d.t.Errorf("ListPage asked for %d objects; doctor needs one", limit)
	}
	return d.page, d.more, d.pageErr
}

func (d *readOnlyDest) ObserveRetention(context.Context, string) (dest.RetentionObservation, time.Time, error) {
	return d.held, d.until, d.heldErr
}

func quietLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

var lockedStatus = dest.WormStatus{Verdict: dest.VerdictImmutable, Mode: "COMPLIANCE", Details: "Object Lock enabled; default retention COMPLIANCE"}

// A destination whose PutImmutable calls t.Fatal is never reached, whatever it answers, and
// neither is a walk of the listing. Doctor reads; anything else is a defect.
func TestDoctorNeverReachesAWrite(t *testing.T) {
	refused := &smithy.OperationError{ServiceID: "S3", OperationName: "GetObjectRetention",
		Err: &smithy.GenericAPIError{Code: "AccessDenied", Message: "Access Denied"}}
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	held := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	object := []dest.Object{{Key: "github.com/acme/api/2026-10-03/api.bundle"}}

	for _, tc := range []struct {
		name     string
		d        readOnlyDest
		observed string // "" when there is no retention check
		detail   string // a fragment of the retention check's detail
		// whether the retention check fails under worm.require; the worm check fails there unless
		// the bucket is locked
		absent bool
	}{
		{name: "locked, holding a retained object", d: readOnlyDest{worm: lockedStatus, page: object, held: dest.RetentionPresent, until: held},
			observed: "present", detail: "held until 2026-11-02T12:00:00Z"},
		{name: "locked, holding an object whose lock has ended", d: readOnlyDest{worm: lockedStatus, page: object, held: dest.RetentionPresent, until: time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)},
			observed: "lapsed", detail: "the lock on an object here ended on 2026-07-04T00:00:00Z"},
		{name: "locked, holding an object whose lock ends now", d: readOnlyDest{worm: lockedStatus, page: object, held: dest.RetentionPresent, until: now},
			observed: "lapsed", detail: "ended on 2026-10-03T12:00:00Z"},
		{name: "locked and empty", d: readOnlyDest{worm: lockedStatus},
			observed: "none", detail: "nothing written here yet"},
		{name: "locked, an empty first page that says there are more", d: readOnlyDest{worm: lockedStatus, more: true},
			observed: "none", detail: "first page of the listing named no object"},
		{name: "locked, an object with no retention", d: readOnlyDest{worm: lockedStatus, page: object, held: dest.RetentionAbsent},
			observed: "absent", detail: "carries no retention", absent: true},
		{name: "locked, a retention the store will not show", d: readOnlyDest{worm: lockedStatus, page: object, held: dest.RetentionNotChecked, heldErr: refused},
			observed: "unreadable", detail: "the store answered AccessDenied; on S3 this needs s3:GetObjectRetention"},
		{name: "locked, a retention cut off at the cap", d: readOnlyDest{worm: lockedStatus, page: object, held: dest.RetentionNotChecked, heldErr: dest.ErrResponseTooLarge},
			observed: "unreadable", detail: "(too-large)"},
		{name: "locked, a listing that fails", d: readOnlyDest{worm: lockedStatus, pageErr: refused},
			observed: "unreadable", detail: "could not list the destination to find an object to check: the store answered AccessDenied"},
		{name: "not locked", d: readOnlyDest{worm: dest.WormStatus{Verdict: dest.VerdictNotImmutable, Details: "Object Lock not enabled"}, page: object}},
		{name: "unknown", d: readOnlyDest{worm: dest.WormStatus{Verdict: dest.VerdictUnknown, Details: "could not verify immutability: the bucket answered AccessDenied", Refusal: refused}, page: object}},
		{name: "the lock question fails", d: readOnlyDest{wormErr: fmt.Errorf("s3: get object lock config: %w", context.DeadlineExceeded), page: object}},
	} {
		for _, require := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, worm.require %v", tc.name, require), func(t *testing.T) {
				d := tc.d
				d.t = t
				checks := checkDestination(context.Background(), &d, require, now, quietLog())

				if d.pages > 1 {
					t.Errorf("%d pages listed; doctor reads one", d.pages)
				}
				if checks[0].Name != "worm" {
					t.Fatalf("first check is %q, want worm", checks[0].Name)
				}
				if want := d.wormErr == nil && d.worm.Verdict.Immutable() || !require; checks[0].OK != want {
					t.Errorf("worm ok = %v, want %v", checks[0].OK, want)
				}
				if tc.observed == "" {
					if len(checks) != 1 {
						t.Errorf("a retention check on a bucket that does not say it locks: %+v", checks[1:])
					}
					return
				}
				if len(checks) != 2 || checks[1].Name != "retention" {
					t.Fatalf("checks = %+v, want worm then retention", checks)
				}
				ret := checks[1]
				if ret.Observed != tc.observed || !strings.Contains(ret.Detail, tc.detail) {
					t.Errorf("retention = %q, %q; want %q, saying %q", ret.Observed, ret.Detail, tc.observed, tc.detail)
				}
				if want := !tc.absent || !require; ret.OK != want {
					t.Errorf("retention ok = %v, want %v", ret.OK, want)
				}
			})
		}
	}
}

// The worm check's data: the verdict word, a mode gitdr knows, and the code of a refusal.
func TestDoctorWormAnswer(t *testing.T) {
	refused := &smithy.GenericAPIError{Code: "SignatureDoesNotMatch"}
	for _, tc := range []struct {
		name                string
		worm                dest.WormStatus
		err                 error
		verdict, mode, code string // "null" for nil
	}{
		{"locked", lockedStatus, nil, "immutable", "COMPLIANCE", "null"},
		{"locked, a mode the store made up", dest.WormStatus{Verdict: dest.VerdictImmutable, Mode: "Forever"}, nil, "immutable", "null", "null"},
		{"a Cloud Storage policy", dest.WormStatus{Verdict: dest.VerdictImmutable, Mode: "RETENTION"}, nil, "immutable", "RETENTION", "null"},
		{"an unlocked Azure policy", dest.WormStatus{Verdict: dest.VerdictNotImmutable, Mode: "IMMUTABILITY"}, nil, "not-immutable", "IMMUTABILITY", "null"},
		{"refused", dest.WormStatus{Verdict: dest.VerdictUnknown, Refusal: refused}, nil, "unknown", "null", "SignatureDoesNotMatch"},
		{"unknown without a refusal", dest.WormStatus{Verdict: dest.VerdictUnknown}, nil, "unknown", "null", "null"},
		{"no answer", dest.WormStatus{}, dest.ErrResponseTooLarge, "null", "null", "too-large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &readOnlyDest{t: t, worm: tc.worm, wormErr: tc.err}
			c, _ := checkWorm(context.Background(), d, false, quietLog())
			if c.WormAnswer == nil {
				t.Fatal("the worm check carries no answer")
			}
			if text(c.Verdict) != tc.verdict || text(c.Mode) != tc.mode || text(c.Code) != tc.code {
				t.Errorf("verdict %s, mode %s, code %s; want %s, %s, %s",
					text(c.Verdict), text(c.Mode), text(c.Code), tc.verdict, tc.mode, tc.code)
			}
		})
	}
}

// `doctor` looks at an object that is already there, and says nothing when there is not one.
//
// The alternative is writing a canary, and on a compliance-locked bucket that is undeletable
// litter for the whole retention window — by construction, since the destination has no delete
// path. A diagnostic that leaves rubbish behind is one people stop running.
func TestDoctorLooksAtAnObjectRatherThanWritingOne(t *testing.T) {
	ctx := context.Background()

	t.Run("an empty destination has nothing to check", func(t *testing.T) {
		key, more, err := anyObject(ctx, &readOnlyDest{t: t})
		if err != nil {
			t.Fatal(err)
		}
		if key != "" || more {
			t.Errorf("key = %q, more = %v on an empty destination, and nothing was written to make one", key, more)
		}
	})

	t.Run("any object will do", func(t *testing.T) {
		key, _, err := anyObject(ctx, &readOnlyDest{t: t, page: []dest.Object{{Key: "a"}}, more: true})
		if err != nil {
			t.Fatal(err)
		}
		if key != "a" {
			t.Errorf("key = %q from a page that names a", key)
		}
	})

	t.Run("a destination that will not list says so", func(t *testing.T) {
		// Rather than reading an empty list as an empty bucket, which would report "nothing
		// written here yet" about a bucket full of backups the credential cannot see.
		if _, _, err := anyObject(ctx, &readOnlyDest{t: t, pageErr: errors.New("access denied")}); err == nil {
			t.Error("a list failure was reported as an empty destination")
		}
	})
}
