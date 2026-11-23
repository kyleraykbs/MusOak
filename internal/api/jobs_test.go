package api

import (
	"testing"
	"time"
)

// testJob stands in for an import or an archive: the registry only needs to
// know when it ended.
type testJob struct {
	ended time.Time
}

// A finished job and its payload are dropped when the registry is read, so a
// job that is over does not linger until the next one is added.
func TestJobRegistryPrunesOnGet(t *testing.T) {
	reg := newJobRegistry[testJob](time.Minute, func(job *testJob) time.Time { return job.ended })

	done := reg.add(&testJob{ended: time.Now().Add(-2 * time.Minute)})
	if _, ok := reg.get(done); ok {
		t.Fatal("a job finished past its ttl was still there")
	}
	reg.mu.Lock()
	left := len(reg.entries)
	reg.mu.Unlock()
	if left != 0 {
		t.Fatalf("registry kept %d entries, want 0", left)
	}

	// A job still running is never pruned, however the clock reads.
	running := reg.add(&testJob{})
	if _, ok := reg.get(running); !ok {
		t.Fatal("a running job was pruned")
	}
}

// add does not walk the registry each time: within a minute of the last sweep
// it only stores, and a finished job is left for the next read to drop.
func TestJobRegistryAddDoesNotSweep(t *testing.T) {
	reg := newJobRegistry[testJob](time.Minute, func(job *testJob) time.Time { return job.ended })
	past := time.Now().Add(-2 * time.Minute)
	for range 50 {
		reg.add(&testJob{ended: past})
	}
	reg.mu.Lock()
	grown := len(reg.entries)
	reg.mu.Unlock()
	if grown != 50 {
		t.Fatalf("add swept the registry: %d entries, want 50", grown)
	}
}

// A job that never ends is the one nothing used to forget: an archive asked for
// and never downloaded. The registry's own stamp of when it arrived bounds it,
// and a registry that is only ever added to still sweeps.
func TestJobRegistryForgetsAJobThatNeverEnded(t *testing.T) {
	reg := newJobRegistry[testJob](time.Minute, func(job *testJob) time.Time { return job.ended })
	now := time.Now()
	reg.now = func() time.Time { return now }

	abandoned := reg.add(&testJob{})
	now = now.Add(jobMaxAge - time.Minute)
	if _, ok := reg.get(abandoned); !ok {
		t.Fatal("a job inside its lifetime was dropped")
	}

	// Past the lifetime, with nobody ever looking it up again: the next add
	// is what finds it.
	now = now.Add(2 * time.Minute)
	fresh := reg.add(&testJob{})
	reg.mu.Lock()
	_, kept := reg.entries[abandoned]
	_, added := reg.entries[fresh]
	stamps := len(reg.created)
	reg.mu.Unlock()
	if kept {
		t.Error("a job that never ended was remembered past jobMaxAge")
	}
	if !added || stamps != 1 {
		t.Errorf("after the sweep: fresh job kept = %v, %d creation stamps, want true and 1", added, stamps)
	}
}
