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

	"codeberg.org/kyleraykbs/musoak/internal/config"
	"codeberg.org/kyleraykbs/musoak/internal/match"
	"codeberg.org/kyleraykbs/musoak/internal/ranking"
	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// Errors returned by room commands.
var (
	ErrRoomNotFound   = errors.New("room not found")
	ErrMemberNotFound = errors.New("not a member of this room")
	ErrForbidden      = errors.New("only the host may do that")
	ErrWrongPassword  = errors.New("wrong password")
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
	// IconURL is the member's picture, empty for a guest. The Listening panel
	// shows it, so it travels with the member rather than needing a lookup.
	IconURL string `json:"iconUrl,omitempty"`
	// Out is set while this member is sitting the room's track out: they are
	// not waited for and their file is not what the room's length is measured
	// by. The manager sets it from its own record; a client never claims it.
	Out bool `json:"out,omitempty"`
	// IconVersion changes when that picture does, so the panel fetches the new
	// one instead of the copy its browser has been holding.
	IconVersion int `json:"iconVersion,omitempty"`
}

// QueueItem is one entry of a room's queue.
type QueueItem struct {
	ID        string    `json:"id"`
	TrackID   uuid.UUID `json:"trackId"`
	Title     string    `json:"title"`
	AddedBy   string    `json:"addedBy"`
	AddedAtMs int64     `json:"addedAtMs"`
	// ArtworkURL is a path on this server, empty when nothing is known yet. The
	// queue list shows it, and without it every row is a blank square.
	ArtworkURL string `json:"artworkUrl,omitempty"`
	// ArtistIDs lets a client open an artist's page from a queue row's menu:
	// the names alone cannot name a page.
	ArtistIDs []string `json:"artistIds,omitempty"`
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
	// timelineMs is the room-wide length: as long as the host's copy of the
	// track, since the host is the room's clock.
	timelineMs int64
	// durations is what each assigned rendition says it is long, known from the
	// store before anyone has measured the file they actually play.
	durations map[uuid.UUID]int64
	// prepared is set once the renditions have been assigned, and timedOut once
	// the room has waited long enough for the laggards. A track starts when both
	// have happened and everyone else is ready: assignment needs the providers
	// and can outlast the wait.
	prepared bool
	timedOut bool
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

// ownerIdentity is what a member is known by across sessions: their account
// when they have one, else the browser's own member id. Two tabs of one account
// are one owner, and a guest keeps the id their browser remembered, so the host
// who steps out and comes back is still the host rather than a stranger in
// their own room.
func ownerIdentity(member Member) string {
	if member.UserID != nil {
		return "user:" + member.UserID.String()
	}
	return "member:" + member.ID
}

// promoteHostLocked gives the room a new leader: the owner when they are still
// here, and otherwise whoever has been in the room longest. The host was the
// first one in, so the next-longest-standing member is the one who has waited
// longest for the room.
func (m *Manager) promoteHostLocked(room *room) {
	next := ""
	for _, memberID := range room.order {
		member, ok := room.members[memberID]
		if !ok {
			continue
		}
		if next == "" {
			next = memberID // room.order is join order: the first is the longest here
		}
		if room.owner != "" && ownerIdentity(*member) == room.owner {
			next = memberID
			break
		}
	}
	if next == "" || next == room.host {
		return
	}
	previous := room.host
	room.host = next
	m.logger.Info("room host promoted", "room", room.id, "host", next, "was", previous)
	m.publishLocked(room, EventHostChanged, map[string]any{"host": next, "was": previous})
}

// takeHostLocked makes a returning owner the host again. The room is theirs;
// whoever has been leading it steps aside without leaving it.
func (m *Manager) takeHostLocked(room *room, member Member) bool {
	if room.owner == "" || ownerIdentity(member) != room.owner || room.host == member.ID {
		return false
	}
	previous := room.host
	room.host = member.ID
	m.logger.Info("room host returned", "room", room.id, "host", member.ID, "was", previous)
	m.publishLocked(room, EventHostChanged, map[string]any{"host": member.ID, "was": previous})
	return true
}

// room is the mutable room state; every access happens under Manager.mu.
type room struct {
	id   string
	name string
	host string
	// owner is who the room belongs to, in a form that outlives a session: the
	// account's id when they have one, else the browser's member id. The host
	// is whoever is leading right now; the owner is who leads again when they
	// come back.
	owner       string
	ownerName   string
	controls    Controls
	password    string // join password, empty for none; never leaves the manager
	createdAtMs int64

	members map[string]*Member
	order   []string

	// queues holds each member's own queue, including the item they have in
	// flight until it finishes; master is the fair play order derived from it.
	queues  map[string][]QueueItem
	master  []QueueItem
	current *playback
	// next is the song after this one, built while this one plays: its
	// renditions are assigned and members may report ready for it, so that the
	// advance does not begin with a download nobody has started yet.
	next *playback
	// out is who is sitting the room out. They are not waited for and their
	// file is not what the room's song is measured by: a member holding a short
	// or broken copy needs a way to say so rather than hold the room up, and a
	// host who cannot play the song hands the room back to the room.
	out map[string]bool
}

// Snapshot is a room as clients see it.
type Snapshot struct {
	ID          string                 `json:"id"`
	Name        string                 `json:"name"`
	Host        string                 `json:"host"`
	Controls    Controls               `json:"controls"`
	CreatedAtMs int64                  `json:"createdAtMs"`
	Members     []Member               `json:"members"`
	MemberCount int                    `json:"memberCount"`
	HasPassword bool                   `json:"hasPassword"`
	Queues      map[string][]QueueItem `json:"queues"`
	MasterQueue []QueueItem            `json:"masterQueue"`
	Queue       []QueueItem            `json:"queue"`
	Current     *PlaybackView          `json:"current,omitempty"`
	Next        *QueueItem             `json:"next,omitempty"`
	ServerNowMs int64                  `json:"serverNowMs"`
	Skip        SkipRules              `json:"skip"`
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
	// Prepared is set once the renditions have been assigned. A track starts
	// only after that, so a client can tell "waiting on the room" from "waiting
	// on me".
	Prepared   bool           `json:"prepared"`
	Awaiting   []string       `json:"awaiting"`
	CatchingUp []string       `json:"catchingUp"`
	Votes      map[string]int `json:"votes"`
	MeanScore  float64        `json:"meanScore"`
}

// SkipRules are the tunables clients show next to the vote buttons.
type SkipRules struct {
	SkipThreshold        float64 `json:"skipThreshold"`
	MinVotersForSkip     int     `json:"minVotersForSkip"`
	VoterFractionForSkip float64 `json:"voterFractionForSkip"`
	// ReadyFraction is how much of the room must have the song to start it.
	ReadyFraction       float64 `json:"readyFraction"`
	ReadyTimeoutSeconds int     `json:"readyTimeoutSeconds"`
}

// Store is the repository slice rooms need.
type Store interface {
	store.TrackRepo
	store.VoteRepo
	store.SourceRepo
}

// Manager owns every room.
type Manager struct {
	mu        sync.Mutex
	rooms     map[string]*room
	roomOrder []string
	// connections counts the live event sockets per member identity. Two tabs on
	// one account are one listener: closing one window must not take them out of
	// the rooms they are still listening in.
	connections map[string]int
	// leaving holds the pending leave for a member whose last socket closed. A
	// client that reconnects drops its socket and opens another, and that gap
	// must not read as leaving the room; a tab that closed for good must.
	leaving map[string]*time.Timer

	cfg     *config.Config
	logger  *slog.Logger
	bus     *Bus
	clock   Clock
	store   Store
	matcher *match.Matcher
	ranking *ranking.Service
	// warm asks for a rendition to be fetched before anybody needs it. The room
	// is what assigns variants and knows the play order, so the room is what can
	// say which files the next song will need - and a client's own warming is
	// only ever as good as the version of the client doing it.
	warm func(uuid.UUID)
}

// SetWarm installs the callback the room uses to fetch a rendition ahead of
// time. Without one the room does not warm anything: nothing else depends on it.
func (m *Manager) SetWarm(fn func(uuid.UUID)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.warm = fn
}

// warmLocked asks for the assigned renditions of a playback to be fetched. It
// is called once the assignment is known, which is the earliest moment anybody
// can say what the song will be played from.
func (m *Manager) warmLocked(playback *playback) {
	if m.warm == nil || playback == nil {
		return
	}
	for _, variantID := range playback.variants {
		if variantID != uuid.Nil {
			go m.warm(variantID)
		}
	}
}

// NewManager returns a room manager.
func NewManager(cfg *config.Config, st Store, matcher *match.Matcher, rank *ranking.Service, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{
		rooms:       make(map[string]*room),
		connections: make(map[string]int),
		leaving:     make(map[string]*time.Timer),
		cfg:         cfg,
		logger:      logger,
		bus:         NewBus(),
		clock:       realClock{},
		store:       st,
		matcher:     matcher,
		ranking:     rank,
	}
}

// socketGrace is how long a member's rooms are kept after their last event
// socket closes. Long enough that a client reconnecting through a dropped
// socket is not taken out of the room it is still listening to.
const socketGrace = 10 * time.Second

// Connect records that a member has an event socket open, and cancels a leave
// that was waiting on the old one.
func (m *Manager) Connect(memberID string) {
	if memberID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if timer, ok := m.leaving[memberID]; ok {
		timer.Stop()
		delete(m.leaving, memberID)
	}
	m.connections[memberID]++
}

// Disconnect drops one of a member's event sockets, and takes them out of every
// room they were in once it was the last one and the grace has passed.
//
// A tab that closes is a listener that has gone: without this, a member who shut
// the window sits in the room for ever, waiting to be skipped past and holding
// the timeline open. A second tab on the same account keeps them there, and so
// does a client that is only reconnecting - only the last socket, staying
// closed, is a leave.
func (m *Manager) Disconnect(memberID string) {
	if memberID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.connections[memberID] > 1 {
		m.connections[memberID]--
		return
	}
	delete(m.connections, memberID)
	if timer, ok := m.leaving[memberID]; ok {
		timer.Stop()
	}
	m.leaving[memberID] = time.AfterFunc(socketGrace, func() { m.leaveEveryRoom(memberID) })
}

// leaveEveryRoom takes a member out of every room they are in, once the grace
// has passed with no socket of theirs coming back.
func (m *Manager) leaveEveryRoom(memberID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.leaving, memberID)
	if m.connections[memberID] > 0 {
		return
	}
	for _, room := range m.rooms {
		if _, ok := room.members[memberID]; ok {
			m.leaveLocked(room, memberID)
		}
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

// Create opens a room with host as its first member. The password is kept in
// memory only; empty means anyone may join.
func (m *Manager) Create(name string, controls Controls, password string, host Member) (*Snapshot, error) {
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
		owner:       ownerIdentity(host),
		ownerName:   host.Name,
		controls:    controls,
		password:    password,
		createdAtMs: m.nowMsLocked(),
		members:     map[string]*Member{host.ID: &host},
		order:       []string{host.ID},
		queues:      map[string][]QueueItem{host.ID: {}},
		out:         map[string]bool{},
	}
	m.rooms[id] = room
	m.roomOrder = append(m.roomOrder, id)

	m.publishLocked(room, EventMemberJoined, map[string]any{"member": host, "memberCount": 1})
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

// withoutMember is the order with one member's id taken out, keeping the rest
// as it was.
func withoutMember(order []string, memberID string) []string {
	out := make([]string, 0, len(order))
	for _, id := range order {
		if id != memberID {
			out = append(out, id)
		}
	}
	return out
}

// Join adds a member (or refreshes an existing one) and returns the room. A
// member arriving while a track is preparing or playing is given a rendition of
// that track, so joining mid-track still means playing along. The password must
// match when the room has one.
//
// `supersedes` names a member this one replaces: a browser that joined as a
// guest and has since signed in, whose account is a different member id. What
// that member had queued comes across - it was the same person - and the
// membership it was goes, so the room does not show somebody twice, once under
// the name their browser made up.
func (m *Manager) Join(roomID string, member Member, password, supersedes string) (*Snapshot, error) {
	m.mu.Lock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if room.password != "" && room.password != password {
		m.mu.Unlock()
		return nil, ErrWrongPassword
	}
	_, known := room.members[member.ID]
	if !known {
		room.order = append(room.order, member.ID)
		room.queues[member.ID] = []QueueItem{}
		recomputeMasterLocked(room) // the round-robin gains a (still empty) slot
	}
	if supersedes != "" && supersedes != member.ID {
		if _, wasThere := room.members[supersedes]; wasThere {
			if len(room.queues[member.ID]) == 0 {
				room.queues[member.ID] = append([]QueueItem(nil), room.queues[supersedes]...)
			}
			// The member being replaced is the same person: if they were leading
			// the room, the id taking their place leads it now, rather than the
			// room being left pointing at somebody who is gone.
			wasHost := room.host == supersedes
			delete(room.members, supersedes)
			delete(room.queues, supersedes)
			room.order = withoutMember(room.order, supersedes)
			recomputeMasterLocked(room)
			if wasHost {
				room.host = member.ID
			}
			m.publishLocked(room, EventMemberLeft, map[string]any{
				"memberId": supersedes, "memberCount": len(room.members),
			})
		}
	}
	member.JoinedAtMs = m.nowMsLocked()
	room.members[member.ID] = &member
	// The room belongs to its owner, so a host who stepped out and came back
	// leads it again rather than sitting in their own room as a guest.
	m.takeHostLocked(room, member)

	m.publishLocked(room, EventMemberJoined, map[string]any{"member": member, "memberCount": len(room.members)})
	if !known {
		m.publishLocked(room, EventQueueUpdated, queueDataLocked(room))
	}

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
	return m.leaveLocked(room, memberID), nil
}

// leaveLocked is Leave's body, for callers that already hold the lock.
func (m *Manager) leaveLocked(room *room, memberID string) *Snapshot {
	delete(room.members, memberID)
	room.order = removeString(room.order, memberID)
	// A member who leaves takes their queue with them.
	delete(room.queues, memberID)
	recomputeMasterLocked(room)
	if room.current != nil {
		// A member who left must not keep the room waiting or voting.
		delete(room.current.ready, memberID)
		delete(room.current.votes, memberID)
		delete(room.current.variants, memberID)
	}
	// The member who held the shortest file may be the one who left, which
	// moves the end of the track they were holding down.
	m.retimeLocked(room, room.current)
	m.publishLocked(room, EventMemberLeft, map[string]any{"memberId": memberID, "memberCount": len(room.members)})
	m.publishLocked(room, EventQueueUpdated, queueDataLocked(room))

	if len(room.members) == 0 {
		// The last member leaving closes the room; that is still a successful
		// leave, so callers are not told the room was missing.
		m.closeRoomLocked(room)
		return nil
	}
	if room.host == memberID {
		m.promoteHostLocked(room)
	}
	m.maybeStartLocked(room)
	return m.snapshotLocked(room)
}

// Enqueue appends a track to the member's own queue, starting it right away
// when the room is idle.
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
	// The header may predate the cover, so fall back to a lookup: the queue rows
	// show the artwork, and a blank one is the whole row's look.
	artwork := track.ArtworkURL
	if artwork == "" {
		if url, err := m.store.TrackArtwork(ctx, trackID); err == nil {
			artwork = url
		}
	}

	// The row's menu opens the artist's page, so the ids travel with the item.
	var artistIDs []string
	if artists, err := m.store.TrackArtists(ctx, trackID); err == nil {
		artistIDs = make([]string, 0, len(artists))
		for _, artist := range artists {
			artistIDs = append(artistIDs, artist.ID.String())
		}
	}

	room.queues[memberID] = append(room.queues[memberID], QueueItem{
		ID:         uuid.NewString(),
		TrackID:    track.ID,
		Title:      track.Title,
		AddedBy:    memberID,
		AddedAtMs:  m.nowMsLocked(),
		ArtworkURL: artwork,
		ArtistIDs:  artistIDs,
	})
	recomputeMasterLocked(room)
	m.publishLocked(room, EventQueueUpdated, queueDataLocked(room))
	if room.current == nil {
		m.beginNextLocked(room)
	}
	return m.snapshotLocked(room), nil
}

// Remove drops one queued item. A member edits their own queue; the host may
// edit anyone's.
func (m *Manager) Remove(roomID, memberID, itemID string) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}

	for _, owner := range room.order {
		items := room.queues[owner]
		for i, item := range items {
			if item.ID != itemID {
				continue
			}
			if err := m.checkQueueEditLocked(room, memberID, owner); err != nil {
				return nil, err
			}
			room.queues[owner] = append(items[:i], items[i+1:]...)
			recomputeMasterLocked(room)
			m.publishLocked(room, EventQueueUpdated, queueDataLocked(room))
			return m.snapshotLocked(room), nil
		}
	}
	return nil, store.ErrNotFound
}

