package dest_test

import (
	"testing"

	"gitdr.io/gitdr/internal/dest/azure"
	"gitdr.io/gitdr/internal/dest/gcs"
	"gitdr.io/gitdr/internal/dest/s3"

	"gitdr.io/gitdr/internal/dest"
)

func TestForgetfulBackendClaimsNothing(t *testing.T) {
	var st dest.WormStatus
	if st.Verdict != dest.VerdictUnknown {
		t.Errorf("the zero value is %q, so a backend that sets nothing makes a claim", st.Verdict)
	}
	if st.Verdict.Immutable() {
		t.Error("the zero value reports immutable, which would send retention to an unverified bucket")
	}
	if st.Verdict.Wire() != "unknown" {
		t.Errorf("the zero value serialises as %q; an omitted field means an engine too old to say", st.Verdict.Wire())
	}
}

// Every backend in this tree can be asked what retention actually landed.
//
// `RetentionObserver` is optional so `Destination` stays at four methods and a backend that
// genuinely cannot answer declines honestly. Optional is not the same as forgettable: a backend
// that silently lacks it records `not-checked` for every run, which is a check quietly doing
// nothing rather than a check failing. This is what makes "forgot" a CI failure instead of a
// customer's discovery, and it is why the assertion is on the concrete types rather than on
// whatever a factory happens to return.
func TestEveryBackendCanBeAskedWhatRetentionLanded(t *testing.T) {
	for _, tc := range []struct {
		name string
		b    any
	}{
		{"s3", (*s3.Backend)(nil)},
		{"gcs", (*gcs.Backend)(nil)},
		{"azure", (*azure.Backend)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := tc.b.(dest.RetentionObserver); !ok {
				t.Errorf("%s does not implement RetentionObserver, so every run it writes records "+
					"retention as not-checked and the manifest keeps a claim nobody confirmed", tc.name)
			}
		})
	}
}
