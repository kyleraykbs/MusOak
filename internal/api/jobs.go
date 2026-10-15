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

type jobRegistry[T any] struct {
	mu      sync.Mutex
	entries map[string]*T
	ttl     time.Duration
	// endedAt is when a job stopped, or the zero time while it is still going.
	endedAt func(*T) time.Time
}

func newJobRegistry[T any](ttl time.Duration, endedAt func(*T) time.Time) *jobRegistry[T] {
	return &jobRegistry[T]{entries: map[string]*T{}, ttl: ttl, endedAt: endedAt}
}

// add stores a job and returns the id it answers to. It stays O(1): a finished
// job is dropped when it is looked up rather than on every add, so adding one
// never walks the registry.
func (r *jobRegistry[T]) add(job *T) string {
	id := uuid.NewString()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[id] = job
	return id
}

// get reads a job by id, first dropping whatever has been finished for longer
// than the registry remembers. Pruning here is what keeps a finished job - and
// its payload, which may be a whole zip - from lingering until the next add.
// The pointer it returns is the job itself, which its own lock guards.
func (r *jobRegistry[T]) get(id string) (*T, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(time.Now())
	job, ok := r.entries[id]
	return job, ok
}

// pruneLocked drops every job that has been finished for longer than the ttl.
func (r *jobRegistry[T]) pruneLocked(now time.Time) {
	for key, existing := range r.entries {
		if ended := r.endedAt(existing); !ended.IsZero() && now.Sub(ended) > r.ttl {
			delete(r.entries, key)
		}
	}
}
