// Package rooms implements Listen Together. The server is the single source of
// truth for a room: it owns the queue, decides when a track starts (once every
// member is ready, or the readiness timeout has passed), keeps the room-wide
// timeline, applies the skip vote and announces every change as an event.
//
// Clients are followers. They sync their clock to the server, download the
// rendition they were assigned, report readiness with the duration they will
// actually play, and then follow startedAt.
package rooms

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/prismusic/internal/config"
	"codeberg.org/kyleraykbs/prismusic/internal/match"
	"codeberg.org/kyleraykbs/prismusic/internal/ranking"
	"codeberg.org/kyleraykbs/prismusic/internal/store"
)

// Errors returned by room commands.
var (
	ErrRoomNotFound   = errors.New("room not found")
	ErrMemberNotFound = errors.New("not a member of this room")
	ErrForbidden      = errors.New("only the host may do that")
	ErrNoPlayback     = errors.New("no track is playing")
	ErrInvalidVote    = errors.New("vote must be between 1 and 5")
	ErrInvalidOrder   = errors.New("queue order does not match the queue")
	ErrInvalidSeek    = errors.New("seek position must not be negative")
	ErrInvalidControl = errors.New("controls must be \"host\" or \"everyone\"")
)

// defaultTimelineMs is used when neither the members nor the canonical track
// tell us how long a track is.
const defaultTimelineMs int64 = 3 * 60 * 1000

// prepareTimeout bounds the variant resolution of a preparing track.
const prepareTimeout = 45 * time.Second

// Controls decides who may drive a room.
type Controls string

// Control policies.
const (
	ControlsHost     Controls = "host"
	ControlsEveryone Controls = "everyone"
)

// Member is one participant in a room. UserID is nil for guests.
type Member struct {
	ID         string     `json:"id"`
	UserID     *uuid.UUID `json:"userId,omitempty"`
	Name       string     `json:"name"`
	JoinedAtMs int64      `json:"joinedAtMs"`
}

// QueueItem is one entry of a room's queue.
type QueueItem struct {
	ID        string    `json:"id"`
	TrackID   uuid.UUID `json:"trackId"`
	Title     string    `json:"title"`
	AddedBy   string    `json:"addedBy"`
	AddedAtMs int64     `json:"addedAtMs"`
}

// ReadyReport is a member's answer to "this is what I will play".
type ReadyReport struct {
	MemberID   string    `json:"memberId"`
	TrackID    uuid.UUID `json:"trackId"`
	VariantID  uuid.UUID `json:"variantId"`
	DurationMs int64     `json:"durationMs"`
	AtMs       int64     `json:"atMs"`
}

// playback is the current item with its readiness gate and timeline.
type playback struct {
	item QueueItem
	// variants maps member id to the variant the server picked for it; a
	// member's own readiness report overrides the guess.
	variants map[string]uuid.UUID
	ready    map[string]ReadyReport
	votes    map[string]int

	// startedAtMs is zero until the track actually starts.
	startedAtMs int64
	// timelineMs is the room-wide length: the longest rendition in the room.
	timelineMs int64
	// fallbackMs is the canonical track duration, used when nobody reports.
	fallbackMs int64

	paused           bool
	pausedPositionMs int64

	prepareTimer   Timer
	advanceTimer   Timer
	prepareVersion int
	advanceVersion int
}

// positionMs is the room position at server time nowMs.
func (p *playback) positionMs(nowMs int64) int64 {
	if p.startedAtMs == 0 {
		return 0
	}
	if p.paused {
		return clamp(p.pausedPositionMs, p.timelineMs)
	}
	return clamp(p.pausedPositionMs+(nowMs-p.startedAtMs), p.timelineMs)
}

func clamp(position, timeline int64) int64 {
	if position < 0 {
		return 0
	}
	if timeline > 0 && position > timeline {
		return timeline
	}
	return position
}

// room is the mutable room state; every access happens under Manager.mu.
type room struct {
	id          string
	name        string
	host        string
	controls    Controls
	createdAtMs int64

	members map[string]*Member
	order   []string

	queue   []QueueItem
	current *playback
}

// Snapshot is a room as clients see it.
type Snapshot struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Host        string        `json:"host"`
	Controls    Controls      `json:"controls"`
	CreatedAtMs int64         `json:"createdAtMs"`
	Members     []Member      `json:"members"`
	Queue       []QueueItem   `json:"queue"`
	Current     *PlaybackView `json:"current,omitempty"`
	ServerNowMs int64         `json:"serverNowMs"`
	Skip        SkipRules     `json:"skip"`
}

