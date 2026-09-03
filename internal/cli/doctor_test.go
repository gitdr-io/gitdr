package cli

import (
	"context"
	"errors"
	"io"
	"testing"

	"gitdr.io/gitdr/internal/dest"
)

// A destination that only has to answer List for these tests.
type listOnly struct {
	objs []dest.Object
	err  error
}

func (l listOnly) VerifyWorm(context.Context) (dest.WormStatus, error) { return dest.WormStatus{}, nil }
func (l listOnly) PutImmutable(context.Context, string, io.Reader, int64, dest.Retention) (dest.PutResult, error) {
	return dest.PutResult{}, errors.New("not used")
}
func (l listOnly) List(context.Context, string) ([]dest.Object, error) { return l.objs, l.err }
func (l listOnly) Get(context.Context, string) (io.ReadCloser, error) {
	return nil, errors.New("not used")
}

// `doctor` looks at an object that is already there, and says nothing when there is not one.
//
// The alternative is writing a canary, and on a compliance-locked bucket that is undeletable
// litter for the whole retention window — by construction, since the destination has no delete
// path. A diagnostic that leaves rubbish behind is one people stop running.
func TestDoctorLooksAtAnObjectRatherThanWritingOne(t *testing.T) {
	ctx := context.Background()

	t.Run("an empty destination has nothing to check", func(t *testing.T) {
		key, err := anyObject(ctx, listOnly{})
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			t.Errorf("key = %q on an empty destination, and nothing was written to make one", key)
		}
	})

	t.Run("any object will do", func(t *testing.T) {
		key, err := anyObject(ctx, listOnly{objs: []dest.Object{{Key: "a"}, {Key: "b"}}})
		if err != nil {
			t.Fatal(err)
		}
		if key == "" {
			t.Error("no object chosen from a destination that has two")
		}
	})

	t.Run("a destination that will not list says so", func(t *testing.T) {
		// Rather than reading an empty list as an empty bucket, which would report "nothing
		// written here yet" about a bucket full of backups the credential cannot see.
		if _, err := anyObject(ctx, listOnly{err: errors.New("access denied")}); err == nil {
			t.Error("a list failure was reported as an empty destination")
		}
	})
}
