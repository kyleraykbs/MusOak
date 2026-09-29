package cli

import (
	"io"
	"time"
)

// key is one decoded keystroke from the player.
type key int

// Player keys. Bindings follow vim: j/k move down/up (volume), h/l move
// left/right (previous/next track).
const (
	keyNone key = iota
	keyQuit
	keyVolumeUp
	keyVolumeDown
	keyNext
	keyPrevious
	keyPause
)

// escapeTimeout bounds how long the parser waits for the rest of an escape
// sequence before treating a lone ESC as "no key".
const escapeTimeout = 30 * time.Millisecond

// keyReader decodes a byte stream into keys, so arrow keys work without
// blocking on partial escape sequences.
type keyReader struct {
	bytes chan byte
}

// newKeyReader reads from r in the background; the reader is done when r ends.
func newKeyReader(r io.Reader) *keyReader {
	reader := &keyReader{bytes: make(chan byte, 64)}
	go func() {
		defer close(reader.bytes)
		buf := make([]byte, 64)
		for {
			n, err := r.Read(buf)
			for _, b := range buf[:n] {
				reader.bytes <- b
			}
			if err != nil {
				return
			}
		}
	}()
	return reader
}

// next decodes the next key, waiting at most timeout for one to arrive.
func (k *keyReader) next(timeout time.Duration) key {
	b, ok := k.await(timeout)
	if !ok {
		return keyNone
	}

	switch b {
	case 27: // ESC: an arrow key is ESC [ A/B/C/D
		if second, ok := k.await(escapeTimeout); ok && second == '[' {
			if final, ok := k.await(escapeTimeout); ok {
				switch final {
				case 'A':
					return keyVolumeUp
				case 'B':
					return keyVolumeDown
				case 'C':
					return keyNext
				case 'D':
					return keyPrevious
				}
			}
		}
		return keyNone
	case 'k':
		return keyVolumeUp
	case 'j':
		return keyVolumeDown
	case 'l':
		return keyNext
	case 'h':
		return keyPrevious
	case ' ':
		return keyPause
	case 'q', 3: // q, or Ctrl-C (raw mode delivers it as a byte)
		return keyQuit
	}
	return keyNone
}

func (k *keyReader) await(timeout time.Duration) (byte, bool) {
	if timeout <= 0 {
		b, ok := <-k.bytes
		return b, ok
	}
	select {
	case b, ok := <-k.bytes:
		return b, ok
	case <-time.After(timeout):
		return 0, false
	}
}

// volumeBar renders a 0-100 volume as a ten cell bar.
func volumeBar(volume int) string {
	if volume < 0 {
		volume = 0
	}
	if volume > 100 {
		volume = 100
	}
	filled := (volume*10 + 50) / 100 // rounded to the nearest cell
	return repeat("#", filled) + repeat("-", 10-filled)
}

func repeat(s string, n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]byte, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
