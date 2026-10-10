package rooms

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/config"
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
	// IconVersion changes when that picture does, so the panel fetches the new
	// one instead of the copy its browser has been holding.
	IconVersion int `json:"iconVersion,omitempty"`
}

// QueueItem is one entry of a member's queue: the row the queue lists and the
// song the room plays.
type QueueItem struct {
	ID         string    `json:"id"`
	TrackID    uuid.UUID `json:"trackId"`
	Title      string    `json:"title"`
	AddedBy    string    `json:"addedBy"`
	AddedAtMs  int64     `json:"addedAtMs"`
	DurationMs int64     `json:"durationMs,omitempty"`
	// ArtworkURL is a path on this server, empty when nothing is known yet. The
	// queue list shows it, and without it every row is a blank square.
	ArtworkURL string `json:"artworkUrl,omitempty"`
	// ArtistIDs lets a client open an artist's page from a queue row's menu:
	// the names alone cannot name a page.
	ArtistIDs []string `json:"artistIds,omitempty"`
}

// playback is the last host sync: the item, its position and whether the host
// is paused. The server stamps the position at receipt so clients can compare
// their player to it between the once-per-second syncs.
type playback struct {
	item       QueueItem
	votes      map[string]int
	anchor     int64
	atMs       int64
	started    bool
	paused     bool
	durationMs int64
}

// positionMs is where the song is at server time nowMs.
func (p *playback) positionMs(nowMs int64) int64 {
	if !p.started || p.paused {
		return p.anchor
	}
	position := p.anchor + (nowMs - p.atMs)
	if position < 0 {
		return 0
	}
	return position
}

// room is the mutable room state; every access happens under Manager.mu.
type room struct {
	id          string
	name        string
	host        string
	controls    Controls
	password    string // join password, empty for none; never leaves the manager
	createdAtMs int64
	// owner is who the room belongs to, in a form that outlives a session: the
	// account's id when they have one, else the browser's member id. The host
	// is whoever is leading right now; the owner is who leads again when they
	// come back.
	owner     string
	ownerName string

	members map[string]*Member
	order   []string

	// queues holds each member's own queue, including the item in flight until
	// it finishes; master is the fair play order derived from it. The current
	// item keeps its place in both until the room moves off it, so the
	// round-robin phase survives while the room works through the mix.
	queues  map[string][]QueueItem
	master  []QueueItem
	current *playback

	// serverDriven is true while the host has no live socket and the server is
	// therefore the room's clock: the room keeps time and moves on by itself
	// until the host comes back. It is derived (reticLocked) rather than set by
	// hand, so only one place decides who drives.
	serverDriven bool
	// endTimer fires when the server, driving a song, reaches its end; idleTimer
	// closes a room that has stopped making progress. Both are re-armed from
	// scratch after every change.
	endTimer  Timer
	idleTimer Timer
	// retic guards the publish funnel: reticLocked publishes the driver change,
	// and that publish must not re-enter it.
	retic bool
	// closed marks a room that has been forgotten; nothing more is scheduled for
	// it and the clock re-read leaves it alone.
	closed bool

	// seq counts every event the room has published; a client that sees a gap
	// refetches the snapshot.
	seq int64
}

// Who is running a room's clock. The host's own player is the clock while the
// host has a live socket; otherwise the server keeps time.
const (
	DrivenByHost   = "host"
	DrivenByServer = "server"
)

// PlaybackView is the current song's state. PositionMs is the position at AtMs
// on the server clock; a client computes where the room should be with
// position + (serverNow - atMs) while Started is true and Paused is false.
type PlaybackView struct {
	Item       QueueItem `json:"item"`
	PositionMs int64     `json:"positionMs"`
	AtMs       int64     `json:"atMs"`
	Started    bool      `json:"started"`
	Paused     bool      `json:"paused"`
	DurationMs int64     `json:"durationMs"`
	// DrivenBy is "host" while the host's player is the room's clock and
	// "server" while the server keeps time because the host is away.
	DrivenBy  string         `json:"drivenBy"`
	Votes     map[string]int `json:"votes"`
	MeanScore float64        `json:"meanScore"`
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
	Current     *PlaybackView          `json:"current,omitempty"`
	ServerNowMs int64                  `json:"serverNowMs"`
	Seq         int64                  `json:"seq"`
	Skip        SkipRules              `json:"skip"`
}

