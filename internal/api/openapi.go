package api

import (
	_ "embed"
	"net/http"
	"strings"
)

//go:embed openapi.yaml
var openAPISpec []byte

// handleOpenAPI serves the specification this server implements, so a client
// author never has to guess which version of the document matches the running
// binary.
func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(openAPISpec); err != nil {
		s.logger.Debug("openapi: write failed", "error", err)
	}
}

// route is one HTTP route: the pattern ServeMux registers and its handler.
//
// The table exists so the specification and the server cannot drift: a test
// compares it against the paths in openapi.yaml.
type route struct {
	pattern string
	handler http.HandlerFunc
}

// routeTable lists every route this server serves.
func (s *Server) routeTable() []route {
	return []route{
		{"GET /healthz", s.handleHealthz},
		{"GET /api/v1/openapi.yaml", s.handleOpenAPI},

		{"GET /api/v1/providers", s.handleProviders},
		{"GET /api/v1/artwork/{kind}/{artworkId}", s.handleArtwork},
		{"GET /api/v1/search", s.handleSearch},
		{"GET /api/v1/tracks/{trackId}", s.handleTrack},
		{"GET /api/v1/tracks/{trackId}/variants", s.handleTrackVariants},
		{"GET /api/v1/tracks/{trackId}/lyrics", s.handleTrackLyrics},
		{"POST /api/v1/tracks/{trackId}/resolve", s.handleTrackResolve},
		{"POST /api/v1/library/import", s.handleLibraryImport},
		{"POST /api/v1/radio", s.handleRadio},

		{"GET /api/v1/albums/search", s.handleAlbumSearch},
		{"GET /api/v1/albums/{albumId}", s.handleAlbum},
		{"POST /api/v1/albums/{albumId}/sync", s.handleAlbumSync},
		{"GET /api/v1/playlists/search", s.handlePlaylistSearch},
		{"GET /api/v1/playlists/{playlistId}", s.handlePlaylistShared},
		{"POST /api/v1/playlists/{playlistId}/sync", s.handlePlaylistSync},
		{"GET /api/v1/artists/search", s.handleArtistSearch},
		{"GET /api/v1/artists/{artistId}", s.handleArtist},
		{"POST /api/v1/artists/{artistId}/sync", s.handleArtistSync},

		{"GET /api/v1/media/{variantId}", s.handleMediaFile},
		{"GET /api/v1/media/{variantId}/status", s.handleMediaStatus},
		{"POST /api/v1/media/{variantId}/download", s.handleMediaDownload},

		{"POST /api/v1/auth/register", s.handleRegister},
		{"POST /api/v1/auth/login", s.handleLogin},
		{"POST /api/v1/auth/logout", s.handleLogout},
		{"GET /api/v1/me", s.handleMe},
		{"GET /api/v1/me/favorites", s.handleFavoritesList},
		{"POST /api/v1/me/favorites", s.handleFavoriteAdd},
		{"DELETE /api/v1/me/favorites/{trackId}", s.handleFavoriteRemove},
		{"GET /api/v1/me/providers/ranking", s.handleRankingGet},
		{"GET /api/v1/me/playback", s.handlePlaybackState},
		{"PUT /api/v1/me/playback", s.handlePlaybackStateSave},
		{"PUT /api/v1/me/providers/ranking", s.handleRankingPut},
		{"POST /api/v1/me/plays", s.handleRecordPlay},
		{"GET /api/v1/me/history", s.handleHistory},
		{"GET /api/v1/me/stats/top", s.handleTopTracks},

		{"GET /api/v1/me/playlists", s.handlePlaylistList},
		{"POST /api/v1/me/playlists", s.handlePlaylistCreate},
		{"POST /api/v1/me/playlists/import", s.handlePlaylistImportStart},
		{"GET /api/v1/me/playlist-imports/{jobId}", s.handlePlaylistImportStatus},
		{"POST /api/v1/me/playlists/{playlistId}/archives", s.handlePlaylistArchiveStart},
		{"GET /api/v1/me/playlist-archives/{archiveId}", s.handlePlaylistArchiveStatus},
		{"GET /api/v1/me/playlist-archives/{archiveId}/file", s.handlePlaylistArchiveFile},
		{"GET /api/v1/me/playlists/{playlistId}", s.handlePlaylistGet},
		{"PATCH /api/v1/me/playlists/{playlistId}", s.handlePlaylistRename},
		{"DELETE /api/v1/me/playlists/{playlistId}", s.handlePlaylistDelete},
		{"PUT /api/v1/me/playlists/{playlistId}/artwork", s.handlePlaylistArtwork},
		{"POST /api/v1/me/playlists/{playlistId}/items", s.handlePlaylistAdd},
		{"DELETE /api/v1/me/playlists/{playlistId}/items/{position}", s.handlePlaylistRemove},
		{"POST /api/v1/me/playlists/{playlistId}/reorder", s.handlePlaylistReorder},

		{"POST /api/v1/uploads", s.handleUploadCreate},
		{"GET /api/v1/uploads", s.handleUploadList},
		{"GET /api/v1/uploads/all", s.handleUploadListAll},
		{"GET /api/v1/uploads/association", s.handleUploadAssociation},
		{"GET /api/v1/uploads/duplicates", s.handleUploadDuplicates},
		{"POST /api/v1/uploads/associate", s.handleUploadsAssociate},
		{"GET /api/v1/users/{userId}/uploads", s.handleUserUploads},
		{"PATCH /api/v1/uploads/{uploadId}", s.handleUploadPatch},
		{"DELETE /api/v1/uploads/{uploadId}", s.handleUploadDelete},

		{"GET /api/v1/tracks/{trackId}/sources", s.handleTrackSources},
		{"POST /api/v1/tracks/{trackId}/sources", s.handleTrackSourceAttach},
		{"POST /api/v1/variants/{variantId}/vote", s.handleVariantVote},
		{"PUT /api/v1/me/tracks/{trackId}/preference", s.handleTrackPreference},

		{"PATCH /api/v1/me", s.handleMePatch},
		{"POST /api/v1/me/icon", s.handleMeIcon},
		{"POST /api/v1/me/password", s.handleMePassword},
		{"POST /api/v1/me/username", s.handleMeUsername},

		{"GET /api/v1/users", s.handleUserSearch},
		{"GET /api/v1/users/{userId}", s.handleUserGet},
		{"GET /api/v1/users/{userId}/playback", s.handleUserPlayback},
		{"GET /api/v1/me/friends", s.handleFriendsList},
		{"POST /api/v1/me/friends", s.handleFriendAdd},
		{"POST /api/v1/me/friends/{userId}/accept", s.handleFriendAccept},
		{"DELETE /api/v1/me/friends/{userId}", s.handleFriendRemove},
		{"POST /api/v1/me/friends/{userId}/ignore", s.handleFriendIgnore},
		{"DELETE /api/v1/me/friends/{userId}/ignore", s.handleFriendUnignore},
		{"GET /api/v1/me/shares", s.handleShareList},
		{"POST /api/v1/me/shares", s.handleShareCreate},
		{"GET /api/v1/me/notifications", s.handleNotifications},
		{"POST /api/v1/me/notifications/read", s.handleNotificationsRead},

		{"DELETE /api/v1/rooms/{roomId}/queue", s.handleRoomClear},

		{"GET /api/v1/clock", s.handleClock},
		{"GET /api/v1/ws", s.handleWS},
		{"GET /api/v1/rooms", s.handleRoomList},
		{"POST /api/v1/rooms", s.handleRoomCreate},
		{"GET /api/v1/rooms/{roomId}", s.handleRoomGet},
		{"POST /api/v1/rooms/{roomId}/join", s.handleRoomJoin},
		{"POST /api/v1/rooms/{roomId}/leave", s.handleRoomLeave},
		{"POST /api/v1/rooms/{roomId}/queue", s.handleRoomEnqueue},
		{"DELETE /api/v1/rooms/{roomId}/queue/{itemId}", s.handleRoomRemove},
		{"POST /api/v1/rooms/{roomId}/queue/reorder", s.handleRoomReorder},
		{"POST /api/v1/rooms/{roomId}/pause", s.handleRoomPause},
		{"POST /api/v1/rooms/{roomId}/resume", s.handleRoomResume},
		{"POST /api/v1/rooms/{roomId}/skip", s.handleRoomSkip},
		{"POST /api/v1/rooms/{roomId}/ended", s.handleRoomEnded},
		{"POST /api/v1/rooms/{roomId}/seek", s.handleRoomSeek},
		{"POST /api/v1/rooms/{roomId}/vote", s.handleRoomVote},
		{"POST /api/v1/rooms/{roomId}/started", s.handleRoomStarted},
	}
}

func (s *Server) routes() {
	for _, route := range s.routeTable() {
		s.mux.HandleFunc(route.pattern, route.handler)
	}
}

// publicPaths are the endpoints reachable without a token when requireLogin is
// set. They are derived from the OpenAPI document's own list.
var publicPaths = map[string]bool{
	"/healthz":              true,
	"/api/v1/auth/register": true,
	"/api/v1/auth/login":    true,
	"/api/v1/openapi.yaml":  true,
	// The room channel: a guest follows the room they joined, and the socket is
	// how they hear what it is doing.
	"/api/v1/ws": true,
}

// publicPrefixes are the endpoints a guest may reach whatever follows them.
//
// A room is joined by anybody who has the link - that is what a room is for -
// and hearing what a room is playing needs the audio, so the media path is open
// with it. Neither one names the library: searching, browsing, playlists and
// everything that is somebody's own still need an account.
var publicPrefixes = []string{
	"/api/v1/rooms",
	"/api/v1/media/",
}

// isPublic reports whether a guest may reach this path.
func isPublic(path string) bool {
	if publicPaths[path] {
		return true
	}
	for _, prefix := range publicPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}