// Reorder rearranges one member's queue; itemIDs must be a permutation of it.
// A member edits their own queue; the host may edit anyone's.
func (m *Manager) Reorder(roomID, memberID string, itemIDs []string) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}
	if len(itemIDs) == 0 {
		return nil, ErrInvalidOrder
	}

	// The items name the queue they belong to.
	owner := ""
	for _, candidate := range room.order {
		for _, item := range room.queues[candidate] {
			if item.ID == itemIDs[0] {
				owner = candidate
			}
		}
	}
	if owner == "" {
		return nil, ErrInvalidOrder
	}
	items := room.queues[owner]
	if len(itemIDs) != len(items) {
		return nil, ErrInvalidOrder
	}
	if err := m.checkQueueEditLocked(room, memberID, owner); err != nil {
		return nil, err
	}

	byID := make(map[string]QueueItem, len(items))
	for _, item := range items {
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

	room.queues[owner] = reordered
	recomputeMasterLocked(room)
	m.publishLocked(room, EventQueueUpdated, queueDataLocked(room))
	return m.snapshotLocked(room), nil
}

// Clear empties one member's queue — the caller's own, or any queue when the
// caller is the host.
func (m *Manager) Clear(roomID, memberID, targetMemberID string) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}
	if _, ok := room.members[memberID]; !ok {
		return nil, ErrMemberNotFound
	}
	if targetMemberID == "" {
		targetMemberID = memberID
	}
	if targetMemberID != memberID && memberID != room.host {
		return nil, ErrForbidden
	}
	if _, ok := room.members[targetMemberID]; !ok {
		return nil, store.ErrNotFound
	}

	room.queues[targetMemberID] = []QueueItem{}
	recomputeMasterLocked(room)
	m.publishLocked(room, EventQueueUpdated, queueDataLocked(room))
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
		"by":         memberRefLocked(room, memberID),
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
		"by":         memberRefLocked(room, memberID),
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
		m.advanceLocked(context.Background(), room, "seek", memberID)
		return m.snapshotLocked(room), nil
	}
	m.publishLocked(room, EventSeeked, map[string]any{
		"positionMs": positionMs,
		"startedAt":  playback.startedAtMs,
		"item":       playback.item,
		"by":         memberRefLocked(room, memberID),
	})
	m.scheduleAdvanceLocked(room, playback)
	return m.snapshotLocked(room), nil
}

