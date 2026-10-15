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

// add stays O(1): it stores without walking the registry, so a finished job is
// left for the next read to drop rather than swept on every add.
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
