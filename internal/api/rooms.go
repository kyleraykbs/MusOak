package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/rooms"
	"codeberg.org/kyleraykbs/musoak/internal/store"
)

// memberHeader carries a guest's room member id. Authenticated users are
// identified by their account instead.
const memberHeader = "X-Member-Id"

type createRoomRequest struct {
	Name     string         `json:"name"`
	Controls rooms.Controls `json:"controls"`
	Password string         `json:"password"`
}

type joinRoomRequest struct {
	Password string `json:"password"`
}

type roomResponse struct {
	Room     *rooms.Snapshot `json:"room"`
	MemberID string          `json:"memberId"`
}

type queueRequest struct {
	TrackID string `json:"trackId"`
}

type reorderRequest struct {
	ItemIDs []string `json:"itemIds"`
}

type seekRequest struct {
	PositionMs int64 `json:"positionMs"`
}

type voteRequest struct {
	Score int `json:"score"`
}

type readyRequest struct {
	TrackID    string `json:"trackId"`
	VariantID  string `json:"variantId"`
	DurationMs int64  `json:"durationMs"`
}

// roomOutRequest is a member saying whether they are playing the room's track.
type roomOutRequest struct {
	Out bool `json:"out"`
}

// callerMember builds the member identity for a room command. Authenticated
// callers are their account; guests carry their member id in a header.
func (s *Server) callerMember(r *http.Request, allowNew bool) (rooms.Member, bool) {
	if user := s.currentUser(r); user != nil {
		id := user.ID
		return rooms.Member{
			ID: id.String(), UserID: &id, Name: memberName(user),
			IconURL: user.IconURL, IconVersion: user.IconVersion,
		}, true
	}
	if id := r.Header.Get(memberHeader); id != "" {
		return rooms.Member{ID: id, Name: r.Header.Get("X-Member-Name")}, true
	}
	if allowNew {
		return rooms.Member{ID: uuid.NewString(), Name: r.Header.Get("X-Member-Name")}, true
	}
	return rooms.Member{}, false
}

// guestMemberID is the member a browser joined as, when this request is that
// same browser arriving as its account instead. It is empty for a guest - who
// is its own member - and for a client that never carried a member id.
func guestMemberID(r *http.Request, member rooms.Member) string {
	if member.UserID == nil {
		return ""
	}
	id := r.Header.Get(memberHeader)
	if id == "" || id == member.ID {
		return ""
	}
	return id
}

// memberName is what a room shows for somebody: the display name they chose,
// which is the name people know them by, rather than the handle they sign in
// with.
func memberName(user *store.User) string {
	if name := strings.TrimSpace(user.DisplayName); name != "" {
		return name
	}
	return user.Username
}

// handleRoomCreate makes a room, with the caller as its host.
func (s *Server) handleRoomCreate(w http.ResponseWriter, r *http.Request) {
	var req createRoomRequest
	if r.ContentLength > 0 && !decodeJSON(w, r, &req) {
		return
	}
	member, ok := s.callerMember(r, true)
	if !ok {
		writeError(w, http.StatusBadRequest, "member identity required")
		return
	}
	room, err := s.rooms.Create(req.Name, req.Controls, req.Password, member)
	if err != nil {
		writeRoomError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, roomResponse{Room: room, MemberID: member.ID})
}

func (s *Server) handleRoomList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"rooms": s.rooms.List()})
}