// Ended records that the host's copy of the current track has run out, and moves
// the room on.
//
// The host is the room's clock, so their file reaching its end is the song
// reaching its end - and their file is the one that decides when that is, not a
// number the room worked out in advance. Anybody else's file ending says
// nothing: they follow the host. The room's own timer stays as the backstop, for
// a host who has gone or whose client stopped listening without saying so.
func (m *Manager) Ended(roomID, memberID string) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}
	if _, ok := room.members[memberID]; !ok {
		return nil, ErrMemberNotFound
	}
	if memberID != room.host || room.current == nil || room.current.startedAtMs == 0 {
		// Not the room's clock, or nothing of the room's is playing: there is
		// nothing for this to mean.
		return m.snapshotLocked(room), nil
	}
	m.advanceLocked(context.Background(), room, "completed", memberID)
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
	m.advanceLocked(context.Background(), room, "skipped", memberID)
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
	m.advanceLocked(ctx, current, "votes", "")
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
	playback := room.playbackForTrack(trackID)
	if playback == nil {
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

	// A member may change which file they play, and a member may arrive: either
	// can change the host's copy, and so the end of the track.
	// Readiness for the song prepared behind this one is kept, not announced:
	// there is nothing for the room to show about a song it has not started.
	if playback != room.current {
		return m.snapshotLocked(room), nil
	}

	state := map[string]any{
		"ready":   len(playback.ready),
		"members": len(room.members),
		"item":    playback.item,
		"out":     outMembers(room),
	}
	if playback.startedAtMs != 0 {
		m.retimeLocked(room, playback)
		state["timelineMs"] = playback.timelineMs
	}

	m.publishLocked(room, EventReadyState, state)
	m.maybeStartLocked(room)
	return m.snapshotLocked(room), nil
}

