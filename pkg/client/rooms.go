package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

// RoomMember is one participant in a room. UserID is empty for guests.
type RoomMember struct {
	ID         string `json:"id"`
	UserID     string `json:"userId,omitempty"`
	Name       string `json:"name"`
	JoinedAtMs int64  `json:"joinedAtMs"`
}

// QueueItem is one entry of a room's queue.
type QueueItem struct {
	ID        string `json:"id"`
	TrackID   string `json:"trackId"`
	Title     string `json:"title"`
	AddedBy   string `json:"addedBy"`
	AddedAtMs int64  `json:"addedAtMs"`
}

// ReadyReport is a member's readiness for a track.
type ReadyReport struct {
	MemberID   string `json:"memberId"`
	TrackID    string `json:"trackId"`
	VariantID  string `json:"variantId"`
	DurationMs int64  `json:"durationMs"`
	AtMs       int64  `json:"atMs"`
}

// RoomPlayback is the current track's room state. PositionMs is the position
// at the server time the snapshot carried.
type RoomPlayback struct {
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

// RoomSkipRules are the tunables shown next to the vote buttons.
type RoomSkipRules struct {
	SkipThreshold        float64 `json:"skipThreshold"`
	MinVotersForSkip     int     `json:"minVotersForSkip"`
	VoterFractionForSkip float64 `json:"voterFractionForSkip"`
	ReadyTimeoutSeconds  int     `json:"readyTimeoutSeconds"`
}

// Room is a Listen Together room.
type Room struct {
	ID          string        `json:"id"`
	Name        string        `json:"name"`
	Host        string        `json:"host"`
	Controls    string        `json:"controls"`
	CreatedAtMs int64         `json:"createdAtMs"`
	Members     []RoomMember  `json:"members"`
	Queue       []QueueItem   `json:"queue"`
	Current     *RoomPlayback `json:"current,omitempty"`
	ServerNowMs int64         `json:"serverNowMs"`
	Skip        RoomSkipRules `json:"skip"`
}

// RoomMemberID reports which member id this client joined as.
func (r *Room) HasMember(memberID string) bool {
	for _, member := range r.Members {
		if member.ID == memberID {
			return true
		}
	}
	return false
}

// RoomClient is a room seen through this client's membership.
type RoomClient struct {
	client   *Client
	RoomID   string
	MemberID string
}

// Client exposes the underlying client (for auth state and media helpers).
func (r *RoomClient) Client() *Client { return r.client }

type roomEnvelope struct {
	Room     *Room  `json:"room"`
	MemberID string `json:"memberId"`
}

// CreateRoom opens a room; the caller becomes its host.
func (c *Client) CreateRoom(ctx context.Context, name, controls string) (*RoomClient, error) {
	body := map[string]string{"name": name}
	if controls != "" {
		body["controls"] = controls
	}
	var out roomEnvelope
	if err := c.do(ctx, http.MethodPost, "/api/v1/rooms", body, &out); err != nil {
		return nil, err
	}
	if out.MemberID != "" {
		c.memberID = out.MemberID
	}
	return &RoomClient{client: c, RoomID: out.Room.ID, MemberID: out.MemberID}, nil
}

// JoinRoom joins a room, generating a guest identity when needed.
func (c *Client) JoinRoom(ctx context.Context, roomID string) (*RoomClient, error) {
	var out roomEnvelope
	if err := c.do(ctx, http.MethodPost, "/api/v1/rooms/"+roomID+"/join", nil, &out); err != nil {
		return nil, err
	}
	if out.MemberID != "" {
		c.memberID = out.MemberID
	}
	return &RoomClient{client: c, RoomID: out.Room.ID, MemberID: out.MemberID}, nil
}

// Rooms lists every room.
func (c *Client) Rooms(ctx context.Context) ([]Room, error) {
	var out struct {
		Rooms []Room `json:"rooms"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/rooms", nil, &out); err != nil {
		return nil, err
	}
	return out.Rooms, nil
}

// Snapshot fetches the room state.
func (r *RoomClient) Snapshot(ctx context.Context) (*Room, error) {
	var out Room
	if err := r.client.do(ctx, http.MethodGet, "/api/v1/rooms/"+r.RoomID, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Queue appends a track.
func (r *RoomClient) Queue(ctx context.Context, trackID string) (*Room, error) {
	return r.QueueMany(ctx, []string{trackID})
}

// MaxQueueBatch is how many tracks one add may carry. The server holds the room
// lock across a batch, so it refuses more; QueueMany splits longer runs.
const MaxQueueBatch = 500

// QueueMany appends a run of tracks as one edit: a playlist added to a room is
// one change, not one per song. Each song on its own is a round trip that
// answers with the whole room, so a run of them is a run of whole rooms.
func (r *RoomClient) QueueMany(ctx context.Context, trackIDs []string) (*Room, error) {
	if len(trackIDs) == 0 {
		return nil, errors.New("musoak: no tracks to queue")
	}
	var room *Room
	for start := 0; start < len(trackIDs); start += MaxQueueBatch {
		var out Room
		body := map[string][]string{"trackIds": trackIDs[start:min(start+MaxQueueBatch, len(trackIDs))]}
		if err := r.client.do(ctx, http.MethodPost, r.path("/queue"), body, &out); err != nil {
			return nil, err
		}
		room = &out
	}
	return room, nil
}

// Remove drops a queued item.
func (r *RoomClient) Remove(ctx context.Context, itemID string) (*Room, error) {
	var out Room
	if err := r.client.do(ctx, http.MethodDelete, r.path("/queue/"+itemID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Reorder rearranges the queue.
func (r *RoomClient) Reorder(ctx context.Context, itemIDs []string) (*Room, error) {
	var out Room
	body := map[string][]string{"itemIds": itemIDs}
	if err := r.client.do(ctx, http.MethodPost, r.path("/queue/reorder"), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Pause freezes the room.
func (r *RoomClient) Pause(ctx context.Context) (*Room, error) {
	return r.command(ctx, "/pause")
}

// Resume continues a paused room.
func (r *RoomClient) Resume(ctx context.Context) (*Room, error) {
	return r.command(ctx, "/resume")
}

// Skip advances past the current track.
func (r *RoomClient) Skip(ctx context.Context) (*Room, error) {
	return r.command(ctx, "/skip")
}

// Seek moves the room position; clients correct their drift with small seeks.
func (r *RoomClient) Seek(ctx context.Context, positionMs int64) (*Room, error) {
	var out Room
	body := map[string]int64{"positionMs": positionMs}
	if err := r.client.do(ctx, http.MethodPost, r.path("/seek"), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Vote scores the current track from 1 (bad) to 5 (great).
func (r *RoomClient) Vote(ctx context.Context, score int) (*Room, error) {
	var out Room
	body := map[string]int{"score": score}
	if err := r.client.do(ctx, http.MethodPost, r.path("/vote"), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Ready reports that this member has its rendition and how long it plays.
func (r *RoomClient) Ready(ctx context.Context, trackID, variantID string, durationMs int64) (*Room, error) {
	var out Room
	body := map[string]any{"trackId": trackID, "variantId": variantID, "durationMs": durationMs}
	if err := r.client.do(ctx, http.MethodPost, r.path("/ready"), body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Leave removes this member from the room.
func (r *RoomClient) Leave(ctx context.Context) error {
	return r.client.do(ctx, http.MethodPost, r.path("/leave"), nil, nil)
}

func (r *RoomClient) command(ctx context.Context, suffix string) (*Room, error) {
	var out Room
	if err := r.client.do(ctx, http.MethodPost, r.path(suffix), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (r *RoomClient) path(suffix string) string {
	return "/api/v1/rooms/" + r.RoomID + suffix
}

// Events subscribes to room events; pass an empty roomID for every room.
// The channel closes when ctx ends or the connection drops.
func (c *Client) Events(ctx context.Context, roomID string) (<-chan Event, error) {
	path := "/api/v1/ws"
	if roomID != "" {
		path += "?roomId=" + roomID
	}
	return c.streamEvents(ctx, path)
}

// Events subscribes to this room's event stream.
func (r *RoomClient) Events(ctx context.Context) (<-chan Event, error) {
	return r.client.Events(ctx, r.RoomID)
}

// Event is one message from the room event stream.
type Event struct {
	Type   string          `json:"type"`
	RoomID string          `json:"roomId,omitempty"`
	AtMs   int64           `json:"atMs,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
}

// Event types.
const (
	EventMemberJoined  = "member_joined"
	EventMemberLeft    = "member_left"
	EventQueueUpdated  = "queue_updated"
	EventTrackPrepared = "track_prepared"
	EventTrackStarted  = "track_started"
	EventTrackSkipped  = "track_skipped"
	EventPaused        = "paused"
	EventResumed       = "resumed"
	EventSeeked        = "seeked"
	EventVoteUpdated   = "vote_updated"
	EventReadyState    = "ready_state"
	EventRoomClosed    = "room_closed"
)

// DecodeData decodes the event payload into v.
func (e Event) DecodeData(v any) error {
	if len(e.Data) == 0 {
		return nil
	}
	return json.Unmarshal(e.Data, v)
}

// TrackPrepared is the payload of track_prepared: which variant each member
// was told to play.
type TrackPrepared struct {
	Item     QueueItem         `json:"item"`
	Variants map[string]string `json:"variants"`
}

// TrackStarted is the payload of track_started.
type TrackStarted struct {
	Item       QueueItem         `json:"item"`
	StartedAt  int64             `json:"startedAt"`
	TimelineMs int64             `json:"timelineMs"`
	Variants   map[string]string `json:"variants"`
}

// TrackSkipped is the payload of track_skipped.
type TrackSkipped struct {
	Item       QueueItem `json:"item"`
	Reason     string    `json:"reason"`
	PositionMs int64     `json:"positionMs"`
	Mean       float64   `json:"mean"`
}
