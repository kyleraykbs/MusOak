package api

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// The registry for work that outlives the request that started it.
//
// Both long jobs here - importing a playlist, and writing one out as a zip -
// answer the request with an id and carry on, and both need the same two
// things: somewhere to find a job by id, and a way for a job that is over to
// stop being remembered. A job says when it ended; the registry drops it once
// that is further back than the ttl it was made with.
//
// A job that never says it ended is the other way one is remembered for ever:
// an archive that was asked for and never downloaded has nothing to end, and
// holds its playlist until the process exits. So the registry keeps its own
// stamp of when each job arrived, and nothing outlives jobMaxAge whatever it
// says about itself.

// jobMaxAge is the longest a job is remembered without having ended. Longer
// than any import may run (importTimeout) and than any archive a browser is
// still downloading; a job older than this is one nobody is coming back for.
const jobMaxAge = 6 * time.Hour

// jobPruneEvery is how often adding a job also looks for ones to drop. Lookups
// prune every time; this is for the registry that is only ever added to.
const jobPruneEvery = time.Minute

type jobRegistry[T any] struct {
	mu      sync.Mutex
	entries map[string]*T
	// created is when each job was added, by id.
	created map[string]time.Time
	ttl     time.Duration
	// endedAt is when a job stopped, or the zero time while it is still going.
	endedAt func(*T) time.Time
	// now is the clock, so a test can run six hours in a line.
	now      func() time.Time
	prunedAt time.Time
}

func newJobRegistry[T any](ttl time.Duration, endedAt func(*T) time.Time) *jobRegistry[T] {
	return &jobRegistry[T]{
		entries: map[string]*T{}, created: map[string]time.Time{},
		ttl: ttl, endedAt: endedAt, now: time.Now,
	}
}

// add stores a job and returns the id it answers to. It walks the registry at
// most once a minute: a finished job is otherwise dropped when one is looked
// up, but a registry nobody looks anything up in - a client that starts
// archives and never fetches them - would only ever grow.
func (r *jobRegistry[T]) add(job *T) string {
	id := uuid.NewString()
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if now.Sub(r.prunedAt) >= jobPruneEvery {
		r.pruneLocked(now)
	}
	r.entries[id] = job
	r.created[id] = now
	return id
}

// get reads a job by id, first dropping whatever has been finished for longer
// than the registry remembers. Pruning here is what keeps a finished job - and
// its payload, which may be a whole zip - from lingering until the next add.
// The pointer it returns is the job itself, which its own lock guards.
func (r *jobRegistry[T]) get(id string) (*T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(r.now())
	job, ok := r.entries[id]
	return job, ok
}

// pruneLocked drops every job that has been finished for longer than the ttl,
// and every job that has been here longer than any job is given.
func (r *jobRegistry[T]) pruneLocked(now time.Time) {
	r.prunedAt = now
	for key, existing := range r.entries {
		ended := r.endedAt(existing)
		finished := !ended.IsZero() && now.Sub(ended) > r.ttl
		if finished || now.Sub(r.created[key]) > jobMaxAge {
			delete(r.entries, key)
			delete(r.created, key)
		}
	}
}
