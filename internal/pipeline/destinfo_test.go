package pipeline

import (
	"testing"
	"time"

	"gitdr.io/gitdr/internal/config"
	"gitdr.io/gitdr/internal/dest"
)

// The destination block names the bucket or container of every store config accepts, and a mode
// only where gitdr set one, which is S3. A store added to config and forgotten here would sign an
// empty bucket again, so the table is every type Validate takes.
func TestTheDestinationBlockNamesWhatEachStoreWroteTo(t *testing.T) {
	ret := dest.Retention{Mode: dest.RetentionCompliance, Until: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	for _, tc := range []struct {
		typ, bucket, mode string
	}{
		{typ: "s3", bucket: "s3-bucket", mode: "COMPLIANCE"},
		{typ: "gcs", bucket: "gcs-bucket"},
		{typ: "azure", bucket: "azure-container"},
	} {
		c := config.Default()
		c.Destination.Type = tc.typ
		c.Destination.S3.Bucket = "s3-bucket"
		c.Destination.GCS.Bucket = "gcs-bucket"
		c.Destination.Azure.Container = "azure-container"
		if err := c.Validate(); err != nil {
			t.Fatalf("%s: %v", tc.typ, err)
		}
		r := &backupRun{cfg: c, wormStatus: dest.WormStatus{Verdict: dest.VerdictImmutable, Details: "locked"}}
		d := r.destInfo(ret, dest.VerdictImmutable, dest.RetentionPresent)
		if d.Type != tc.typ || d.Bucket != tc.bucket || d.WormMode != tc.mode {
			t.Errorf("%s: type %q, bucket %q, wormMode %q; want %q, %q, %q", tc.typ, d.Type, d.Bucket, d.WormMode, tc.typ, tc.bucket, tc.mode)
		}
		if !d.WormImmutable || d.WormVerdict != "immutable" || d.WormDetails != "locked" || d.RetentionObserved != "present" {
			t.Errorf("%s: the verdict did not carry through: %+v", tc.typ, d)
		}
	}
}