func (s *Server) handleRoomGet(w http.ResponseWriter, r *http.Request) {
	room, err := s.rooms.Get(r.PathValue("roomId"))
	if err != nil {
		writeRoomError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

func (s *Server) handleRoomJoin(w http.ResponseWriter, r *http.Request) {
	var req joinRoomRequest
	if r.ContentLength > 0 && !decodeJSON(w, r, &req) {
		return
	}
	member, ok := s.callerMember(r, true)
	if !ok {
		writeError(w, http.StatusBadRequest, "member identity required")
		return
	}
	// A signed-in caller is its account, which is a different member id from the
	// one the browser joined as: name that one so the account takes its place
	// rather than sitting beside it.
	room, err := s.rooms.Join(r.PathValue("roomId"), member, req.Password, guestMemberID(r, member))
	if err != nil {
		writeRoomError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, roomResponse{Room: room, MemberID: member.ID})
}

func (s *Server) handleRoomLeave(w http.ResponseWriter, r *http.Request) {
	member, ok := s.callerMember(r, false)
	if !ok {
		writeError(w, http.StatusBadRequest, "member identity required")
		return
	}
	if _, err := s.rooms.Leave(r.PathValue("roomId"), member.ID); err != nil {
		writeRoomError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) handleRoomEnqueue(w http.ResponseWriter, r *http.Request) {
	member, ok := s.callerMember(r, false)
	if !ok {
		writeError(w, http.StatusBadRequest, "member identity required")
		return
	}
	var req queueRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	trackID, err := uuid.Parse(req.TrackID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid trackId")
		return
	}
	room, err := s.rooms.Enqueue(r.Context(), r.PathValue("roomId"), member.ID, trackID)
	if err != nil {
		writeRoomError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

func (s *Server) handleRoomRemove(w http.ResponseWriter, r *http.Request) {
	member, ok := s.callerMember(r, false)
	if !ok {
		writeError(w, http.StatusBadRequest, "member identity required")
		return
	}
	room, err := s.rooms.Remove(r.PathValue("roomId"), member.ID, r.PathValue("itemId"))
	if err != nil {
		writeRoomError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

func (s *Server) handleRoomReorder(w http.ResponseWriter, r *http.Request) {
	member, ok := s.callerMember(r, false)
	if !ok {
		writeError(w, http.StatusBadRequest, "member identity required")
		return
	}
	var req reorderRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	room, err := s.rooms.Reorder(r.PathValue("roomId"), member.ID, req.ItemIDs)
	if err != nil {
		writeRoomError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

// handleRoomClear empties the caller's queue. A host may empty anyone's by
// naming the owner with ?memberId=.
func (s *Server) handleRoomClear(w http.ResponseWriter, r *http.Request) {
	member, ok := s.callerMember(r, false)
	if !ok {
		writeError(w, http.StatusBadRequest, "member identity required")
		return
	}
	room, err := s.rooms.Clear(r.PathValue("roomId"), member.ID, r.URL.Query().Get("memberId"))
	if err != nil {
		writeRoomError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

func (s *Server) handleRoomPause(w http.ResponseWriter, r *http.Request) {
	s.roomCommand(w, r, func(roomID, memberID string) (*rooms.Snapshot, error) {
		return s.rooms.Pause(roomID, memberID)
	})
}

func (s *Server) handleRoomResume(w http.ResponseWriter, r *http.Request) {
	s.roomCommand(w, r, func(roomID, memberID string) (*rooms.Snapshot, error) {
		return s.rooms.Resume(roomID, memberID)
	})
}

func (s *Server) handleRoomSkip(w http.ResponseWriter, r *http.Request) {
	s.roomCommand(w, r, func(roomID, memberID string) (*rooms.Snapshot, error) {
		return s.rooms.Skip(roomID, memberID)
	})
}

// handleRoomEnded is the host saying their copy of the song has run out: the
// room moves on from their end, rather than from a length it worked out before
// the song started.
func (s *Server) handleRoomEnded(w http.ResponseWriter, r *http.Request) {
	s.roomCommand(w, r, func(roomID, memberID string) (*rooms.Snapshot, error) {
		return s.rooms.Ended(roomID, memberID)
	})
}

func (s *Server) handleRoomSeek(w http.ResponseWriter, r *http.Request) {
	member, ok := s.callerMember(r, false)
	if !ok {
		writeError(w, http.StatusBadRequest, "member identity required")
		return
	}
	var req seekRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	room, err := s.rooms.Seek(r.PathValue("roomId"), member.ID, req.PositionMs)
	if err != nil {
		writeRoomError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

func (s *Server) handleRoomVote(w http.ResponseWriter, r *http.Request) {
	member, ok := s.callerMember(r, false)
	if !ok {
		writeError(w, http.StatusBadRequest, "member identity required")
		return
	}
	var req voteRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	room, err := s.rooms.Vote(r.Context(), r.PathValue("roomId"), member.ID, req.Score)
	if err != nil {
		writeRoomError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

// handleRoomOut records that a member is sitting the room's track out, or is
// back in. The room plays for as long as the shortest file in it, so a member
// holding a short or broken copy can step out of that reckoning without
// leaving the room.
func (s *Server) handleRoomOut(w http.ResponseWriter, r *http.Request) {
	member, ok := s.callerMember(r, false)
	if !ok {
		writeError(w, http.StatusBadRequest, "member identity required")
		return
	}
	var req roomOutRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	room, err := s.rooms.SetOut(r.PathValue("roomId"), member.ID, req.Out)
	if err != nil {
		writeRoomError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"room": room})
}

func (s *Server) handleRoomReady(w http.ResponseWriter, r *http.Request) {
	member, ok := s.callerMember(r, false)
	if !ok {
		writeError(w, http.StatusBadRequest, "member identity required")
		return
	}
	var req readyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	trackID, err := uuid.Parse(req.TrackID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid trackId")
		return
	}
	variantID, err := uuid.Parse(req.VariantID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid variantId")
		return
	}
	room, err := s.rooms.Ready(r.PathValue("roomId"), member.ID, trackID, variantID, req.DurationMs)
	if err != nil {
		writeRoomError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

// handleClock reports the server clock; clients measure their offset from it
// and keep re-syncing while they follow a room.
func (s *Server) handleClock(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"nowMs": s.rooms.NowMs(),
		"listenTogether": map[string]any{
			"skipThreshold":        s.cfg.ListenTogether.SkipThreshold,
			"minVotersForSkip":     s.cfg.ListenTogether.MinVotersForSkip,
			"voterFractionForSkip": s.cfg.ListenTogether.VoterFractionForSkip,
			"readyTimeoutSeconds":  s.cfg.ListenTogether.ReadyTimeoutSeconds,
		},
	})
}

// roomCommand wraps the commands that need only a member identity.
func (s *Server) roomCommand(w http.ResponseWriter, r *http.Request, fn func(roomID, memberID string) (*rooms.Snapshot, error)) {
	member, ok := s.callerMember(r, false)
	if !ok {
		writeError(w, http.StatusBadRequest, "member identity required")
		return
	}
	room, err := fn(r.PathValue("roomId"), member.ID)
	if err != nil {
		writeRoomError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, room)
}

func writeRoomError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, rooms.ErrRoomNotFound), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, rooms.ErrMemberNotFound), errors.Is(err, rooms.ErrForbidden),
		errors.Is(err, rooms.ErrWrongPassword):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, rooms.ErrNoPlayback):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, rooms.ErrInvalidVote), errors.Is(err, rooms.ErrInvalidOrder),
		errors.Is(err, rooms.ErrInvalidSeek), errors.Is(err, rooms.ErrInvalidControl):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
