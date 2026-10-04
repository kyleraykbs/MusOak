package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/store"
	"strings"
)

// presenceWindow is how recently an account must have played something to
// count as online — and how fresh a playback document must be to be what
// somebody is listening to rather than a leftover from an hour ago.
const presenceWindow = 5 * time.Minute

// maxUserSearch bounds a people search to a page of names.
const maxUserSearch = 20

// directoryUserLimit is how many accounts a server may hold and still answer an
// empty people search with all of them. Below it a list is what somebody wants -
// typing a name to find a person you have not met is a guessing game - and above
// it the answer is nothing, which the client reports as too many to list rather
// than as nobody being here.
const directoryUserLimit = 50

// publicUserResponse is an account as other people see it: no password material,
// but how it relates to the viewer and what they are playing right now.
type publicUserResponse struct {
	ID           string          `json:"id"`
	Username     string          `json:"username"`
	DisplayName  string          `json:"displayName"`
	IconURL      string          `json:"iconUrl"`
	IconVersion  int             `json:"iconVersion"`
	Online       bool            `json:"online"`
	LastPlayedAt *time.Time      `json:"lastPlayedAt"`
	Listening    json.RawMessage `json:"listening"`
	Relationship string          `json:"relationship"`
}

// shareResponse is one thing a user sent another: a song or a room invite.
// The track is embedded so a listing of shares renders without a fetch per
// row; the ids are there too because they are what was actually shared.
type shareResponse struct {
	ID        string             `json:"id"`
	From      publicUserResponse `json:"from"`
	To        publicUserResponse `json:"to"`
	TrackID   *string            `json:"trackId"`
	RoomID    *string            `json:"roomId"`
	Track     *trackResponse     `json:"track"`
	CreatedAt time.Time          `json:"createdAt"`
}

// notificationResponse is one bell entry.
type notificationResponse struct {
	ID        string              `json:"id"`
	Kind      string              `json:"kind"`
	From      *publicUserResponse `json:"from"`
	TrackID   *string             `json:"trackId"`
	RoomID    *string             `json:"roomId"`
	CreatedAt time.Time           `json:"createdAt"`
	Read      bool                `json:"read"`
}

// buildPublicUser fills in what an account row cannot: whether the user counts as
// online and what they are playing right now.
func (s *Server) buildPublicUser(ctx context.Context, viewerID uuid.UUID, u store.User, rel store.Relationship) (publicUserResponse, error) {
	out := publicUserResponse{
		ID:           u.ID.String(),
		Username:     u.Username,
		DisplayName:  u.DisplayName,
		IconURL:      u.IconURL,
		IconVersion:  u.IconVersion,
		Relationship: string(rel),
	}
	if !u.LastPlayedAt.IsZero() {
		last := u.LastPlayedAt
		out.LastPlayedAt = &last
		out.Online = time.Since(u.LastPlayedAt) <= presenceWindow
	}

	state, updated, err := s.store.RecentPlayback(ctx, u.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// They never saved what they were playing; listening stays null.
	case err != nil:
		return publicUserResponse{}, err
	case time.Since(updated) <= presenceWindow:
		out.Listening = json.RawMessage(state)
	}
	return out, nil
}

// buildPublicUsers labels a whole listing with the relationship its bucket stands
// for: every row of the friends list is a friend.
func (s *Server) buildPublicUsers(ctx context.Context, viewerID uuid.UUID, users []store.User, rel store.Relationship) ([]publicUserResponse, error) {
	out := make([]publicUserResponse, 0, len(users))
	for _, u := range users {
		response, err := s.buildPublicUser(ctx, viewerID, u, rel)
		if err != nil {
			return nil, err
		}
		out = append(out, response)
	}
	return out, nil
}

// buildShare resolves both ends and the song so a listing of shares needs no
// further lookups to render.
func (s *Server) buildShare(ctx context.Context, viewerID uuid.UUID, share store.Share) (shareResponse, error) {
	from, rel, err := s.store.UserForViewer(ctx, viewerID, share.FromUser)
	if err != nil {
		return shareResponse{}, err
	}
	fromResponse, err := s.buildPublicUser(ctx, viewerID, *from, rel)
	if err != nil {
		return shareResponse{}, err
	}
	to, rel, err := s.store.UserForViewer(ctx, viewerID, share.ToUser)
	if err != nil {
		return shareResponse{}, err
	}
	toResponse, err := s.buildPublicUser(ctx, viewerID, *to, rel)
	if err != nil {
		return shareResponse{}, err
	}

	out := shareResponse{
		ID:        share.ID.String(),
		From:      fromResponse,
		To:        toResponse,
		CreatedAt: share.CreatedAt,
	}
	if share.TrackID != nil {
		trackID := share.TrackID.String()
		out.TrackID = &trackID
		track, err := s.store.Track(ctx, *share.TrackID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			// The track was deleted since; the id still says what was shared.
		case err != nil:
			return shareResponse{}, err
		default:
			response, err := s.buildTrack(ctx, *track)
			if err != nil {
				return shareResponse{}, err
			}
			out.Track = &response
		}
	}
	if share.RoomID != "" {
		roomID := share.RoomID
		out.RoomID = &roomID
	}
	return out, nil
}

