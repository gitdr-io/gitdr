package dest_test

import (
	"testing"

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
