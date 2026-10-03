package cli

import (
	"strings"
	"testing"
	"time"
)

// The run deadline comes from --deadline, or from GITDR_DEADLINE when the flag is not given, as an
// RFC 3339 time. A value that is not one, and a deadline already past, are refused before the run
// starts rather than read as no deadline at all.
func TestRunDeadline(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, flag, env string
		want            time.Time
		wantErr         string
	}{
		{name: "neither"},
		{name: "the flag", flag: "2026-10-03T18:00:00Z", want: now.Add(6 * time.Hour)},
		{name: "the environment", env: "2026-10-03T18:00:00+03:00", want: now.Add(3 * time.Hour)},
		{name: "the flag over the environment", flag: "2026-10-03T13:00:00Z", env: "2026-10-03T18:00:00Z", want: now.Add(time.Hour)},
		{name: "not a time", flag: "in six hours", wantErr: "--deadline"},
		{name: "not a time, from the environment", env: "tomorrow", wantErr: "GITDR_DEADLINE"},
		{name: "already past", flag: "2026-10-03T11:59:59Z", wantErr: "already passed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runDeadline(tc.flag, tc.env, now)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("runDeadline = %v, %v; want an error naming %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("deadline = %s, want %s", got, tc.want)
			}
		})
	}
}