// SetOut records that a member is sitting the room's track out, or is back in.
//
// The room plays for as long as the shortest file in it, so one member holding
// a short or broken copy would otherwise end the song for everybody. Sitting it
// out takes their file out of that reckoning and stops the room waiting on
// them, without leaving the room.
func (m *Manager) SetOut(roomID, memberID string, out bool) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}
	if _, ok := room.members[memberID]; !ok {
		return nil, ErrMemberNotFound
	}
	if out {
		room.out[memberID] = true
	} else {
		delete(room.out, memberID)
	}

	state := map[string]any{
		"members": len(room.members),
		"out":     outMembers(room),
	}
	playback := room.current
	if playback != nil {
		if out {
			// Their rendition is no longer the room's business, and the end of
			// the track may move without it.
			delete(playback.ready, memberID)
			delete(playback.variants, memberID)
			m.retimeLocked(room, playback)
		} else {
			// Back in: they need a rendition again, and the room counts them.
			go m.prepare(room.id, playback.item.ID)
		}
		state["ready"] = len(playback.ready)
		state["item"] = playback.item
	}

	m.publishLocked(room, EventReadyState, state)
	m.maybeStartLocked(room)
	return m.snapshotLocked(room), nil
}

// outMembers lists who is sitting this room's track out.
func outMembers(room *room) []string {
	out := make([]string, 0, len(room.out))
	for _, memberID := range room.order {
		if room.out[memberID] {
			out = append(out, memberID)
		}
	}
	return out
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

// checkQueueEditLocked enforces queue ownership: a member edits their own
// queue, the host may edit anyone's. Control policies govern playback, not
// what each member has queued.
func (m *Manager) checkQueueEditLocked(room *room, memberID, ownerID string) error {
	if _, ok := room.members[memberID]; !ok {
		return ErrMemberNotFound
	}
	if ownerID != memberID && memberID != room.host {
		return ErrForbidden
	}
	return nil
}

// recomputeMasterLocked rebuilds the play order: a perfectly fair
// round-robin across the member queues in join order — one item from each
// member per pass, each member's own order kept. With A=[a1 a2] and
// B=[b1 b2] the master queue is [a1 b1 a2 b2].
func recomputeMasterLocked(room *room) {
	total := 0
	for _, memberID := range room.order {
		total += len(room.queues[memberID])
	}
	master := make([]QueueItem, 0, total)
	for pass := 0; len(master) < total; pass++ {
		for _, memberID := range room.order {
			items := room.queues[memberID]
			if pass < len(items) {
				master = append(master, items[pass])
			}
		}
	}
	room.master = master
}

// dropItemLocked removes one played item from its owner's queue and from the
// play order. The remaining order keeps its round-robin phase — the master is
// only recomputed when members edit their queues, so the room plays one track
// per member per pass even as queues change.
func dropItemLocked(room *room, item QueueItem) {
	if items, ok := room.queues[item.AddedBy]; ok {
		for i, queued := range items {
			if queued.ID == item.ID {
				room.queues[item.AddedBy] = append(items[:i], items[i+1:]...)
				break
			}
		}
	}
	for i, queued := range room.master {
		if queued.ID == item.ID {
			room.master = append(room.master[:i], room.master[i+1:]...)
			break
		}
	}
}

// pendingLocked is the play order after the current track — the "queue" key
// existing clients already follow.
func pendingLocked(room *room) []QueueItem {
	pending := make([]QueueItem, 0, len(room.master))
	for _, item := range room.master {
		if room.current != nil && item.ID == room.current.item.ID {
			continue
		}
		pending = append(pending, item)
	}
	return pending
}

// queueDataLocked renders the queue state for queue_updated: every member's
// own queue and the fair master mix, for everyone.
func queueDataLocked(room *room) map[string]any {
	queues := make(map[string][]QueueItem, len(room.queues))
	for memberID, items := range room.queues {
		queues[memberID] = append([]QueueItem(nil), items...)
	}
	return map[string]any{
		"queues":      queues,
		"masterQueue": append([]QueueItem(nil), room.master...),
		"queue":       pendingLocked(room),
		"next":        nextItemLocked(room),
	}
}

// nextItemLocked is the song prepared behind the current one, reported so that
// clients fetch it while there is still time. Only while it is still what the
// room would play next: a queue that changed underneath it is not news.
func nextItemLocked(room *room) *QueueItem {
	if room.next == nil || len(room.master) < 2 || room.master[1].ID != room.next.item.ID {
		return nil
	}
	item := room.next.item
	return &item
}

// playbackFor finds the playback an item belongs to: the one playing, or the
// one prepared behind it.
func (r *room) playbackFor(itemID string) *playback {
	if r.current != nil && r.current.item.ID == itemID {
		return r.current
	}
	if r.next != nil && r.next.item.ID == itemID {
		return r.next
	}
	return nil
}

// playbackForTrack is the same by canonical track, which is what a readiness
// report names.
func (r *room) playbackForTrack(trackID uuid.UUID) *playback {
	if r.current != nil && r.current.item.TrackID == trackID {
		return r.current
	}
	if r.next != nil && r.next.item.TrackID == trackID {
		return r.next
	}
	return nil
}

// beginNextLocked moves the next master-queue item into preparation. The item
// keeps its place in its owner's queue and in the play order until it finishes,
// so the round-robin phase survives while the room works through the mix.
func (m *Manager) beginNextLocked(room *room) {
	m.stopTimersLocked(room)

	if len(room.master) == 0 {
		room.current = nil
		m.publishLocked(room, EventQueueUpdated, queueDataLocked(room))
		return
	}

	item := room.master[0]
	// A playback prepared while the last song played is this one: keep it, and
	// every readiness report its members have already made about it.
	playback := room.next
	if playback != nil && playback.item.ID != item.ID {
		playback = nil
	}
	if playback == nil {
		playback = m.newPlayback(item)
	}
	room.next = nil
	room.current = playback

	data := queueDataLocked(room)
	data["preparing"] = item
	m.publishLocked(room, EventQueueUpdated, data)

	if playback.prepared {
		// Assigned while the last song played. If the members are ready - which
		// is what preparing ahead is for - the room starts now instead of
		// waiting out a timeout for files it already has.
		m.maybeStartLocked(room)
	} else {
		// Variant assignment needs the providers (and possibly the network), so
		// it happens off the lock.
		go m.prepare(room.id, item.ID)
	}

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
		playback.timedOut = true
		m.maybeStartLocked(current)
	})
}

