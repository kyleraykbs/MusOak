package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"codeberg.org/kyleraykbs/musoak/pkg/client"
)

// socketServer serves one event socket, writing with whatever the test does.
func socketServer(t *testing.T, serve func(ctx context.Context, conn *websocket.Conn)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		serve(r.Context(), conn)
	}))
	t.Cleanup(server.Close)
	return server
}

// A room event carries the room's whole queue - every member's items - which a
// room with a few listeners passes the websocket library's 32 KiB default.
// Going over it does not report a large message: the connection is closed with
// 1009, which from the room looks like the member walking out and coming back
// later with their queue gone. The socket has to survive a big one.
func TestTheEventSocketSurvivesALargeRoomEvent(t *testing.T) {
	big := strings.Repeat("x", 200_000)
	server := socketServer(t, func(ctx context.Context, conn *websocket.Conn) {
		_ = wsjson.Write(ctx, conn, map[string]any{
			"type": "queue_updated", "roomId": "r1", "seq": 1,
			"data": map[string]any{"title": big},
		})
		_ = wsjson.Write(ctx, conn, map[string]any{
			"type": "playback", "roomId": "r1", "seq": 2,
		})
		<-ctx.Done()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	events, err := client.New(server.URL).Events(ctx, "")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	first, ok := <-events
	if !ok {
		t.Fatal("the socket closed on a room event larger than the library's default limit")
	}
	if first.Seq != 1 {
		t.Errorf("first event seq = %d, want 1", first.Seq)
	}
	if second, ok := <-events; !ok || second.Seq != 2 {
		t.Fatalf("the socket did not go on to the next event: ok = %v, seq = %d", ok, second.Seq)
	}
}

// A client that is not taking its events must not stop the socket being read:
// the server's own write then times out and the connection closes, which is the
// member leaving the room. What is behind is dropped instead - the counter in
// the next event shows the gap, and a client that sees a gap refetches the room.
func TestASlowConsumerDoesNotCloseTheSocket(t *testing.T) {
	const flood = 4000
	server := socketServer(t, func(ctx context.Context, conn *websocket.Conn) {
		for i := 1; i <= flood; i++ {
			writeCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
			err := wsjson.Write(writeCtx, conn, map[string]any{
				"type": "queue_updated", "roomId": "r1", "seq": i,
				"data": map[string]any{"filler": strings.Repeat("y", 200)},
			})
			cancel()
			if err != nil {
				return // the write deadline passed: the client stopped reading
			}
		}
		_ = wsjson.Write(ctx, conn, map[string]any{"type": "playback", "roomId": "r1", "seq": flood + 1})
		<-ctx.Done()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	events, err := client.New(server.URL).Events(ctx, "")
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	// Nobody reads for a while, so the buffer fills and the rest are dropped.
	time.Sleep(1500 * time.Millisecond)

	last := int64(0)
	seen := 0
	deadline := time.After(10 * time.Second)
	for last < flood+1 {
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatal("the socket closed while the consumer was behind")
			}
			seen++
			if event.Seq > last {
				last = event.Seq
			}
		case <-deadline:
			t.Fatalf("the last event never arrived: seq = %d, want %d", last, flood+1)
		}
	}
	if last != flood+1 {
		t.Fatalf("last seq = %d, want %d", last, flood+1)
	}
	// What was behind was dropped rather than the socket stalling: a client
	// that reads its own counter sees the gap and refetches the room.
	if seen >= flood {
		t.Errorf("saw %d events of %d, want the ones it was behind for to be dropped", seen, flood)
	}
}