// PlaybackView is the current track's state, including the room position at
// the server time reported in the snapshot.
type PlaybackView struct {
	Item        QueueItem         `json:"item"`
	StartedAtMs int64             `json:"startedAtMs"`
	TimelineMs  int64             `json:"timelineMs"`
	PositionMs  int64             `json:"positionMs"`
	Paused      bool              `json:"paused"`
	Variants    map[string]string `json:"variants"`
	Ready       []ReadyReport     `json:"ready"`
	Awaiting    []string          `json:"awaiting"`
	CatchingUp  []string          `json:"catchingUp"`
	Votes       map[string]int    `json:"votes"`
	MeanScore   float64           `json:"meanScore"`
}

// SkipRules are the tunables clients show next to the vote buttons.
type SkipRules struct {
	SkipThreshold        float64 `json:"skipThreshold"`
	MinVotersForSkip     int     `json:"minVotersForSkip"`
	VoterFractionForSkip float64 `json:"voterFractionForSkip"`
	ReadyTimeoutSeconds  int     `json:"readyTimeoutSeconds"`
}

// Store is the repository slice rooms need.
type Store interface {
	store.TrackRepo
	store.VoteRepo
}

// Manager owns every room.
type Manager struct {
	mu        sync.Mutex
	rooms     map[string]*room
	roomOrder []string

	cfg     *config.Config
	logger  *slog.Logger
	bus     *Bus
	clock   Clock
	store   Store
	matcher *match.Matcher
	ranking *ranking.Service
}

// NewManager returns a room manager.
func NewManager(cfg *config.Config, st Store, matcher *match.Matcher, rank *ranking.Service, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		rooms:   make(map[string]*room),
		cfg:     cfg,
		logger:  logger,
		bus:     NewBus(),
		clock:   realClock{},
		store:   st,
		matcher: matcher,
		ranking: rank,
	}
}

// Bus exposes the event stream.
func (m *Manager) Bus() *Bus { return m.bus }

// Subscribe returns a channel of room events and a cancel function.
func (m *Manager) Subscribe() (<-chan Event, func()) { return m.bus.Subscribe() }

// Close stops every room timer. Rooms are in-memory state; nothing to persist.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, room := range m.rooms {
		m.stopTimersLocked(room)
	}
}

// SetClock replaces the clock; it exists for tests that drive the timeline.
func (m *Manager) SetClock(c Clock) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clock = c
}

// NowMs is the server clock rooms schedule against.
func (m *Manager) NowMs() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nowMsLocked()
}

func (m *Manager) nowMsLocked() int64 { return m.clock.Now().UnixMilli() }

// Create opens a room with host as its first member.
func (m *Manager) Create(name string, controls Controls, host Member) (*Snapshot, error) {
	if controls == "" {
		controls = ControlsEveryone
	}
	if controls != ControlsHost && controls != ControlsEveryone {
		return nil, ErrInvalidControl
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	id := uuid.NewString()
	if name == "" {
		name = "room " + id[:8]
	}
	host.JoinedAtMs = m.nowMsLocked()
	room := &room{
		id:          id,
		name:        name,
		host:        host.ID,
		controls:    controls,
		createdAtMs: m.nowMsLocked(),
		members:     map[string]*Member{host.ID: &host},
		order:       []string{host.ID},
	}
	m.rooms[id] = room
	m.roomOrder = append(m.roomOrder, id)

	m.publishLocked(room, EventMemberJoined, map[string]any{"member": host})
	m.logger.Info("room created", "room", id, "host", host.ID, "controls", controls)
	return m.snapshotLocked(room), nil
}

// List returns every room, oldest first.
func (m *Manager) List() []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]Snapshot, 0, len(m.roomOrder))
	for _, id := range m.roomOrder {
		if room, ok := m.rooms[id]; ok {
			out = append(out, *m.snapshotLocked(room))
		}
	}
	return out
}

// Get returns one room's state.
func (m *Manager) Get(roomID string) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, ok := m.rooms[roomID]
	if !ok {
		return nil, ErrRoomNotFound
	}
	return m.snapshotLocked(room), nil
}