// SkipRules are the tunables clients show next to the vote buttons.
type SkipRules struct {
	SkipThreshold        float64 `json:"skipThreshold"`
	MinVotersForSkip     int     `json:"minVotersForSkip"`
	VoterFractionForSkip float64 `json:"voterFractionForSkip"`
}

// Store is the repository slice rooms need.
type Store interface {
	store.TrackRepo
	store.VoteRepo
}

// Manager owns every room.
type Manager struct {
	mu  sync.Mutex
	cfg *config.Config
	// rooms holds every open room, oldest first in roomOrder.
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

	// grace is how long a member's rooms are kept after their last event
	// socket closes. It is a field so tests can shrink it; production uses
	// socketGrace.
	grace time.Duration
	// idleAfter is how long a room may go without making progress before it
	// closes itself. A field for the same reason as grace.
	idleAfter time.Duration

	logger *slog.Logger
	bus    *Bus
	clock  Clock
	store  Store
}

// socketGrace is how long a member's rooms are kept after their last event
// socket closes. Long enough that a client reconnecting through a dropped
// socket is not taken out of the room it is still listening to.
const socketGrace = 10 * time.Second

// idleRoomAfter is how long a room that is not making progress is left open: no
// song, the song paused, or a server-driven room with nothing left to play. A
// room with nothing playing in it and nobody to come back to it is one that
// should not sit there for ever.
const idleRoomAfter = 2 * time.Hour

// NewManager returns a room manager. The store is what queue rows read their
// titles and artwork from, and what votes are kept in.
func NewManager(cfg *config.Config, st Store, logger *slog.Logger) *Manager {
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
		grace:       socketGrace,
		idleAfter:   idleRoomAfter,
	}
}

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
	m.reticMemberLocked(memberID)
}

// Disconnect drops one of a member's event sockets, and takes them out of every
// room they were in once it was the last one and the grace has passed.
//
// A tab that closes is a listener that has gone: without this, a member who shut
// the window sits in the room for ever. A second tab on the same account keeps
// them there, and so does a client that is only reconnecting - only the last
// socket, staying closed, is a leave.
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
	m.leaving[memberID] = time.AfterFunc(m.grace, func() { m.leaveEveryRoom(memberID) })
	// A host whose socket just went is a room that has to keep its own time.
	m.reticMemberLocked(memberID)
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
	for _, id := range append([]string(nil), m.roomOrder...) {
		room, ok := m.rooms[id]
		if !ok {
			continue
		}
		if _, member := room.members[memberID]; member {
			m.leaveLocked(room, memberID)
		}
	}
}

// Bus exposes the event stream.
func (m *Manager) Bus() *Bus { return m.bus }

// Subscribe returns a channel of room events and a cancel function.
func (m *Manager) Subscribe() (<-chan Event, func()) { return m.bus.Subscribe() }

// Close stops the pending leave timers. Room state is in memory; nothing is
// persisted.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for memberID, timer := range m.leaving {
		timer.Stop()
		delete(m.leaving, memberID)
	}
	for _, room := range m.rooms {
		m.stopTimer(&room.endTimer)
		m.stopTimer(&room.idleTimer)
	}
}

// SetClock replaces the clock; it exists for tests that hold time still.
func (m *Manager) SetClock(c Clock) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clock = c
}

