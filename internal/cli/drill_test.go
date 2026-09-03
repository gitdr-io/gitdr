package cli

import (
	"errors"
	"fmt"
	"testing"

	"gitdr.io/gitdr/internal/pipeline"
)

// The exit code is the whole contract for `drill`.
//
// The agent that runs this in the control plane takes the exit code as the verdict and refuses
// to read the report beside it, deliberately: a report claiming success next to a non-zero exit
// is a broken contract, and guessing which half to believe is how a reader ends up trusting the
// convenient one. That refusal only works if the code itself carries the distinction.
func TestTheDrillExitCodeSeparatesTheTwoFailures(t *testing.T) {
	stored := fmt.Errorf("%w: %w", pipeline.ErrReportNotStored, errors.New("access denied"))

	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"a clean drill", nil, 0},
		{"a repository did not come back", pipeline.ErrDrillFailures, 1},
		{"everything restored, the report was not filed", stored, 3},
		// Both at once. The restore failure wins, because exit 3 is a promise that the restores
		// passed and it has to hold every time it is issued.
		{"both", errors.Join(pipeline.ErrDrillFailures, stored), 1},
		// Anything else is a failure nobody has classified, and an unclassified failure is not
		// allowed to borrow exit 3's promise.
		{"an unrecognised failure", errors.New("locate manifest: no such key"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := drillExit(tc.err); got != tc.want {
				t.Errorf("drillExit(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
