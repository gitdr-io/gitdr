package pipeline

import (
	"context"
	"testing"
	"time"
)

// The context a manifest is written on outlives a stop by its grace, and no longer, so a stopped
// run files its manifest and still ends.
func TestTheManifestOutlivesAStopByItsGraceAlone(t *testing.T) {
	ctx, stop := context.WithCancel(context.Background())
	final, done := manifestContext(ctx, 50*time.Millisecond)
	defer done()

	stop()
	if final.Err() != nil {
		t.Fatal("the stop cancelled the manifest's context at once")
	}
	select {
	case <-final.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the manifest's context outlived the stop by far more than its grace")
	}

	// Not stopped, it lasts until the run is done with it.
	unstopped, finish := manifestContext(context.Background(), time.Nanosecond)
	if unstopped.Err() != nil {
		t.Fatal("a run that was not stopped lost its manifest's context")
	}
	finish()
	if unstopped.Err() == nil {
		t.Error("the manifest's context outlived the run")
	}
}