// NowMs is the server clock rooms stamp state with.
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
// here, and otherwise whoever has been in the room longest.
func (m *Manager) promoteHostLocked(room *room) {
	next := ""
	for _, memberID := range room.order {
		member, ok := room.members[memberID]
		if !ok {
			continue
		}
		if next == "" {
			next = memberID
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
// member arriving while a song is playing follows it like everybody else; there
// is nothing to prepare and nothing to wait for. The password must match when
// the room has one.
//
// `supersedes` names a member this one replaces: a browser that joined as a
// guest and has since signed in, whose account is a different member id. What
// that member had queued comes across - it was the same person - and the
// membership it goes, so the room does not show somebody twice.
func (m *Manager) Join(roomID string, member Member, password, supersedes string) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}
	if room.password != "" && room.password != password {
		return nil, ErrWrongPassword
	}
	_, known := room.members[member.ID]
	if !known {
		room.order = append(room.order, member.ID)
		room.queues[member.ID] = []QueueItem{}
	}
	if supersedes != "" && supersedes != member.ID {
		if _, wasThere := room.members[supersedes]; wasThere {
			if len(room.queues[member.ID]) == 0 {
				room.queues[member.ID] = append([]QueueItem(nil), room.queues[supersedes]...)
			}
			// The member being replaced is the same person: if they were leading
			// the room, the id taking their place leads it now.
			wasHost := room.host == supersedes
			delete(room.members, supersedes)
			delete(room.queues, supersedes)
			room.order = withoutMember(room.order, supersedes)
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
	recomputeMasterLocked(room)

	m.publishLocked(room, EventMemberJoined, map[string]any{"member": member, "memberCount": len(room.members)})
	if !known || supersedes != "" {
		m.publishLocked(room, EventQueueUpdated, queueDataLocked(room, member.ID))
	}
	return m.snapshotLocked(room), nil
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
		delete(room.current.votes, memberID)
	}
	m.publishLocked(room, EventMemberLeft, map[string]any{"memberId": memberID, "memberCount": len(room.members)})
	m.publishLocked(room, EventQueueUpdated, queueDataLocked(room, memberID))

	if len(room.members) == 0 {
		// The last member leaving closes the room; that is still a successful
		// leave, so callers are not told the room was missing.
		m.closeRoomLocked(room)
		return nil
	}
	if room.host == memberID {
		m.promoteHostLocked(room)
	}
	return m.snapshotLocked(room)
}

// queueItemLocked builds the row a track takes in a member's queue: the title,
// length and artwork the list shows, and the artist ids its menu opens.
func (m *Manager) queueItemLocked(ctx context.Context, memberID string, track *store.Track) QueueItem {
	artwork := track.ArtworkURL
	if artwork == "" {
		if url, err := m.store.TrackArtwork(ctx, track.ID); err == nil {
			artwork = url
		}
	}
	var artistIDs []string
	if artists, err := m.store.TrackArtists(ctx, track.ID); err == nil {
		artistIDs = make([]string, 0, len(artists))
		for _, artist := range artists {
			artistIDs = append(artistIDs, artist.ID.String())
		}
	}
	return QueueItem{
		ID:         uuid.NewString(),
		TrackID:    track.ID,
		Title:      track.Title,
		AddedBy:    memberID,
		AddedAtMs:  m.nowMsLocked(),
		DurationMs: track.DurationMs,
		ArtworkURL: artwork,
		ArtistIDs:  artistIDs,
	}
}

// recomputeMasterLocked rebuilds the play order: a perfectly fair round-robin
// across the member queues in join order — one item from each member per pass,
// each member's own order kept. With A=[a1 a2] and B=[b1 b2] the master queue
// is [a1 b1 a2 b2].
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

// Enqueue appends a track to the member's queue. The host starts it by syncing
// the first item from the generated mix.
func (m *Manager) Enqueue(ctx context.Context, roomID, memberID string, trackID uuid.UUID) (*Snapshot, error) {
	return m.EnqueueMany(ctx, roomID, memberID, []uuid.UUID{trackID})
}

// EnqueueMany appends a run of tracks to the member's own queue as one edit.
// A playlist added to a room is one change, not one per song: each song on its
// own costs a lock, a recompute of the mix, a queue event and a room snapshot
// back. The batch is all or nothing - an unknown track leaves the queue
// untouched.
func (m *Manager) EnqueueMany(ctx context.Context, roomID, memberID string, trackIDs []uuid.UUID) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return nil, err
	}
	if _, ok := room.members[memberID]; !ok {
		return nil, ErrMemberNotFound
	}
	if len(trackIDs) == 0 {
		return m.snapshotLocked(room), nil
	}
	items := make([]QueueItem, 0, len(trackIDs))
	for _, trackID := range trackIDs {
		track, err := m.store.Track(ctx, trackID)
		if err != nil {
			return nil, err
		}
		items = append(items, m.queueItemLocked(ctx, memberID, track))
	}
	room.queues[memberID] = append(room.queues[memberID], items...)
	recomputeMasterLocked(room)
	m.publishLocked(room, EventQueueUpdated, queueDataLocked(room, memberID))
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
			m.publishLocked(room, EventQueueUpdated, queueDataLocked(room, owner))
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
	m.publishLocked(room, EventQueueUpdated, queueDataLocked(room, owner))
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
	m.publishLocked(room, EventQueueUpdated, queueDataLocked(room, targetMemberID))
	return m.snapshotLocked(room), nil
}