// buildTrackList resolves track ids, skipping the ones deleted since.
func (s *Server) buildTrackList(ctx context.Context, ids []uuid.UUID) ([]trackResponse, error) {
	tracks := make([]trackResponse, 0, len(ids))
	for _, id := range ids {
		track, err := s.store.Track(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue // gone from the library, but it was there once
		}
		if err != nil {
			return nil, err
		}
		response, err := s.buildTrack(ctx, *track)
		if err != nil {
			return nil, err
		}
		tracks = append(tracks, response)
	}
	return tracks, nil
}

// recentTrackIDs reads the tracks out of a stored playback document: what is
// playing now first, then what was queued behind it. The document is the
// client's to define — only the track ids are read here.
func recentTrackIDs(doc []byte) []uuid.UUID {
	var state struct {
		Queue []struct {
			TrackID string `json:"trackId"`
		} `json:"queue"`
		CurrentTrackID string `json:"currentTrackId"`
	}
	if err := json.Unmarshal(doc, &state); err != nil {
		return nil
	}
	ids := []uuid.UUID{}
	seen := map[uuid.UUID]bool{}
	add := func(raw string) {
		id, err := uuid.Parse(raw)
		if err != nil || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	add(state.CurrentTrackID)
	for _, item := range state.Queue {
		add(item.TrackID)
	}
	return ids
}

// userIDFromPath parses the {userId} path value.
func userIDFromPath(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	raw := r.PathValue("userId")
	if len(raw) == 0 || len(raw) > maxMediaVariantIDLen {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return uuid.Nil, false
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid user id")
		return uuid.Nil, false
	}
	return id, true
}

// handleUserSearch lists everyone a name matches — the Search tab of the
// friends page. Guests have no people to search for.
func (s *Server) handleUserSearch(w http.ResponseWriter, r *http.Request) {
	viewer, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := maxUserSearch
	// An empty search is a request for the whole server. A small one answers
	// with everybody; a large one answers with nothing and says which it was,
	// because an empty page reads as "nobody is here" otherwise.
	directory := false
	if query == "" {
		total, err := s.store.CountUsers(r.Context())
		if err != nil {
			writeStoreError(w, err, "users unavailable")
			return
		}
		if total > directoryUserLimit {
			writeJSON(w, http.StatusOK, map[string]any{
				"users":     []publicUserResponse{},
				"directory": false,
			})
			return
		}
		directory = true
		limit = directoryUserLimit
	}
	users, err := s.store.SearchUsers(r.Context(), query, limit)
	if err != nil {
		writeStoreError(w, err, "users unavailable")
		return
	}
	ids := make([]uuid.UUID, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	relationships, err := s.store.Relationships(r.Context(), viewer.ID, ids)
	if err != nil {
		writeStoreError(w, err, "users unavailable")
		return
	}
	responses := make([]publicUserResponse, 0, len(users))
	for _, u := range users {
		response, err := s.buildPublicUser(r.Context(), viewer.ID, u, relationships[u.ID])
		if err != nil {
			writeStoreError(w, err, "users unavailable")
			return
		}
		responses = append(responses, response)
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": responses, "directory": directory})
}

// handleUserGet is the friend page: the account plus the four lists the page
// shows — their playlists, their favourites, the songs in their last
// playback state, and everything they shared with the caller.
//
// Playlist privacy is not modelled anywhere in the schema, so every playlist
// a user owns counts as public here.
func (s *Server) handleUserGet(w http.ResponseWriter, r *http.Request) {
	viewer, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := userIDFromPath(w, r)
	if !ok {
		return
	}
	user, rel, err := s.store.UserForViewer(r.Context(), viewer.ID, id)
	if err != nil {
		writeStoreError(w, err, "user not found")
		return
	}
	response, err := s.buildPublicUser(r.Context(), viewer.ID, *user, rel)
	if err != nil {
		writeStoreError(w, err, "user unavailable")
		return
	}

	// What another account's page shows is what they shared; your own shows all
	// of them, private ones included.
	var playlists []store.Playlist
	if id == viewer.ID {
		playlists, err = s.store.PlaylistsForUser(r.Context(), id)
	} else {
		playlists, err = s.store.PublicPlaylistsForUser(r.Context(), id)
	}
	if err != nil {
		writeStoreError(w, err, "user unavailable")
		return
	}
	public := make([]playlistResponse, 0, len(playlists))
	for i := range playlists {
		public = append(public, playlistSummary(&playlists[i], viewer.ID))
	}

	favoriteIDs, err := s.store.Favorites(r.Context(), id)
	if err != nil {
		writeStoreError(w, err, "user unavailable")
		return
	}
	favorites, err := s.buildTrackList(r.Context(), favoriteIDs)
	if err != nil {
		writeStoreError(w, err, "user unavailable")
		return
	}

	recent := []trackResponse{}
	doc, _, err := s.store.RecentPlayback(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// nothing played, nothing to show
	case err != nil:
		writeStoreError(w, err, "user unavailable")
		return
	default:
		if recent, err = s.buildTrackList(r.Context(), recentTrackIDs(doc)); err != nil {
			writeStoreError(w, err, "user unavailable")
			return
		}
	}

	shares, err := s.store.SharesBetween(r.Context(), id, viewer.ID)
	if err != nil {
		writeStoreError(w, err, "user unavailable")
		return
	}
	shared := make([]shareResponse, 0, len(shares))
	for _, share := range shares {
		built, err := s.buildShare(r.Context(), viewer.ID, share)
		if err != nil {
			writeStoreError(w, err, "user unavailable")
			return
		}
		shared = append(shared, built)
	}
	s.withPlays(r.Context(), viewer, trackRefs(favorites))
	s.withPlays(r.Context(), viewer, trackRefs(recent))
	sharedRefs := make([]*trackResponse, 0, len(shared))
	for i := range shared {
		if shared[i].Track != nil {
			sharedRefs = append(sharedRefs, shared[i].Track)
		}
	}
	s.withPlays(r.Context(), viewer, sharedRefs)

	writeJSON(w, http.StatusOK, map[string]any{
		"user":            response,
		"publicPlaylists": public,
		"favorites":       favorites,
		"recent":          recent,
		"shared":          shared,
	})
}

// handleUserPlayback returns a friend's playback document so the client can
// play along: it re-renders the document locally and polls to keep up. Only
// friends get to see where somebody is in a song.
func (s *Server) handleUserPlayback(w http.ResponseWriter, r *http.Request) {
	viewer, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := userIDFromPath(w, r)
	if !ok {
		return
	}
	if id != viewer.ID {
		_, rel, err := s.store.UserForViewer(r.Context(), viewer.ID, id)
		if err != nil {
			writeStoreError(w, err, "user not found")
			return
		}
		if rel != store.RelFriend {
			writeError(w, http.StatusForbidden, "only friends can see playback")
			return
		}
	}

	// Nothing stored is not an error: playing along is the same code path
	// whether or not there is anything to follow yet.
	document := map[string]any{}
	if raw, _, err := s.store.RecentPlayback(r.Context(), id); err == nil {
		if unmarshalErr := json.Unmarshal(raw, &document); unmarshalErr != nil {
			document = map[string]any{}
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		writeStoreError(w, err, "playback unavailable")
		return
	}
	document["userId"] = id.String()
	writeJSON(w, http.StatusOK, document)
}

// handleFriendsList returns the three lists the Requests tab shows.
func (s *Server) handleFriendsList(w http.ResponseWriter, r *http.Request) {
	viewer, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	friends, err := s.store.Friends(r.Context(), viewer.ID)
	if err != nil {
		writeStoreError(w, err, "friends unavailable")
		return
	}
	incoming, err := s.store.IncomingFriendRequests(r.Context(), viewer.ID)
	if err != nil {
		writeStoreError(w, err, "friends unavailable")
		return
	}
	outgoing, err := s.store.OutgoingFriendRequests(r.Context(), viewer.ID)
	if err != nil {
		writeStoreError(w, err, "friends unavailable")
		return
	}
	friendResponses, err := s.buildPublicUsers(r.Context(), viewer.ID, friends, store.RelFriend)
	if err != nil {
		writeStoreError(w, err, "friends unavailable")
		return
	}
	incomingResponses, err := s.buildPublicUsers(r.Context(), viewer.ID, incoming, store.RelPendingIn)
	if err != nil {
		writeStoreError(w, err, "friends unavailable")
		return
	}
	outgoingResponses, err := s.buildPublicUsers(r.Context(), viewer.ID, outgoing, store.RelPendingOut)
	if err != nil {
		writeStoreError(w, err, "friends unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"friends":  friendResponses,
		"incoming": incomingResponses,
		"outgoing": outgoingResponses,
	})
}

type friendAddRequest struct {
	Username string `json:"username"`
}

// handleFriendAdd sends a friend request; the answer comes from the other
// side.
func (s *Server) handleFriendAdd(w http.ResponseWriter, r *http.Request) {
	viewer, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req friendAddRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	target, err := s.store.UserByUsername(r.Context(), req.Username)
	if err != nil {
		writeStoreError(w, err, "user not found")
		return
	}
	if target.ID == viewer.ID {
		writeError(w, http.StatusBadRequest, "you cannot friend yourself")
		return
	}
	if err := s.store.SendFriendRequest(r.Context(), viewer.ID, target.ID); err != nil {
		writeStoreError(w, err, "the request could not be sent")
		return
	}
	// Tell them, so the bell has something to say: a request nobody notices is
	// a request nobody answers. Failing to record it must not fail the request
	// itself, which has already been sent.
	if _, err := s.store.CreateNotification(r.Context(), &store.Notification{
		UserID:   target.ID,
		Kind:     "friend-request",
		FromUser: &viewer.ID,
	}); err != nil {
		s.logger.Warn("friend request notification failed", "to", target.ID, "error", err)
	}
	response, err := s.buildPublicUser(r.Context(), viewer.ID, *target, store.RelPendingOut)
	if err != nil {
		writeStoreError(w, err, "user unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": response})
}

// handleFriendAccept answers a pending request with yes.
func (s *Server) handleFriendAccept(w http.ResponseWriter, r *http.Request) {
	viewer, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := userIDFromPath(w, r)
	if !ok {
		return
	}
	if err := s.store.AcceptFriendRequest(r.Context(), viewer.ID, id); err != nil {
		writeStoreError(w, err, "no friend request from this user")
		return
	}
	// The other side asked; this is the answer. Same rule: the acceptance is
	// what matters, and a notification that cannot be recorded does not undo it.
	if _, err := s.store.CreateNotification(r.Context(), &store.Notification{
		UserID:   id,
		Kind:     "friend-accepted",
		FromUser: &viewer.ID,
	}); err != nil {
		s.logger.Warn("friend accepted notification failed", "to", id, "error", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleFriendRemove unfriends — or declines the request this user sent.
func (s *Server) handleFriendRemove(w http.ResponseWriter, r *http.Request) {
	viewer, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := userIDFromPath(w, r)
	if !ok {
		return
	}
	if err := s.store.RemoveFriend(r.Context(), viewer.ID, id); err != nil {
		writeStoreError(w, err, "not friends with this user")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleFriendIgnore silences this user: their shares stop creating
// notifications. It is a separate action from unfriending on purpose — you
// can stay friends with somebody whose shares you do not want to hear about.
func (s *Server) handleFriendIgnore(w http.ResponseWriter, r *http.Request) {
	viewer, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := userIDFromPath(w, r)
	if !ok {
		return
	}
	if id == viewer.ID {
		writeError(w, http.StatusBadRequest, "you cannot ignore yourself")
		return
	}
	if _, err := s.store.User(r.Context(), id); err != nil {
		writeStoreError(w, err, "user not found")
		return
	}
	if err := s.store.IgnoreUser(r.Context(), viewer.ID, id); err != nil {
		writeStoreError(w, err, "the user could not be ignored")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleFriendUnignore lets a silenced user's shares ring the bell again.
func (s *Server) handleFriendUnignore(w http.ResponseWriter, r *http.Request) {
	viewer, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	id, ok := userIDFromPath(w, r)
	if !ok {
		return
	}
	if err := s.store.UnignoreUser(r.Context(), viewer.ID, id); err != nil {
		writeStoreError(w, err, "this user is not ignored")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleShareList is everything the caller has shared and everything shared
// with them, newest first.
func (s *Server) handleShareList(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	shares, err := s.store.UserShares(r.Context(), caller.ID)
	if err != nil {
		writeStoreError(w, err, "shares unavailable")
		return
	}
	responses := make([]shareResponse, 0, len(shares))
	for _, share := range shares {
		response, err := s.buildShare(r.Context(), caller.ID, share)
		if err != nil {
			writeStoreError(w, err, "shares unavailable")
			return
		}
		responses = append(responses, response)
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": responses})
}

type shareCreateRequest struct {
	UserID  string `json:"userId"`
	TrackID string `json:"trackId"`
	RoomID  string `json:"roomId"`
}

// handleShareCreate sends someone a song or a room invite. The share reaches
// their bell as one notification — unless they ignore the sender.
func (s *Server) handleShareCreate(w http.ResponseWriter, r *http.Request) {
	sender, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req shareCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	targetID, err := uuid.Parse(req.UserID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid userId")
		return
	}
	if targetID == sender.ID {
		writeError(w, http.StatusBadRequest, "you cannot share with yourself")
		return
	}
	if _, err := s.store.User(r.Context(), targetID); err != nil {
		writeStoreError(w, err, "user not found")
		return
	}

	share := store.Share{FromUser: sender.ID, ToUser: targetID, RoomID: req.RoomID}
	kind := ""
	switch {
	case req.TrackID != "" && req.RoomID != "":
		writeError(w, http.StatusBadRequest, "share a track or a room, not both")
		return
	case req.TrackID != "":
		trackID, err := uuid.Parse(req.TrackID)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid trackId")
			return
		}
		if _, err := s.store.Track(r.Context(), trackID); err != nil {
			writeStoreError(w, err, "track not found")
			return
		}
		share.TrackID = &trackID
		kind = "share"
	case req.RoomID != "":
		kind = "room-invite"
	default:
		writeError(w, http.StatusBadRequest, "share a track or a room")
		return
	}

	if err := s.store.RecordShare(r.Context(), &share); err != nil {
		writeStoreError(w, err, "the share could not be stored")
		return
	}
	if _, err := s.store.CreateNotification(r.Context(), &store.Notification{
		UserID:   targetID,
		Kind:     kind,
		FromUser: &sender.ID,
		TrackID:  share.TrackID,
		RoomID:   share.RoomID,
	}); err != nil {
		writeStoreError(w, err, "the share could not be stored")
		return
	}

	response, err := s.buildShare(r.Context(), sender.ID, share)
	if err != nil {
		writeStoreError(w, err, "the share could not be stored")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"share": response})
}

// handleNotifications returns the bell: the newest entries and how many are
// still unread.
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	entries, unread, err := s.store.Notifications(r.Context(), caller.ID)
	if err != nil {
		writeStoreError(w, err, "notifications unavailable")
		return
	}

	// The Friends tab carries how many people are waiting on an answer, and the
	// bell's poll is what keeps it current: one request, not two.
	friendRequests, err := s.store.CountIncomingFriendRequests(r.Context(), caller.ID)
	if err != nil {
		writeStoreError(w, err, "notifications unavailable")
		return
	}

	// One lookup for every sender on the page rather than one per row.
	senders := make([]uuid.UUID, 0, len(entries))
	for _, entry := range entries {
		if entry.FromUser != nil {
			senders = append(senders, *entry.FromUser)
		}
	}
	relationships, err := s.store.Relationships(r.Context(), caller.ID, senders)
	if err != nil {
		writeStoreError(w, err, "notifications unavailable")
		return
	}

	responses := make([]notificationResponse, 0, len(entries))
	for _, entry := range entries {
		response := notificationResponse{
			ID:        entry.ID.String(),
			Kind:      entry.Kind,
			CreatedAt: entry.CreatedAt,
			Read:      entry.Read,
		}
		if entry.TrackID != nil {
			trackID := entry.TrackID.String()
			response.TrackID = &trackID
		}
		if entry.RoomID != "" {
			roomID := entry.RoomID
			response.RoomID = &roomID
		}
		if entry.FromUser != nil {
			from, err := s.store.User(r.Context(), *entry.FromUser)
			switch {
			case errors.Is(err, store.ErrNotFound):
				// The sender is gone; the entry still says what happened.
			case err != nil:
				writeStoreError(w, err, "notifications unavailable")
				return
			default:
				fromResponse, err := s.buildPublicUser(r.Context(), caller.ID, *from, relationships[*entry.FromUser])
				if err != nil {
					writeStoreError(w, err, "notifications unavailable")
					return
				}
				response.From = &fromResponse
			}
		}
		responses = append(responses, response)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"notifications":  responses,
		"unread":         unread,
		"friendRequests": friendRequests,
	})
}

// handleNotificationsRead marks everything read; the badge goes to zero.
func (s *Server) handleNotificationsRead(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if err := s.store.MarkNotificationsRead(r.Context(), caller.ID); err != nil {
		writeStoreError(w, err, "notifications could not be marked read")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
