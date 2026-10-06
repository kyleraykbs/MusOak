// Package rooms implements Listen Together in the simplest shape that works:
// the host's player is the clock, and every other client follows it.
//
// The room owns one queue per member (mixed round-robin into a play order) and
// one current track. The current track starts when the host's player says it
// started, advances when the host's file runs out — or a skip or a vote says
// so — and moves under pause/seek as commands against the same anchor. Clients
// keep time with arithmetic: while running, position = anchor + (now - at).
//
// A follower does exactly two things: if the room's track is not the track it
// is playing, it loads that track; if it is, it stays within two seconds of
// where the room says it should be, seeking when it is not. The host runs the
// same loop and adds two reports — /started when its player begins a track,
// /ended when its file runs out — because the host is what the room follows.
package rooms

import (
	"sync"
	"time"
)

// EventType identifies a room event.
type EventType string

// Room events. Clients follow the server, so every state change is announced.
// There are six: membership, who leads, one member's queue, the whole
// playback state (votes included), and the room going away.
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

// Clock abstracts time: it stamps state, and it is what arms the moment a song
// ends so a test can hold the room's own clock still and move it by hand.
type Clock interface {
	Now() time.Time
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is the subset of time.Timer rooms use.
type Timer interface {
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }
