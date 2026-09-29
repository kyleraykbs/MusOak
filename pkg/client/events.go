package client

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// streamEvents dials the event socket and forwards every message.
func (c *Client) streamEvents(ctx context.Context, path string) (<-chan Event, error) {
	conn, err := c.dialSocket(ctx, path)
	if err != nil {
		return nil, err
	}

	events := make(chan Event, 64)
	go func() {
		defer close(events)
		defer conn.CloseNow()
		for {
			var event Event
			if err := wsjson.Read(ctx, conn, &event); err != nil {
				return
			}
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	return events, nil
}

// dialSocket opens a WebSocket, carrying the client's credentials.
func (c *Client) dialSocket(ctx context.Context, path string) (*websocket.Conn, error) {
	header := http.Header{}
	if c.token != "" {
		header.Set("Authorization", "Bearer "+c.token)
	}
	if c.memberID != "" {
		header.Set("X-Member-Id", c.memberID)
	}

	conn, _, err := websocket.Dial(ctx, c.socketURL(path), &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, fmt.Errorf("prismusic: websocket %s: %w", path, err)
	}
	return conn, nil
}

func (c *Client) socketURL(path string) string {
	switch {
	case strings.HasPrefix(c.baseURL, "https://"):
		return "wss://" + strings.TrimPrefix(c.baseURL, "https://") + path
	case strings.HasPrefix(c.baseURL, "http://"):
		return "ws://" + strings.TrimPrefix(c.baseURL, "http://") + path
	default:
		return c.baseURL + path
	}
}

// ClockSample is one ping round trip.
type ClockSample struct {
	OffsetMs int64
	RTTMs    int64
}

// ClockOffset estimates the offset between the server clock and this machine's
// clock: add it to a local millisecond timestamp to get server time.
//
// It takes NTP-style samples over the event socket and keeps the one with the
// lowest round trip, because that is the sample with the least network noise.
func (c *Client) ClockOffset(ctx context.Context, samples int) (time.Duration, error) {
	if samples < 1 {
		samples = 1
	}
	conn, err := c.dialSocket(ctx, "/api/v1/ws")
	if err != nil {
		return 0, err
	}
	defer conn.CloseNow()

	var best *ClockSample
	for i := 0; i < samples; i++ {
		sample, err := c.clockSample(ctx, conn)
		if err != nil {
			return 0, err
		}
		if best == nil || sample.RTTMs < best.RTTMs {
			best = sample
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return time.Duration(best.OffsetMs) * time.Millisecond, nil
}

type socketPing struct {
	Type         string `json:"type"`
	ClientSentAt int64  `json:"clientSentAt,omitempty"`
}

type socketPong struct {
	Type             string `json:"type"`
	ClientSentAt     int64  `json:"clientSentAt,omitempty"`
	ServerReceivedAt int64  `json:"serverReceivedAt,omitempty"`
	ServerSentAt     int64  `json:"serverSentAt,omitempty"`
}

func (c *Client) clockSample(ctx context.Context, conn *websocket.Conn) (*ClockSample, error) {
	sent := time.Now().UnixMilli()
	if err := wsjson.Write(ctx, conn, socketPing{Type: "ping", ClientSentAt: sent}); err != nil {
		return nil, fmt.Errorf("prismusic: clock ping: %w", err)
	}

	for {
		var message socketPong
		if err := wsjson.Read(ctx, conn, &message); err != nil {
			return nil, fmt.Errorf("prismusic: clock pong: %w", err)
		}
		if message.Type != "pong" || message.ClientSentAt != sent {
			continue // an event arrived while we were measuring
		}
		received := time.Now().UnixMilli()
		roundTrip := received - sent
		offset := message.ServerReceivedAt - (sent+received)/2
		return &ClockSample{OffsetMs: offset, RTTMs: roundTrip}, nil
	}
}
