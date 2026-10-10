// Package rooms implements Listen Together around a simple contract: each
// member has a queue, the room mixes those queues, and the host's ordinary
// player drives the mixed order.
//
// The host reports its current queue item, position and pause state on `/sync`.
// The server timestamps that state and broadcasts it; it does not run its own
// song-end timer. Votes and explicit transport commands may still skip a song.
//
// Followers load a different song and seek the same song only when drift is
// greater than two seconds.
package rooms

import (
	"sync"
	"time"
)

// EventType identifies a room event.
type EventType string

// Room events. Clients follow the room, so every state change is announced.
const (
	EventMemberJoined EventType = "member_joined"
	EventMemberLeft   EventType = "member_left"
	EventHostChanged  EventType = "host_changed"
	EventQueueUpdated EventType = "queue_updated"
	EventPlayback     EventType = "playback"
	EventRoomClosed   EventType = "room_closed"
)

// Event is one room state change, broadcast to every subscriber.
//
// Seq is per-room and counts every event the room published. A client that sees
// a gap missed events (the bus drops them rather than stall the room) and
// refetches the snapshot, which is the resync path.
//
// The payloads are small on purpose, and the play order is never sent:
//
//   - queue_updated carries {memberId, memberQueue} — one member's queue, or
//     {memberId, gone: true} when they left. A client replaces that queue and
//     derives the play order itself (round-robin across members in join order).
//   - playback carries {current}, the whole current-state view or null when the
//     room is idle. When the current item changes or goes away, the client also
//     removes the previous item from its queue mirror and from its derived play
//     order — the server dropped it too, and does not resend the queue for it.
//     This is what keeps the derived order's round-robin phase identical to the
//     server's.
type Event struct {
	Type   EventType `json:"type"`
	RoomID string    `json:"roomId"`
	Seq    int64     `json:"seq"`
	AtMs   int64     `json:"atMs"`
	Data   any       `json:"data,omitempty"`
}

// subscriberBuffer is how many events a slow client may fall behind before
// events are dropped for it; it re-syncs from a snapshot when that happens.
const subscriberBuffer = 128

// Bus fans room events out to subscribers (WebSocket connections).
type Bus struct {
	mu     sync.RWMutex
	nextID int
	subs   map[int]chan Event
}

// NewBus returns an empty event bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[int]chan Event)}
}

// Subscribe returns a channel of events and a cancel function.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan Event, subscriberBuffer)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(ch)
		}
	}
}

// Publish delivers e to every subscriber without blocking: a subscriber that
// cannot keep up misses events rather than stalling the room.
func (b *Bus) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // full: this subscriber falls behind and resyncs from a snapshot
		}
	}
}

// Clock stamps room state and arms a room's own deadlines; it is replaceable in
// tests, which have to fire a two-hour idle close rather than wait for one.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, fn func()) Timer
}

// Timer is one scheduled callback.
type Timer interface {
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, fn func()) Timer { return time.AfterFunc(d, fn) }
