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

// wsReadLimit bounds a message from the client. Client messages are clock
// pings and nothing else, so the library's own 32 KiB default is already
// generous; the limit is set anyway so the bound is a decision, not a default.
const wsReadLimit = 64 << 10

// The server keeps a connection honest: a ping every wsPingEvery, and wsPongWait
// for the pong. A peer that has gone without a FIN - a phone that slept, a
// killed tab - answers nothing, so the ping fails and the socket is closed;
// that is what makes the handler, its subscriber and its member fall away
// through the leave grace instead of leaking until OS TCP keepalive.
const (
	wsPingEvery = 30 * time.Second
	wsPongWait  = 10 * time.Second
)

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
	// Seq counts a room's events: a client that sees a gap refetches the room,
	// because the bus drops events for a client that cannot keep up.
	Seq  int64 `json:"seq,omitempty"`
	AtMs int64 `json:"atMs,omitempty"`
	Data any   `json:"data,omitempty"`
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
	// A client sends nothing but the occasional clock ping; anything larger is
	// not one of ours, and the bound keeps a client from making the server read
	// without end.
	conn.SetReadLimit(wsReadLimit)

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
	} else if s.namesAccount(r, identity) {
		// A socket counts as an account's presence only when it is that
		// account's: anybody else naming it could close the socket and take the
		// member out of every room through the leave grace.
		if user := s.currentUser(r); user == nil || user.ID.String() != identity {
			identity = ""
		}
	}
	if identity != "" {
		s.rooms.Connect(identity)
		defer s.rooms.Disconnect(identity)
	}

	// A connection follows one room and subscribes to just that room: another
	// room's heartbeat never lands in its buffer and costs it one of its own
	// events, which it would then have to resync from a snapshot. No room named
	// means every room, as before.
	roomFilter := r.URL.Query().Get("roomId")
	events, unsubscribe := s.rooms.SubscribeRoom(roomFilter)
	defer unsubscribe()

	// One writer: the read loop funnels pongs back through this channel.
	pongs := make(chan serverMessage, 8)
	go s.readSocket(ctx, conn, pongs)
	// And one keepalive, so a connection that is only half here is noticed.
	go s.pingSocket(ctx, conn, cancel)

	for {
		select {
		case <-ctx.Done():
			return
		case pong, ok := <-pongs:
			if !ok {
				return
			}
			if err := s.writeSocket(ctx, conn, pong); err != nil {
				return
			}
		case event, ok := <-events:
			if !ok {
				return
			}
			// A password room's events are for its members: a socket that is
			// nobody, or somebody it did not let in, is not told who is in it
			// or what it is playing.
			if !s.rooms.OpenTo(event.RoomID, identity) {
				continue
			}
			message := serverMessage{
				Type:   string(event.Type),
				RoomID: event.RoomID,
				Seq:    event.Seq,
				AtMs:   event.AtMs,
				Data:   event.Data,
			}
			if err := s.writeSocket(ctx, conn, message); err != nil {
				// The library closes a connection whose write ran out of time,
				// so there is no skipping one event and carrying on: the socket
				// is gone either way, and the client's reconnect refetches the
				// room.
				return
			}
		}
	}
}

// pingSocket keeps the connection honest. It pings every wsPingEvery and waits
// wsPongWait for the pong, which readSocket reads in passing; that wait is the
// deadline a half-open connection trips, since a peer that has gone without a
// FIN answers nothing. A ping that fails cancels the connection, so the handler
// returns and the member goes through the leave grace, rather than the socket,
// its subscriber and the member sitting here until OS TCP keepalive.
func (s *Server) pingSocket(ctx context.Context, conn *websocket.Conn, cancel context.CancelFunc) {
	ticker := time.NewTicker(wsPingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, stop := context.WithTimeout(ctx, wsPongWait)
			err := conn.Ping(pingCtx)
			stop()
			if err != nil {
				s.logger.Debug("ws: ping failed, closing the connection", "error", err)
				cancel()
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

// writeSocket writes one message, bounded by wsWriteTimeout. A write that runs
// out of time has closed the connection (coder/websocket closes it rather than
// leave a frame half sent), so any error here is the end of this socket.
func (s *Server) writeSocket(ctx context.Context, conn *websocket.Conn, message serverMessage) error {
	writeCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	if err := wsjson.Write(writeCtx, conn, message); err != nil {
		s.logger.Debug("ws: write failed", "error", err)
		return err
	}
	return nil
}