// prepare resolves a playable rendition per member and announces the
// assignments. It runs outside the manager lock.
func (m *Manager) prepare(roomID, itemID string) {
	ctx, cancel := context.WithTimeout(context.Background(), prepareTimeout)
	defer cancel()

	m.mu.Lock()
	room, ok := m.rooms[roomID]
	if !ok {
		m.mu.Unlock()
		return
	}
	target := room.playbackFor(itemID)
	if target == nil {
		m.mu.Unlock()
		return
	}
	trackID := target.item.TrackID
	members := make([]Member, 0, len(room.order))
	for _, memberID := range room.order {
		if member, ok := room.members[memberID]; ok {
			member := *member
			member.Out = room.out[memberID]
			members = append(members, member)
		}
	}
	m.mu.Unlock()

	variants, err := m.matcher.Resolve(ctx, trackID)
	if err != nil {
		m.logger.Warn("room: no playable rendition", "room", roomID, "track", trackID, "error", err)
	}

	assignments := make(map[string]uuid.UUID, len(members))
	durations := make(map[uuid.UUID]int64, len(variants))
	for _, variant := range variants {
		if variant.DurationMs > 0 {
			durations[variant.ID] = variant.DurationMs
		}
	}
	for _, member := range members {
		if room.out[member.ID] {
			continue // no rendition to fetch: they are not playing this one
		}
		if variant, ok := m.pickVariant(ctx, member, variants); ok {
			assignments[member.ID] = variant.ID
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	room, ok = m.rooms[roomID]
	if !ok {
		return
	}
	target = room.playbackFor(itemID)
	if target == nil {
		return
	}
	target.variants = assignments
	target.durations = durations
	target.prepared = true
	// Fetch what this song will be played from, now that it is known. For the
	// song prepared behind the current one that is the whole point: the file is
	// here before the advance, whoever is listening and whatever their client
	// knows how to do.
	m.warmLocked(target)
	if target == room.current {
		// Someone who arrived mid-track has a file the room has not reckoned
		// with, and the end of the song moves.
		m.retimeLocked(room, target)
	}
	m.publishLocked(room, EventTrackPrepared, map[string]any{
		"item":     target.item,
		"variants": assignments,
	})
	m.maybeStartLocked(room)
}

// pickVariant chooses the rendition for one member: their effective provider
// ranking decides, exactly as it does for the CLI.
func (m *Manager) pickVariant(ctx context.Context, member Member, variants []store.Variant) (store.Variant, bool) {
	order := m.cfg.DefaultProviderOrder
	caller := uuid.Nil
	if member.UserID != nil {
		caller = *member.UserID
		if own, err := m.ranking.User(ctx, *member.UserID); err == nil {
			order = own
		} else {
			m.logger.Warn("room: ranking lookup failed", "member", member.ID, "error", err)
		}
	} else if aggregate, err := m.ranking.Aggregate(ctx); err == nil {
		order = aggregate
	}
	// The reserved slots name a rendition rather than a provider, so they are
	// resolved against this track's uploads before the order can decide. The
	// room assigns the same rendition the member's own client would pick.
	if len(variants) == 0 {
		return store.Variant{}, false
	}
	trackID := variants[0].TrackID
	uploaders, err := m.store.VariantUploaders(ctx, trackID)
	if err != nil {
		m.logger.Warn("room: uploaders lookup failed", "track", trackID, "error", err)
	}
	counts, err := m.store.VariantVoteCounts(ctx, trackID)
	if err != nil {
		m.logger.Warn("room: vote count lookup failed", "track", trackID, "error", err)
	}
	for _, variant := range ranking.OrderVariants(order, variants, uploaders, counts, caller) {
		if variant.Downloadable {
			return variant, true
		}
	}
	return ranking.Pick(order, variants)
}

// timelineFor is how long the room plays the current track: as long as the
// host's copy of it. The host is the room's clock - the song starts when their
// file is here and runs as long as their file lasts - so everyone else follows
// them, and a member holding a longer copy is cut or a shorter one sits quiet
// rather than either of them moving the room.
//
// A host whose length is not known yet falls back to the shortest file in the
// room, which is what the whole room used to run on. A member sitting the song
// out is not counted either way.
func timelineFor(room *room, playback *playback) int64 {
	if duration := durationFor(room, playback, room.host); duration > 0 {
		return duration
	}
	shortest := int64(0)
	for memberID := range room.members {
		duration := durationFor(room, playback, memberID)
		if duration > 0 && (shortest == 0 || duration < shortest) {
			shortest = duration
		}
	}
	if shortest > 0 {
		return shortest
	}
	if playback.fallbackMs > 0 {
		return playback.fallbackMs
	}
	return defaultTimelineMs
}

// durationFor is how long one member's copy of the current track is: what they
// reported, else what the rendition the room handed them says. Zero for a
// member who is sitting this one out, or one the room knows nothing about.
func durationFor(room *room, playback *playback, memberID string) int64 {
	if memberID == "" || room.out[memberID] {
		return 0
	}
	if report, ok := playback.ready[memberID]; ok && report.DurationMs > 0 {
		return report.DurationMs
	}
	return playback.durations[playback.variants[memberID]]
}

// retimeLocked moves the end of the current track if the host's copy of it has
// changed, and tells everyone. The timer that advances the room moves with it:
// an end that is only written down is an end nobody acts on.
func (m *Manager) retimeLocked(room *room, playback *playback) {
	if playback == nil || playback.startedAtMs == 0 {
		return
	}
	timeline := timelineFor(room, playback)
	if timeline == playback.timelineMs {
		return
	}
	playback.timelineMs = timeline
	m.scheduleAdvanceLocked(room, playback)
	m.publishLocked(room, EventReadyState, map[string]any{
		"ready":      len(playback.ready),
		"members":    len(room.members),
		"item":       playback.item,
		"timelineMs": timeline,
	})
}

// maybeStartLocked starts the current track as soon as the room can begin.
func (m *Manager) maybeStartLocked(room *room) {
	playback := room.current
	if playback == nil || playback.startedAtMs != 0 || !playback.prepared {
		return
	}
	if !m.quorumReadyLocked(room, playback) {
		return
	}
	m.startLocked(room, playback)
}

// hostLeadsLocked reports whether the host is the one the room waits for: they
// are still here, and they are not sitting this song out.
func (m *Manager) hostLeadsLocked(room *room) bool {
	host := room.host
	if host == "" || room.out[host] {
		return false
	}
	_, ok := room.members[host]
	return ok
}

// quorumReadyLocked reports whether the room can begin.
//
// The host's file is what the room is waiting for. They are the room's clock -
// the song starts when their copy is here and runs as long as it lasts - so
// waiting for anybody else would hold the host up for a member who is going to
// follow them anyway. A member still fetching joins wherever the song has got
// to, which is what the timeline is for.
//
// With the host gone, or sitting this one out, there is nobody to wait for and
// the room as a whole decides again - and there the window is the backstop, for
// a room that would otherwise wait on somebody who may never arrive.
func (m *Manager) quorumReadyLocked(room *room, playback *playback) bool {
	if m.hostLeadsLocked(room) {
		// No window while the host leads: their file is what starts the song,
		// and starting without them would be the room playing to somebody who
		// cannot hear it yet. A host who cannot fetch it sits the song out,
		// which is what hands the room back to the room.
		_, ready := playback.ready[room.host]
		return ready
	}
	if playback.timedOut {
		return true
	}
	playing, ready := 0, 0
	for memberID := range room.members {
		if room.out[memberID] {
			continue
		}
		playing++
		if _, ok := playback.ready[memberID]; ok {
			ready++
		}
	}
	if playing == 0 {
		return true
	}
	return float64(ready) >= float64(playing)*m.cfg.ListenTogether.ReadyFraction
}

// waitedForLocked reports whether the room's start is still waiting on this
// member: the host alone while the host is here, and the room as a whole when
// there is no host to lead it. What the room says it is waiting for is what it
// is really waiting for.
func (m *Manager) waitedForLocked(room *room, memberID string) bool {
	if !m.hostLeadsLocked(room) {
		return true
	}
	return memberID == room.host
}

// startLocked fixes the timeline and the start instant, then announces
// track_started. Everyone who was not ready is catching up.
func (m *Manager) startLocked(room *room, playback *playback) {
	if playback == nil || playback.startedAtMs != 0 || room.current != playback {
		return
	}

	timeline := timelineFor(room, playback)
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
	m.prepareNextLocked(room)
}

// newPlayback is a playback waiting for its renditions: the room's clock
// starts at zero and the assignment fills in the rest.
func (m *Manager) newPlayback(item QueueItem) *playback {
	playback := &playback{
		item:     item,
		variants: map[string]uuid.UUID{},
		ready:    map[string]ReadyReport{},
		votes:    map[string]int{},
	}
	if track, err := m.store.Track(context.Background(), item.TrackID); err == nil {
		playback.fallbackMs = track.DurationMs
	}
	return playback
}

// prepareNextLocked builds the playback for the song after this one, without
// starting it. Its renditions are assigned now, and members may report ready
// for it, so that the advance has nothing left to wait for: a download that
// would otherwise begin at the end of the song happens during it instead.
func (m *Manager) prepareNextLocked(room *room) {
	if len(room.master) < 2 {
		room.next = nil
		return
	}
	item := room.master[1]
	if room.next != nil && room.next.item.ID == item.ID {
		return
	}
	room.next = m.newPlayback(item)
	go m.prepare(room.id, item.ID)
	m.publishLocked(room, EventQueueUpdated, queueDataLocked(room))
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
		m.advanceLocked(context.Background(), current, "completed", "")
	})
}