// Join adds a member (or refreshes an existing one) and returns the room. A
// member arriving while a track is preparing or playing is given a rendition of
// that track, so joining mid-track still means playing along.
func (m *Manager) Join(roomID string, member Member) (*Snapshot, error) {
	m.mu.Lock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	_, known := room.members[member.ID]
	if !known {
		room.order = append(room.order, member.ID)
	}
	member.JoinedAtMs = m.nowMsLocked()
	room.members[member.ID] = &member

	m.publishLocked(room, EventMemberJoined, map[string]any{"member": member})

	var prepareItem string
	if room.current != nil && room.current.variants[member.ID] == uuid.Nil {
		prepareItem = room.current.item.ID
	}
	m.maybeStartLocked(room) // a new member must not block an already-ready room
	snapshot := m.snapshotLocked(room)
	m.mu.Unlock()

	if prepareItem != "" {
		// Assignment can need a provider search, so it happens off the lock.
		go m.prepare(roomID, prepareItem)
	}
	return snapshot, nil
}

// Leave removes a member. A room without members is closed; a room without its
// host promotes the longest-standing member.
func (m *Manager) Leave(roomID, memberID string) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}
	if _, ok := room.members[memberID]; !ok {
		return nil, ErrMemberNotFound
	}

	delete(room.members, memberID)
	room.order = removeString(room.order, memberID)
	if room.current != nil {
		// A member who left must not keep the room waiting or voting.
		delete(room.current.ready, memberID)
		delete(room.current.votes, memberID)
		delete(room.current.variants, memberID)
	}
	m.publishLocked(room, EventMemberLeft, map[string]any{"memberId": memberID})

	if len(room.members) == 0 {
		// The last member leaving closes the room; that is still a successful
		// leave, so callers are not told the room was missing.
		m.closeRoomLocked(room)
		return nil, nil
	}
	if room.host == memberID {
		room.host = room.order[0]
		m.logger.Info("room host promoted", "room", room.id, "host", room.host)
		m.publishLocked(room, EventMemberJoined, map[string]any{"host": room.host})
	}
	m.maybeStartLocked(room)
	return m.snapshotLocked(room), nil
}

// Enqueue appends a track, starting it right away when the room is idle.
func (m *Manager) Enqueue(ctx context.Context, roomID, memberID string, trackID uuid.UUID) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}
	if _, ok := room.members[memberID]; !ok {
		return nil, ErrMemberNotFound
	}
	track, err := m.store.Track(ctx, trackID)
	if err != nil {
		return nil, err
	}

	room.queue = append(room.queue, QueueItem{
		ID:        uuid.NewString(),
		TrackID:   track.ID,
		Title:     track.Title,
		AddedBy:   memberID,
		AddedAtMs: m.nowMsLocked(),
	})
	m.publishLocked(room, EventQueueUpdated, map[string]any{"queue": room.queue})
	if room.current == nil {
		m.beginNextLocked(room)
	}
	return m.snapshotLocked(room), nil
}

// Remove drops one queued item.
func (m *Manager) Remove(roomID, memberID, itemID string) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}
	if err := m.checkControlLocked(room, memberID); err != nil {
		return nil, err
	}

	index := -1
	for i, item := range room.queue {
		if item.ID == itemID {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, store.ErrNotFound
	}
	room.queue = append(room.queue[:index], room.queue[index+1:]...)
	m.publishLocked(room, EventQueueUpdated, map[string]any{"queue": room.queue})
	return m.snapshotLocked(room), nil
}

// Reorder rearranges the queue; itemIDs must be a permutation of it.
func (m *Manager) Reorder(roomID, memberID string, itemIDs []string) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}
	if err := m.checkControlLocked(room, memberID); err != nil {
		return nil, err
	}
	if len(itemIDs) != len(room.queue) {
		return nil, ErrInvalidOrder
	}

	byID := make(map[string]QueueItem, len(room.queue))
	for _, item := range room.queue {
		byID[item.ID] = item
	}
	reordered := make([]QueueItem, 0, len(itemIDs))
	seen := make(map[string]bool, len(itemIDs))
	for _, id := range itemIDs {
		item, ok := byID[id]
		if !ok || seen[id] {
			return nil, ErrInvalidOrder
		}
		seen[id] = true
		reordered = append(reordered, item)
	}

	room.queue = reordered
	m.publishLocked(room, EventQueueUpdated, map[string]any{"queue": room.queue})
	return m.snapshotLocked(room), nil
}

