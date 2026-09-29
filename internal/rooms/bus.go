package rooms

import (
	"sync"
	"time"
)

// EventType identifies a room event.
type EventType string

// Room events. Clients follow the server, so every state change is announced.
const (
	EventMemberJoined  EventType = "member_joined"
	EventMemberLeft    EventType = "member_left"
	EventQueueUpdated  EventType = "queue_updated"
	EventTrackPrepared EventType = "track_prepared"
	EventTrackStarted  EventType = "track_started"
	EventTrackSkipped  EventType = "track_skipped"
	EventPaused        EventType = "paused"
	EventResumed       EventType = "resumed"
	EventSeeked        EventType = "seeked"
	EventVoteUpdated   EventType = "vote_updated"
	EventReadyState    EventType = "ready_state"
	EventRoomClosed    EventType = "room_closed"
)

// Event is one room state change, broadcast to every subscriber.
type Event struct {
	Type   EventType `json:"type"`
	RoomID string    `json:"roomId"`
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
	id := b.nextID
	b.nextID++
	ch := make(chan Event, subscriberBuffer)
	b.subs[id] = ch
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			close(ch)
			b.mu.Unlock()
		})
	}
	return ch, cancel
}

// Publish delivers e to every subscriber without blocking: a subscriber that
// cannot keep up misses events rather than stalling the room.
func (b *Bus) Publish(e Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Clock abstracts time so the room timeline can be driven in tests.
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

func (realClock) AfterFunc(d time.Duration, f func()) Timer {
	if d < 0 {
		d = 0
	}
	return time.AfterFunc(d, f)
}