// SyncState is one report from the host's ordinary queue player. ItemID names
// the queue entry (not only its canonical track, which may be queued twice).
type SyncState struct {
	ItemID     string
	TrackID    uuid.UUID
	PositionMs int64
	DurationMs int64
	Started    bool
	Paused     bool
}

// beginLocked puts up the first queued item when there is no current one. The
// host's next sync starts it; explicit skips and votes also use this path.
func (m *Manager) beginLocked(room *room) {
	if room.current != nil || len(room.master) == 0 {
		return
	}
	item := room.master[0]
	room.current = &playback{
		item:       item,
		votes:      map[string]int{},
		anchor:     0,
		atMs:       m.nowMsLocked(),
		durationMs: item.DurationMs,
		paused:     true,
	}
	m.publishLocked(room, EventPlayback, m.playbackDataLocked(room))
}

func (m *Manager) advanceLocked(room *room, reason, by string) {
	if room.current == nil {
		return
	}
	m.logger.Info("room: advance requested", "room", room.id, "reason", reason, "by", by,
		"track", room.current.item.TrackID, "position_ms", room.current.positionMs(m.nowMsLocked()))
	dropItemLocked(room, room.current.item)
	room.current = nil
	m.beginLocked(room)
	if room.current == nil {
		m.publishLocked(room, EventPlayback, m.playbackDataLocked(room))
	}
}

// reticLocked re-reads a room's clock after a change: who drives it, when the
// song the server is driving ends, and whether the room has stopped making
// progress. Every deadline is re-armed from scratch rather than adjusted, so one
// place decides what is scheduled.
//
// It publishes when the driver changes, and that publish comes back here, so the
// guard is what keeps one re-read from arming two sets of deadlines and
// orphaning the first.
func (m *Manager) reticLocked(room *room) {
	if room.closed || room.retic {
		return
	}
	room.retic = true
	defer func() { room.retic = false }()
	m.stopTimer(&room.endTimer)
	m.stopTimer(&room.idleTimer)

	driven := !m.hostPresentLocked(room)
	if driven != room.serverDriven {
		room.serverDriven = driven
		// Whose clock it is is part of what a client needs: a follower stops
		// waiting for a host that is not there.
		m.publishLocked(room, EventPlayback, m.playbackDataLocked(room))
	}

	current := room.current
	playing := current != nil && current.started && !current.paused
	timed := false
	if driven && playing && current.durationMs > 0 && m.anyoneListeningLocked(room) {
		remaining := current.durationMs - current.positionMs(m.nowMsLocked())
		if remaining < 1 {
			remaining = 1
		}
		timed = true
		itemID := current.item.ID
		room.endTimer = m.clock.AfterFunc(time.Duration(remaining)*time.Millisecond, func() {
			m.endSong(room.id, itemID)
		})
	}
	// A room that is not playing, or one the server has no way to move on
	// (nothing listening, or a length it never learned), is going nowhere: it
	// closes once it has been that way for the idle window.
	if !playing || (driven && !timed) {
		room.idleTimer = m.clock.AfterFunc(m.idleAfter, func() { m.closeIdle(room.id) })
	}
}