// Pause freezes the room position.
func (m *Manager) Pause(roomID, memberID string) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}
	if err := m.checkControlLocked(room, memberID); err != nil {
		return nil, err
	}
	playback := room.current
	if playback == nil || playback.startedAtMs == 0 || playback.paused {
		return nil, ErrNoPlayback
	}

	playback.pausedPositionMs = playback.positionMs(m.nowMsLocked())
	playback.paused = true
	playback.advanceVersion++
	if playback.advanceTimer != nil {
		playback.advanceTimer.Stop()
		playback.advanceTimer = nil
	}
	m.publishLocked(room, EventPaused, map[string]any{
		"positionMs": playback.pausedPositionMs,
		"item":       playback.item,
	})
	return m.snapshotLocked(room), nil
}

// Resume continues from the paused position.
func (m *Manager) Resume(roomID, memberID string) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}
	if err := m.checkControlLocked(room, memberID); err != nil {
		return nil, err
	}
	playback := room.current
	if playback == nil || playback.startedAtMs == 0 || !playback.paused {
		return nil, ErrNoPlayback
	}

	playback.paused = false
	playback.startedAtMs = m.nowMsLocked()
	m.publishLocked(room, EventResumed, map[string]any{
		"positionMs": playback.positionMs(m.nowMsLocked()),
		"startedAt":  playback.startedAtMs,
		"item":       playback.item,
	})
	m.scheduleAdvanceLocked(room, playback)
	return m.snapshotLocked(room), nil
}

// Seek moves the room position. Clients correct their drift with small seeks.
func (m *Manager) Seek(roomID, memberID string, positionMs int64) (*Snapshot, error) {
	if positionMs < 0 {
		return nil, ErrInvalidSeek
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}
	if err := m.checkControlLocked(room, memberID); err != nil {
		return nil, err
	}
	playback := room.current
	if playback == nil || playback.startedAtMs == 0 {
		return nil, ErrNoPlayback
	}

	playback.pausedPositionMs = positionMs
	playback.startedAtMs = m.nowMsLocked()
	if playback.timelineMs > 0 && positionMs >= playback.timelineMs {
		m.advanceLocked(context.Background(), room, "seek")
		return m.snapshotLocked(room), nil
	}
	m.publishLocked(room, EventSeeked, map[string]any{
		"positionMs": positionMs,
		"startedAt":  playback.startedAtMs,
		"item":       playback.item,
	})
	m.scheduleAdvanceLocked(room, playback)
	return m.snapshotLocked(room), nil
}

// Skip advances past the current track.
func (m *Manager) Skip(roomID, memberID string) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}
	if err := m.checkControlLocked(room, memberID); err != nil {
		return nil, err
	}
	if room.current == nil {
		return nil, ErrNoPlayback
	}
	m.advanceLocked(context.Background(), room, "skipped")
	return m.snapshotLocked(room), nil
}

// Vote records a member's score and applies the skip rule.
func (m *Manager) Vote(ctx context.Context, roomID, memberID string, score int) (*Snapshot, error) {
	if score < 1 || score > 5 {
		return nil, ErrInvalidVote
	}

	m.mu.Lock()
	room, err := m.roomLocked(roomID)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	member, ok := room.members[memberID]
	if !ok {
		m.mu.Unlock()
		return nil, ErrMemberNotFound
	}
	playback := room.current
	if playback == nil {
		m.mu.Unlock()
		return nil, ErrNoPlayback
	}

	playback.votes[memberID] = score
	mean := meanScore(playback.votes)
	m.publishLocked(room, EventVoteUpdated, map[string]any{
		"memberId": memberID,
		"score":    score,
		"mean":     mean,
		"votes":    len(playback.votes),
		"item":     playback.item,
	})
	skip := m.shouldSkipLocked(room, playback)
	trackID := playback.item.TrackID
	snapshot := m.snapshotLocked(room)
	m.mu.Unlock()

	// Votes outlive the room, so they are recorded for later stats.
	m.persistVote(ctx, store.Vote{
		RoomID:   roomID,
		TrackID:  trackID,
		MemberID: memberID,
		UserID:   member.UserID,
		Score:    score,
	})

	if !skip {
		return snapshot, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.rooms[roomID]
	if !ok || current.current == nil || current.current.item.TrackID != trackID {
		return snapshot, nil
	}
	m.advanceLocked(ctx, current, "votes")
	return m.snapshotLocked(current), nil
}

// Ready records that a member has its rendition downloaded and tells the room
// how long it will actually play. The room starts once everyone is ready.
func (m *Manager) Ready(roomID, memberID string, trackID, variantID uuid.UUID, durationMs int64) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}
	if _, ok := room.members[memberID]; !ok {
		return nil, ErrMemberNotFound
	}
	playback := room.current
	if playback == nil || playback.item.TrackID != trackID {
		return nil, ErrNoPlayback
	}

	report := ReadyReport{
		MemberID:   memberID,
		TrackID:    trackID,
		VariantID:  variantID,
		DurationMs: durationMs,
		AtMs:       m.nowMsLocked(),
	}
	playback.ready[memberID] = report
	// The member's own rendition is the truth; the server's pick was a guess.
	if variantID != uuid.Nil {
		playback.variants[memberID] = variantID
	}

	m.publishLocked(room, EventReadyState, map[string]any{
		"ready":   len(playback.ready),
		"members": len(room.members),
		"item":    playback.item,
	})
	m.maybeStartLocked(room)
	return m.snapshotLocked(room), nil
}

