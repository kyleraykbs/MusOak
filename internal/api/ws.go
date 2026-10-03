package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// writeTimeout bounds a single WebSocket write.
const wsWriteTimeout = 10 * time.Second

// clientMessage is what a client may send on the event socket.
type clientMessage struct {
	Type         string `json:"type"`
	ClientSentAt int64  `json:"clientSentAt,omitempty"`
	RoomID       string `json:"roomId,omitempty"`
}

// serverMessage is what the server sends on the event socket: either a room
// event or the pong of a clock-sync ping.
type serverMessage struct {
	Type             string `json:"type"`
	ClientSentAt     int64  `json:"clientSentAt,omitempty"`
	ServerReceivedAt int64  `json:"serverReceivedAt,omitempty"`
	ServerSentAt     int64  `json:"serverSentAt,omitempty"`
	RoomID           string `json:"roomId,omitempty"`
	AtMs             int64  `json:"atMs,omitempty"`
	Data             any    `json:"data,omitempty"`
}

// handleWS streams room events and answers clock-sync pings.
//
// A client estimates its offset with several ping samples and takes the one
// with the lowest round-trip time:
//
//	offset = serverReceivedAt - (clientSentAt + clientReceivedAt)/2
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Self-hosted server: any origin may connect. Authentication travels
		// in the Authorization header (never in cookies), so there is no
		// ambient authority for a cross-site request to borrow.
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		// Accept has already written a response.
		s.logger.Debug("ws: accept failed", "error", err)
		return
	}
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// A WebSocket cannot carry the headers the rest of the API uses, so the
	// identity travels in the query - that is how the web client names itself.
	// Without this the socket is nobody's, and nothing is ever disconnected.
	identity := strings.TrimSpace(r.URL.Query().Get("memberId"))
	if identity == "" {
		if member, ok := s.callerMember(r, false); ok {
			identity = member.ID
		}
	}
	if identity != "" {
		s.rooms.Connect(identity)
		defer s.rooms.Disconnect(identity)
	}

	events, unsubscribe := s.rooms.Subscribe()
	defer unsubscribe()

	roomFilter := r.URL.Query().Get("roomId")

	// One writer: the read loop funnels pongs back through this channel.
	pongs := make(chan serverMessage, 8)
	go s.readSocket(ctx, conn, pongs)

	for {
		select {
		case <-ctx.Done():
			return
		case pong, ok := <-pongs:
			if !ok {
				return
			}
			if !s.writeSocket(ctx, conn, pong) {
				return
			}
		case event, ok := <-events:
			if !ok {
				return
			}
			if roomFilter != "" && event.RoomID != roomFilter {
				continue
			}
			message := serverMessage{
				Type:   string(event.Type),
				RoomID: event.RoomID,
				AtMs:   event.AtMs,
				Data:   event.Data,
			}
			if !s.writeSocket(ctx, conn, message) {
				return
			}
		}
	}
}

// readSocket answers client pings until the connection closes.
func (s *Server) readSocket(ctx context.Context, conn *websocket.Conn, pongs chan<- serverMessage) {
	defer close(pongs)
	for {
		var message clientMessage
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			return
		}
		if message.Type != "ping" {
			continue
		}
		receivedAt := s.rooms.NowMs()
		select {
		case pongs <- serverMessage{
			Type:             "pong",
			ClientSentAt:     message.ClientSentAt,
			ServerReceivedAt: receivedAt,
			ServerSentAt:     s.rooms.NowMs(),
		}:
		default:
			// The client is not reading; the next ping will do.
		}
	}
}

func (s *Server) writeSocket(ctx context.Context, conn *websocket.Conn, message serverMessage) bool {
	writeCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	if err := wsjson.Write(writeCtx, conn, message); err != nil {
		s.logger.Debug("ws: write failed", "error", err)
		return false
	}
	return true
}