// hostPresentLocked reports whether the host has a live event socket. The server
// drives exactly while it does not: the host's player is the room's clock while
// the host is there to report it, and the room keeps its own time otherwise.
func (m *Manager) hostPresentLocked(room *room) bool {
	return m.connections[room.host] > 0
}

// anyoneListeningLocked reports whether any member of the room has a live event
// socket. A room nobody is listening to is not moved on: the queue belongs to
// the people in the room, and playing it to an empty room would only spend it.
func (m *Manager) anyoneListeningLocked(room *room) bool {
	for memberID := range room.members {
		if m.connections[memberID] > 0 {
			return true
		}
	}
	return false
}

// reticMemberLocked re-reads every room a member is in: their socket opening or
// closing is what makes a room server-driven, and what stops it being.
func (m *Manager) reticMemberLocked(memberID string) {
	for _, id := range m.roomOrder {
		room, ok := m.rooms[id]
		if !ok {
			continue
		}
		if _, member := room.members[memberID]; member {
			m.reticLocked(room)
		}
	}
}

// endSong is the server's clock reaching the end of the song the host last
// reported. It only acts while the host is still away and the room is still on
// that song: a host that came back, or a room that already moved on, owns the
// answer instead.
func (m *Manager) endSong(roomID, itemID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return
	}
	current := room.current
	if current == nil || current.item.ID != itemID || m.hostPresentLocked(room) {
		return
	}
	m.logger.Info("room: the server is moving the room on", "room", room.id,
		"track", current.item.TrackID, "reason", "host away")
	m.advanceLocked(room, "server", "")
	m.startServerDrivenLocked(room)
	m.reticLocked(room)
}

// startServerDrivenLocked begins the song a server-driven room is on: with no
// host to report a player, the next song starts when the last one ends.
func (m *Manager) startServerDrivenLocked(room *room) {
	current := room.current
	if current == nil || m.hostPresentLocked(room) {
		return
	}
	current.started = true
	current.paused = false
	current.anchor = 0
	current.atMs = m.nowMsLocked()
	m.publishLocked(room, EventPlayback, m.playbackDataLocked(room))
}

// closeIdle closes a room that has been going nowhere for the idle window. The
// room is looked at again here: a song that began since the timer was armed is a
// room worth keeping.
func (m *Manager) closeIdle(roomID string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, ok := m.rooms[roomID]
	if !ok {
		return
	}
	current := room.current
	if current != nil && current.started && !current.paused {
		return
	}
	m.logger.Info("room: closing an idle room", "room", room.id, "name", room.name,
		"idle_for", m.idleAfter.String())
	m.closeRoomLocked(room)
}

// stopTimer cancels a room deadline and clears the field holding it.
func (m *Manager) stopTimer(timer *Timer) {
	if *timer == nil {
		return
	}
	(*timer).Stop()
	*timer = nil
}

