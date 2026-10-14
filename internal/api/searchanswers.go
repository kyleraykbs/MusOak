package api

import (
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// searchKey names one question: what kind of thing was asked for, the words,
// which providers were asked, and how many were wanted. Two asks with the same
// key are the same question and deserve the same answer.
func searchKey(kind, query string, providers []string, limit int) string {
	names := append([]string(nil), providers...)
	sort.Strings(names)
	return strings.Join([]string{kind, query, strings.Join(names, ","), strconv.Itoa(limit)}, "\x00")
}

// searchAnswers holds the last few answers to the same question, and makes sure
// a question is only put to the providers once at a time.
//
// Two things come out of that, and both are the point:
//
//   - speed. A search is a provider round trip - for the music services this
//     server talks to, a whole process and a page fetch - and a person typing
//     asks the same question several times over (every prefix, back and forth
//     over a letter, one tab after another). Within the window the answer is
//     already here.
//   - consistency. Providers order their hits by whatever they like, and the
//     list moves between two identical asks: the same query shows the songs in
//     a different order a second later. Held for the window, the answer to one
//     question is one answer.
//
// Sharing an in-flight ask matters as much as the cache: the tracks tab and the
// albums tab asking at once, or two people asking the same thing, cost one
// provider call rather than one each - and every one of those is a process.
// SearchAnswerTTL is how long an answer is kept. Long enough that typing the
// same words again - every prefix, back and forth over a letter, one tab after
// another - costs nothing and shows the same list; short enough that a re-asked
// question still reaches the providers.
const SearchAnswerTTL = 45 * time.Second

type searchAnswers struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	entries map[string]*searchEntry
}

type searchEntry struct {
	done  chan struct{}
	at    time.Time
	value any
	err   error
}

func newSearchAnswers(ttl time.Duration, max int) *searchAnswers {
	return &searchAnswers{ttl: ttl, max: max, entries: make(map[string]*searchEntry)}
}

// do runs fn for the key, unless its answer is already here or on its way.
func (c *searchAnswers) do(key string, fn func() (any, error)) (any, error) {
	for {
		c.mu.Lock()
		entry, seen := c.entries[key]
		if seen && entry.done == nil && time.Since(entry.at) < c.ttl {
			value, err := entry.value, entry.err
			c.mu.Unlock()
			return value, err
		}
		if seen && entry.done != nil {
			// Somebody is already asking. Wait for their answer rather than
			// spending a second provider call on the same question.
			done := entry.done
			c.mu.Unlock()
			<-done
			continue
		}
		entry = &searchEntry{done: make(chan struct{})}
		c.entries[key] = entry
		c.evictLocked()
		c.mu.Unlock()

		value, err := fn()

		// The channel is taken before the field is cleared: clearing it first
		// and then closing the field would close a nil channel.
		done := entry.done
		c.mu.Lock()
		if err != nil {
			// A failure is not an answer. Remembering one would turn a
			// provider's bad second into a broken search for the whole window.
			delete(c.entries, key)
		} else {
			entry.value, entry.err, entry.at, entry.done = value, nil, time.Now(), nil
		}
		c.mu.Unlock()
		close(done)
		return value, err
	}
}

// evictLocked drops the oldest answers once there are too many: a cache of
// searches is worth a bounded amount of memory and no more.
func (c *searchAnswers) evictLocked() {
	if len(c.entries) <= c.max {
		return
	}
	var oldestKey string
	var oldest time.Time
	for key, entry := range c.entries {
		if entry.done != nil {
			continue // in flight: not a candidate
		}
		if oldestKey == "" || entry.at.Before(oldest) {
			oldestKey, oldest = key, entry.at
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}