// --- internals -------------------------------------------------------------

func (m *Manager) roomLocked(roomID string) (*room, error) {
	room, ok := m.rooms[roomID]
	if !ok {
		return nil, ErrRoomNotFound
	}
	return room, nil
}

// checkControlLocked enforces the room's control policy.
func (m *Manager) checkControlLocked(room *room, memberID string) error {
	if _, ok := room.members[memberID]; !ok {
		return ErrMemberNotFound
	}
	if room.controls == ControlsHost && memberID != room.host {
		return ErrForbidden
	}
	return nil
}

// beginNextLocked moves the next queued item into preparation.
func (m *Manager) beginNextLocked(room *room) {
	m.stopTimersLocked(room)

	if len(room.queue) == 0 {
		room.current = nil
		m.publishLocked(room, EventQueueUpdated, map[string]any{"queue": room.queue})
		return
	}

	item := room.queue[0]
	room.queue = room.queue[1:]
	playback := &playback{
		item:     item,
		variants: map[string]uuid.UUID{},
		ready:    map[string]ReadyReport{},
		votes:    map[string]int{},
	}
	if track, err := m.store.Track(context.Background(), item.TrackID); err == nil {
		playback.fallbackMs = track.DurationMs
	}
	room.current = playback

	m.publishLocked(room, EventQueueUpdated, map[string]any{"queue": room.queue, "preparing": item})

	// Variant assignment needs the providers (and possibly the network), so it
	// happens off the lock.
	go m.prepare(room.id, item.ID)

	timeout := time.Duration(m.cfg.ListenTogether.ReadyTimeoutSeconds) * time.Second
	if len(room.members) == 0 || timeout <= 0 {
		// Nobody to wait for, or waiting disabled: start as soon as the
		// variants are assigned.
		return
	}
	playback.prepareVersion++
	version := playback.prepareVersion
	playback.prepareTimer = m.clock.AfterFunc(timeout, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		current, ok := m.rooms[room.id]
		if !ok || current.current != playback || playback.prepareVersion != version {
			return
		}
		m.logger.Info("room: readiness timeout, starting without everyone",
			"room", room.id, "item", playback.item.ID)
		m.startLocked(current, playback)
	})
}