// memberRefLocked names a member for an event: the id to key on and the name
// to show. The room's own decisions - a vote, a track running out - carry no
// member, so the reference is nil and clients read it as "the room".
func memberRefLocked(room *room, memberID string) map[string]any {
	member, ok := room.members[memberID]
	if !ok {
		return nil
	}
	return map[string]any{"id": member.ID, "name": member.Name}
}

// advanceLocked ends the current track and prepares the next one.
func (m *Manager) advanceLocked(ctx context.Context, room *room, reason string, by string) {
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
				"by":         memberRefLocked(room, by),
			})
		}
		// The played item is done: it leaves its owner's queue and the play
		// order, which keeps its phase rather than being rebuilt.
		dropItemLocked(room, playback.item)
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

	queues := make(map[string][]QueueItem, len(room.queues))
	for memberID, items := range room.queues {
		queues[memberID] = append([]QueueItem(nil), items...)
	}
	snapshot := &Snapshot{
		ID:          room.id,
		Name:        room.name,
		Host:        room.host,
		Controls:    room.controls,
		CreatedAtMs: room.createdAtMs,
		Members:     make([]Member, 0, len(room.order)),
		MemberCount: len(room.members),
		HasPassword: room.password != "",
		Queues:      queues,
		MasterQueue: append([]QueueItem(nil), room.master...),
		Queue:       pendingLocked(room),
		ServerNowMs: nowMs,
		Skip: SkipRules{
			SkipThreshold:        m.cfg.ListenTogether.SkipThreshold,
			MinVotersForSkip:     m.cfg.ListenTogether.MinVotersForSkip,
			VoterFractionForSkip: m.cfg.ListenTogether.VoterFractionForSkip,
			ReadyFraction:        m.cfg.ListenTogether.ReadyFraction,
			ReadyTimeoutSeconds:  m.cfg.ListenTogether.ReadyTimeoutSeconds,
		},
	}
	for _, memberID := range room.order {
		if member, ok := room.members[memberID]; ok {
			member := *member
			member.Out = room.out[memberID]
			snapshot.Members = append(snapshot.Members, member)
		}
	}
	snapshot.Next = nextItemLocked(room)

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
		Prepared:    playback.prepared,
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
		// A member sitting the song out is not waiting for anything, and is not
		// catching up to anything either.
		if room.out[memberID] {
			continue
		}
		if playback.startedAtMs == 0 {
			if m.waitedForLocked(room, memberID) {
				view.Awaiting = append(view.Awaiting, memberID)
			}
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
