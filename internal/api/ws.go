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
			if writeCtx, err := s.writeSocket(ctx, conn, pong); err != nil {
				if writeCtx.Err() == nil {
					return
				}
				// The client is behind rather than gone: another ping will do.
			}
		case event, ok := <-events:
			if !ok {
				return
			}
			message := serverMessage{
				Type:   string(event.Type),
				RoomID: event.RoomID,
				Seq:    event.Seq,
				AtMs:   event.AtMs,
				Data:   event.Data,
			}
			if writeCtx, err := s.writeSocket(ctx, conn, message); err != nil {
				if writeCtx.Err() == nil {
					return
				}
				// Too slow to take this event: it is skipped rather than costing
				// the connection. The counter in the next event shows the gap,
				// and a client that sees a gap refetches the room - which is
				// better than a member appearing to leave and come back, taking
				// their queue with them.
				s.logger.Debug("ws: event skipped, the client is behind",
					"room", event.RoomID, "seq", event.Seq)
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

// writeSocket writes one message and says what went wrong.
//
// A write that runs out of time is a client that is not taking its messages
// fast enough, not one that has gone - the difference matters because the
// caller closes the socket on an error, and closing it here is what made a slow
// moment look like a disconnect. The client sees the gap in the event counter
// and refetches the room. The context is handed back so the caller can tell
// which of the two it was.
func (s *Server) writeSocket(ctx context.Context, conn *websocket.Conn, message serverMessage) (context.Context, error) {
	writeCtx, cancel := context.WithTimeout(ctx, wsWriteTimeout)
	defer cancel()
	if err := wsjson.Write(writeCtx, conn, message); err != nil {
		s.logger.Debug("ws: write failed", "error", err)
		return writeCtx, err
	}
	return writeCtx, nil
}