// prepare resolves a playable rendition per member and announces the
// assignments. It runs outside the manager lock.
func (m *Manager) prepare(roomID, itemID string) {
	ctx, cancel := context.WithTimeout(context.Background(), prepareTimeout)
	defer cancel()

	m.mu.Lock()
	room, ok := m.rooms[roomID]
	if !ok || room.current == nil || room.current.item.ID != itemID {
		m.mu.Unlock()
		return
	}
	trackID := room.current.item.TrackID
	members := make([]Member, 0, len(room.order))
	for _, memberID := range room.order {
		if member, ok := room.members[memberID]; ok {
			members = append(members, *member)
		}
	}
	m.mu.Unlock()

	variants, err := m.matcher.Resolve(ctx, trackID)
	if err != nil {
		m.logger.Warn("room: no playable rendition", "room", roomID, "track", trackID, "error", err)
	}

	assignments := make(map[string]uuid.UUID, len(members))
	for _, member := range members {
		if variant, ok := m.pickVariant(ctx, member, variants); ok {
			assignments[member.ID] = variant.ID
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	room, ok = m.rooms[roomID]
	if !ok || room.current == nil || room.current.item.ID != itemID {
		return
	}
	room.current.variants = assignments
	m.publishLocked(room, EventTrackPrepared, map[string]any{
		"item":     room.current.item,
		"variants": assignments,
	})
	m.maybeStartLocked(room)
}

// pickVariant chooses the rendition for one member: their effective provider
// ranking decides, exactly as it does for the CLI.
func (m *Manager) pickVariant(ctx context.Context, member Member, variants []store.Variant) (store.Variant, bool) {
	order := m.cfg.DefaultProviderOrder
	if member.UserID != nil {
		if own, err := m.ranking.User(ctx, *member.UserID); err == nil {
			order = own
		} else {
			m.logger.Warn("room: ranking lookup failed", "member", member.ID, "error", err)
		}
	} else if aggregate, err := m.ranking.Aggregate(ctx); err == nil {
		order = aggregate
	}
	return ranking.Pick(order, variants)
}

// maybeStartLocked starts the current track when every member is ready.
func (m *Manager) maybeStartLocked(room *room) {
	playback := room.current
	if playback == nil || playback.startedAtMs != 0 {
		return
	}
	for memberID := range room.members {
		if _, ready := playback.ready[memberID]; !ready {
			return
		}
	}
	m.startLocked(room, playback)
}

// startLocked fixes the timeline and the start instant, then announces
// track_started. Everyone who was not ready is catching up.
func (m *Manager) startLocked(room *room, playback *playback) {
	if playback == nil || playback.startedAtMs != 0 || room.current != playback {
		return
	}

	timeline := playback.fallbackMs
	for _, report := range playback.ready {
		if report.DurationMs > timeline {
			timeline = report.DurationMs
		}
	}
	if timeline <= 0 {
		timeline = defaultTimelineMs
	}
	playback.timelineMs = timeline
	playback.startedAtMs = m.nowMsLocked()
	playback.paused = false
	playback.pausedPositionMs = 0
	playback.prepareVersion++
	if playback.prepareTimer != nil {
		playback.prepareTimer.Stop()
		playback.prepareTimer = nil
	}

	m.publishLocked(room, EventTrackStarted, map[string]any{
		"item":       playback.item,
		"startedAt":  playback.startedAtMs,
		"timelineMs": timeline,
		"variants":   playback.variants,
	})
	m.logger.Info("room: track started",
		"room", room.id, "title", playback.item.Title,
		"timeline_ms", timeline, "ready", len(playback.ready), "members", len(room.members))
	m.scheduleAdvanceLocked(room, playback)
}

// scheduleAdvanceLocked (re)schedules the end of the current track.
func (m *Manager) scheduleAdvanceLocked(room *room, playback *playback) {
	if playback == nil || playback.startedAtMs == 0 {
		return
	}
	remaining := time.Duration(playback.timelineMs-playback.positionMs(m.nowMsLocked())) * time.Millisecond
	playback.advanceVersion++
	version := playback.advanceVersion
	if playback.advanceTimer != nil {
		playback.advanceTimer.Stop()
	}
	playback.advanceTimer = m.clock.AfterFunc(remaining, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		current, ok := m.rooms[room.id]
		if !ok || current.current != playback || playback.advanceVersion != version {
			return
		}
		m.advanceLocked(context.Background(), current, "completed")
	})
}

// advanceLocked ends the current track and prepares the next one.
func (m *Manager) advanceLocked(ctx context.Context, room *room, reason string) {
	playback := room.current
	if playback != nil {
		playback.advanceVersion++
		if playback.advanceTimer != nil {
			playback.advanceTimer.Stop()
			playback.advanceTimer = nil
		}
		if reason != "completed" {
			m.publishLocked(room, EventTrackSkipped, map[string]any{
				"item":       playback.item,
				"reason":     reason,
				"positionMs": playback.positionMs(m.nowMsLocked()),
				"votes":      playback.votes,
				"mean":       meanScore(playback.votes),
			})
		}
	}
	m.beginNextLocked(room)
}

// shouldSkipLocked applies the skip rule: enough voters, enough of the room,
// and a mean below the threshold.
func (m *Manager) shouldSkipLocked(room *room, playback *playback) bool {
	rules := m.cfg.ListenTogether
	if len(playback.votes) < rules.MinVotersForSkip {
		return false
	}
	if len(room.members) > 0 {
		fraction := float64(len(playback.votes)) / float64(len(room.members))
		if fraction < rules.VoterFractionForSkip {
			return false
		}
	}
	return meanScore(playback.votes) < rules.SkipThreshold
}

func (m *Manager) persistVote(ctx context.Context, vote store.Vote) {
	if err := m.store.SaveVote(ctx, &vote); err != nil {
		m.logger.Warn("room: persist vote", "room", vote.RoomID, "track", vote.TrackID, "error", err)
	}
}

// closeRoomLocked forgets a room nobody is in.
func (m *Manager) closeRoomLocked(room *room) {
	m.stopTimersLocked(room)
	delete(m.rooms, room.id)
	m.roomOrder = removeString(m.roomOrder, room.id)
	m.publishLocked(room, EventRoomClosed, nil)
	m.logger.Info("room closed", "room", room.id)
}

func (m *Manager) stopTimersLocked(room *room) {
	if room.current == nil {
		return
	}
	room.current.prepareVersion++
	room.current.advanceVersion++
	if room.current.prepareTimer != nil {
		room.current.prepareTimer.Stop()
		room.current.prepareTimer = nil
	}
	if room.current.advanceTimer != nil {
		room.current.advanceTimer.Stop()
		room.current.advanceTimer = nil
	}
}

func (m *Manager) publishLocked(room *room, eventType EventType, data any) {
	m.bus.Publish(Event{Type: eventType, RoomID: room.id, AtMs: m.nowMsLocked(), Data: data})
}

// snapshotLocked renders the room for clients.
func (m *Manager) snapshotLocked(room *room) *Snapshot {
	nowMs := m.nowMsLocked()

	snapshot := &Snapshot{
		ID:          room.id,
		Name:        room.name,
		Host:        room.host,
		Controls:    room.controls,
		CreatedAtMs: room.createdAtMs,
		Members:     make([]Member, 0, len(room.order)),
		Queue:       append(make([]QueueItem, 0, len(room.queue)), room.queue...),
		ServerNowMs: nowMs,
		Skip: SkipRules{
			SkipThreshold:        m.cfg.ListenTogether.SkipThreshold,
			MinVotersForSkip:     m.cfg.ListenTogether.MinVotersForSkip,
			VoterFractionForSkip: m.cfg.ListenTogether.VoterFractionForSkip,
			ReadyTimeoutSeconds:  m.cfg.ListenTogether.ReadyTimeoutSeconds,
		},
	}
	for _, memberID := range room.order {
		if member, ok := room.members[memberID]; ok {
			snapshot.Members = append(snapshot.Members, *member)
		}
	}

	playback := room.current
	if playback == nil {
		return snapshot
	}

	view := &PlaybackView{
		Item:        playback.item,
		StartedAtMs: playback.startedAtMs,
		TimelineMs:  playback.timelineMs,
		PositionMs:  playback.positionMs(nowMs),
		Paused:      playback.paused,
		Variants:    make(map[string]string, len(playback.variants)),
		Ready:       make([]ReadyReport, 0, len(playback.ready)),
		Awaiting:    []string{},
		CatchingUp:  []string{},
		Votes:       make(map[string]int, len(playback.votes)),
		MeanScore:   meanScore(playback.votes),
	}
	for memberID, variantID := range playback.variants {
		view.Variants[memberID] = variantID.String()
	}
	for _, report := range playback.ready {
		view.Ready = append(view.Ready, report)
		delete(view.Variants, report.MemberID) // covered by the report
	}
	sort.Slice(view.Ready, func(i, j int) bool { return view.Ready[i].MemberID < view.Ready[j].MemberID })
	for memberID, score := range playback.votes {
		view.Votes[memberID] = score
	}
	for _, memberID := range room.order {
		if _, ok := playback.ready[memberID]; ok {
			continue
		}
		if playback.startedAtMs == 0 {
			view.Awaiting = append(view.Awaiting, memberID)
		} else {
			view.CatchingUp = append(view.CatchingUp, memberID)
		}
	}
	snapshot.Current = view
	return snapshot
}

func meanScore(votes map[string]int) float64 {
	if len(votes) == 0 {
		return 0
	}
	sum := 0
	for _, score := range votes {
		sum += score
	}
	return float64(sum) / float64(len(votes))
}

func removeString(values []string, target string) []string {
	out := values[:0]
	for _, value := range values {
		if value != target {
			out = append(out, value)
		}
	}
	return out
}