// Sync records the host's current entry, position and pause state. The host's
// normal queue player advances itself; this mirrors its state to the room and
// followers. Hosts send one sync per second, and immediately on player changes.
func (m *Manager) Sync(roomID, memberID string, update SyncState) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	room, err := m.roomLocked(roomID)
	if err != nil {
		return err
	}
	if _, ok := room.members[memberID]; !ok {
		return ErrMemberNotFound
	}
	if memberID != room.host {
		return ErrForbidden
	}

	if update.ItemID == "" {
		changed := room.current != nil
		if room.current != nil {
			dropItemLocked(room, room.current.item)
			room.current = nil
		}
		if len(room.master) > 0 {
			m.beginLocked(room)
		} else if changed {
			m.publishLocked(room, EventPlayback, m.playbackDataLocked(room))
		}
		return nil
	}
	if update.PositionMs < 0 {
		update.PositionMs = 0
	}

	var item QueueItem
	newItem := room.current == nil || room.current.item.ID != update.ItemID
	if !newItem {
		item = room.current.item
		if update.TrackID != uuid.Nil && item.TrackID != update.TrackID {
			return ErrInvalidOrder
		}
	} else {
		// Host queue and room mix use the same order: after the old current is
		// spliced out, the host's new entry must be the first pending item.
		var pending []QueueItem
		for _, queued := range room.master {
			if room.current == nil || queued.ID != room.current.item.ID {
				pending = append(pending, queued)
			}
		}
		if len(pending) == 0 || pending[0].ID != update.ItemID {
			m.logger.Warn("room: host sync is not the next mixed item", "room", room.id,
				"item", update.ItemID, "current", room.current)
			m.publishLocked(room, EventPlayback, m.playbackDataLocked(room))
			return nil
		}
		item = pending[0]
		if update.TrackID != uuid.Nil && item.TrackID != update.TrackID {
			m.logger.Warn("room: host sync track does not match its queue item", "room", room.id,
				"item", update.ItemID, "track", update.TrackID, "queued_track", item.TrackID)
			m.publishLocked(room, EventPlayback, m.playbackDataLocked(room))
			return nil
		}
		if room.current != nil {
			dropItemLocked(room, room.current.item)
		}
		room.current = &playback{item: item, votes: map[string]int{}, durationMs: item.DurationMs}
	}

	current := room.current
	current.anchor = update.PositionMs
	current.atMs = m.nowMsLocked()
	if update.DurationMs > 0 {
		current.durationMs = update.DurationMs
	}
	current.started = current.started || update.Started
	current.paused = update.Paused

	m.publishLocked(room, EventPlayback, m.playbackDataLocked(room))
	return nil
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
	current := room.current
	if current == nil || !current.started || current.paused {
		return nil, ErrNoPlayback
	}
	current.anchor = current.positionMs(m.nowMsLocked())
	current.atMs = m.nowMsLocked()
	current.paused = true
	m.publishLocked(room, EventPlayback, m.playbackDataLocked(room))
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
	current := room.current
	if current == nil || (current.started && !current.paused) {
		return nil, ErrNoPlayback
	}
	current.atMs = m.nowMsLocked()
	current.started = true
	current.paused = false
	m.publishLocked(room, EventPlayback, m.playbackDataLocked(room))
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
	current := room.current
	if current == nil {
		return nil, ErrNoPlayback
	}

	// A seek is the one position move that leaves nothing else behind, so a
	// room whose songs keep ending early is a room somebody is seeking: say who
	// asked and where they put it.
	m.logger.Info("room: seek",
		"room", room.id, "by", memberID, "to_ms", positionMs,
		"was_ms", current.positionMs(m.nowMsLocked()), "duration_ms", current.durationMs)

	if current.durationMs > 0 && positionMs >= current.durationMs {
		// Seeking to the end is asking for the next song.
		m.advanceLocked(room, "seek", memberID)
		return m.snapshotLocked(room), nil
	}
	current.anchor = positionMs
	current.atMs = m.nowMsLocked()
	m.publishLocked(room, EventPlayback, m.playbackDataLocked(room))
	return m.snapshotLocked(room), nil
}

// Skip advances past the current song.
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
	m.advanceLocked(room, "skipped", memberID)
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
	current := room.current
	if current == nil {
		m.mu.Unlock()
		return nil, ErrNoPlayback
	}

	current.votes[memberID] = score
	skip := m.shouldSkipLocked(room, current)
	trackID := current.item.TrackID
	if skip {
		m.advanceLocked(room, "votes", memberID)
	} else {
		m.publishLocked(room, EventPlayback, m.playbackDataLocked(room))
	}
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
	return snapshot, nil
}

// shouldSkipLocked applies the skip rule: enough voters, enough of the room,
// and a mean below the threshold.
func (m *Manager) shouldSkipLocked(room *room, current *playback) bool {
	rules := m.cfg.ListenTogether
	if len(current.votes) < rules.MinVotersForSkip {
		return false
	}
	if len(room.members) > 0 {
		fraction := float64(len(current.votes)) / float64(len(room.members))
		if fraction < rules.VoterFractionForSkip {
			return false
		}
	}
	return meanScore(current.votes) < rules.SkipThreshold
}

func (m *Manager) persistVote(ctx context.Context, vote store.Vote) {
	if err := m.store.SaveVote(ctx, &vote); err != nil {
		m.logger.Warn("room: persist vote", "room", vote.RoomID, "track", vote.TrackID, "error", err)
	}
}

// roomLocked finds an open room.
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

// queueDataLocked renders what a queue edit changed: the edited member's own
// queue. The play order is a function of the member queues and the join order,
// which every client already has, so it is derived rather than sent.
func queueDataLocked(room *room, changed string) map[string]any {
	data := map[string]any{"memberId": changed}
	if _, present := room.members[changed]; !present {
		// They left: the client drops the queue rather than replacing it.
		data["gone"] = true
		return data
	}
	data["memberQueue"] = append([]QueueItem(nil), room.queues[changed]...)
	return data
}

// playbackDataLocked is the payload of a playback event: the whole current
// state, or a null current when the room has gone idle. Clients replace their
// current wholesale, so one event type carries start, pause, resume, seek,
// vote and advance.
func (m *Manager) playbackDataLocked(room *room) map[string]any {
	return map[string]any{"current": m.viewLocked(room, m.nowMsLocked())}
}

// closeRoomLocked forgets a room nobody is in.
func (m *Manager) closeRoomLocked(room *room) {
	delete(m.rooms, room.id)
	m.roomOrder = removeString(m.roomOrder, room.id)
	// A closed room schedules nothing: its deadlines are dropped here, and the
	// publish funnel skips it from now on.
	room.closed = true
	m.stopTimer(&room.endTimer)
	m.stopTimer(&room.idleTimer)
	m.publishLocked(room, EventRoomClosed, nil)
	m.logger.Info("room closed", "room", room.id)
}

// publishLocked announces a room change, then re-reads the room's clock.
//
// The clock depends on state any change can touch - who is connected, what is
// playing, how long it is - so it is re-read in the one function every change
// already passes through rather than at each call site, where a new mutation
// could quietly forget it and leave a room that never closes or never moves on.
func (m *Manager) publishLocked(room *room, eventType EventType, data any) {
	room.seq++
	m.bus.Publish(Event{
		Type:   eventType,
		RoomID: room.id,
		Seq:    room.seq,
		AtMs:   m.nowMsLocked(),
		Data:   data,
	})
	m.reticLocked(room)
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
		ServerNowMs: nowMs,
		Seq:         room.seq,
		Skip: SkipRules{
			SkipThreshold:        m.cfg.ListenTogether.SkipThreshold,
			MinVotersForSkip:     m.cfg.ListenTogether.MinVotersForSkip,
			VoterFractionForSkip: m.cfg.ListenTogether.VoterFractionForSkip,
		},
	}
	for _, memberID := range room.order {
		if member, ok := room.members[memberID]; ok {
			snapshot.Members = append(snapshot.Members, *member)
		}
	}
	snapshot.Current = m.viewLocked(room, nowMs)
	return snapshot
}

// viewLocked renders the current song, or nil when nothing is playing.
func (m *Manager) viewLocked(room *room, nowMs int64) *PlaybackView {
	current := room.current
	if current == nil {
		return nil
	}
	votes := make(map[string]int, len(current.votes))
	for memberID, score := range current.votes {
		votes[memberID] = score
	}
	driver := DrivenByHost
	if room.serverDriven {
		driver = DrivenByServer
	}
	return &PlaybackView{
		Item:       current.item,
		PositionMs: current.positionMs(nowMs),
		AtMs:       nowMs,
		Started:    current.started,
		Paused:     current.paused,
		DurationMs: current.durationMs,
		DrivenBy:   driver,
		Votes:      votes,
		MeanScore:  meanScore(current.votes),
	}
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
