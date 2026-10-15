//go:build e2e

package provider

// A source that is always there and always the same, for the browser suite.
//
// The suite needs a download it can watch: every real provider is network,
// timing and luck, and a bar test written against one measures the internet.
// This one fetches nothing. It answers a query with one of its own songs, and
// its download is a fixed number of bytes written in fixed chunks with fixed
// pauses, reporting its own progress exactly the way a real provider does -
// bytes and a total while it downloads, milliseconds while it converts. The
// numbers are the same every run, so a test asserts that the bar moved and
// what it said, not that a race went its way.
//
// It can also stream: a not-yet-downloaded song is handed out as a generated
// sine in an Ogg/Opus container, paced a piece at a time and beginning wherever
// the caller asked, while the download runs beside it. That is what lets a
// test watch a song start playing before its file exists, and check that the
// file takes over without a gap. See Stream.
//
// Built only with `-tags e2e`; see Extras in extras_e2e.go.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kyleraykbs/MusOak/internal/ffmpeg"
)

const (
	// E2EProviderName is the name the fake registers under, which is also what
	// a test finds in the search answer and in the platform list.
	E2EProviderName = "e2e-slow"
	// E2ETrackID is the one provider track id the fake answers with.
	E2ETrackID = "e2e-slow-1"
	// E2ETitle is the track's title: searchable, and findable in the DOM.
	E2ETitle = "E2E Slow Tone"
	// E2EArtist is the track's artist.
	E2EArtist = "E2E Suite"
	// E2EDurationMs is the track's length, which is the source's length too.
	E2EDurationMs = 30_000

	// e2eSampleRate is the source's rate: 48 kHz mono s16le, so the source is
	// 30 * 48000 * 2 + 44 bytes and the same size on every run.
	e2eSampleRate = 48_000
	// e2eChunks is how many pieces the source is written in, and
	// e2eChunkPause how long each one is held before the next. Ten chunks a
	// quarter of a second apart is two and a half seconds of "downloading".
	e2eChunks     = 10
	e2eChunkPause = 250 * time.Millisecond
	// e2eConvertReports and e2eConvertPause shape the convert stage: three
	// reports a little apart, so a client polling twice a second sees the
	// stage change rather than missing it.
	e2eConvertReports = 3
	e2eConvertPause   = 400 * time.Millisecond
	// e2eSettlePause is held after the last chunk, with the whole source on
	// disk: the moment between a finished fetch and the conversion starting.
	e2eSettlePause = 750 * time.Millisecond

	// e2eStreamChunk and e2eStreamPause pace a stream: one piece every pause,
	// 128 KB/s. Slow enough that a stream is still arriving while its download
	// finishes a few seconds later, fast enough that the browser has audio to
	// play almost at once.
	e2eStreamChunk = 32 * 1024
	e2eStreamPause = 250 * time.Millisecond
	// e2eStreamBitrate is the Opus rate a stream is encoded at: how many bytes
	// a second of audio costs, and therefore when a stream's last piece goes
	// out.
	e2eStreamBitrate = "96k"
	// e2eStreamMIME is what a stream says it is, which is the container this
	// file is generated in.
	e2eStreamMIME = "audio/ogg"
)

// e2eStreamTrack is a second fake song a streaming test can use. The source
// answer has to be *fresh* - a rendition that already has a file is served
// from disk, not streamed - and the suite's backend is shared by every test in
// a run, so one song would already be downloaded by the second test that asks
// for it. Each is the same tone with its own id, so each has its own rendition
// and each starts undownloaded.
type e2eStreamTrack struct {
	ID    string
	Title string
}

// e2eStreamTracks are the fake's streamable songs, one per scenario a test
// wants a not-yet-downloaded source for.
var e2eStreamTracks = []e2eStreamTrack{
	{ID: "e2e-stream-1", Title: "E2E Stream One"},
	{ID: "e2e-stream-2", Title: "E2E Stream Two"},
	{ID: "e2e-stream-3", Title: "E2E Stream Three"},
	{ID: "e2e-stream-4", Title: "E2E Stream Four"},
	{ID: "e2e-stream-5", Title: "E2E Stream Five"},
	{ID: "e2e-stream-6", Title: "E2E Stream Six"},
	{ID: "e2e-stream-7", Title: "E2E Stream Seven"},
	{ID: "e2e-stream-8", Title: "E2E Stream Eight"},
	{ID: "e2e-stream-9", Title: "E2E Stream Nine"},
}

// e2eTrackByID reports whether the fake answers to a provider track id, which
// is any of the streaming songs or the one the bar test downloads.
func e2eTrackByID(id string) (string, bool) {
	if id == E2ETrackID {
		return E2ETitle, true
	}
	for _, track := range e2eStreamTracks {
		if track.ID == id {
			return track.Title, true
		}
	}
	return "", false
}

// e2eStreamLog marks a stream request in the server's own log: the one place a
// browser test can see what a provider was asked for that the request URL does
// not carry. The suite reads it back from the backend process's log, so the
// spelling is a contract with `e2e/test_streaming.py`.
const e2eStreamLogPrefix = "e2e-slow: stream start="

// NewE2E returns the fake source: the provider a test build has and a released
// one does not.
func NewE2E() Provider { return e2eProvider{} }

type e2eProvider struct{}

func (e2eProvider) Name() string { return E2EProviderName }

// Capabilities: it searches, it downloads, and it streams. Search and download
// are what the bar test drives; Stream is what lets a not-yet-downloaded song
// start playing at once, and it is what the streaming tests exercise.
func (e2eProvider) Capabilities() Caps { return Caps{Search: true, Download: true, Stream: true} }

// Search answers the queries a test writes and nothing else, so the fake never
// shows up in a search the suite is about something else.
//
// A query naming one of the streaming songs answers with that one; anything
// else that says "e2e" answers with the song the bar test downloads.
func (e2eProvider) Search(ctx context.Context, q string, opts SearchOpts) ([]Track, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lower := strings.ToLower(q)
	if !strings.Contains(lower, "e2e") {
		return nil, nil
	}
	for _, track := range e2eStreamTracks {
		if strings.Contains(lower, strings.ToLower(track.Title)) {
			return []Track{{
				ProviderTrackID: track.ID,
				Title:           track.Title,
				Artists:         []string{E2EArtist},
				DurationMs:      E2EDurationMs,
			}}, nil
		}
	}
	return []Track{{
		ProviderTrackID: E2ETrackID,
		Title:           E2ETitle,
		Artists:         []string{E2EArtist},
		DurationMs:      E2EDurationMs,
	}}, nil
}

// Download fetches nothing and takes seconds: it writes a generated sine into
// a temporary file beside destPath in chunks, reporting what a real provider
// reports as it goes, and then converts that source onto destPath.
//
// The source is 30 s of 48 kHz mono PCM - a fixed number of bytes - so the
// download stage's Done and Total are real sizes that do not change between
// runs. The convert stage reports the song's own length in milliseconds while
// the sine is transcoded to the Ogg/Opus the manager stores at destPath, which
// is what a real provider's own download does.
//
// The fetched source deliberately does *not* go to destPath. A rendition whose
// file is still being written has a range request answered from destPath itself
// (the manager's `.part`), so a provider whose raw container sat there for the
// whole fetch would hand a listener a WAV where the response says Ogg. Fetching
// to a source file and converting onto destPath is both what a real provider
// does and what keeps a request made during a download answered from the
// source's own stream instead of from half a conversion.
func (e2eProvider) Download(ctx context.Context, providerTrackID, destPath string, progress ProgressFunc) error {
	if _, ok := e2eTrackByID(providerTrackID); !ok {
		return fmt.Errorf("e2e-slow: unknown track %q", providerTrackID)
	}
	report := func(p Progress) {
		if progress != nil {
			progress(p)
		}
	}

	source := sineWAV(E2EDurationMs/1000, 440, e2eSampleRate)
	total := int64(len(source))

	// The fetch: the source arrives into a temporary file, the way a provider's
	// download stages what it fetched before the conversion writes the file the
	// server stores.
	fetched := destPath + ".source"
	defer func() { _ = os.Remove(fetched) }()
	file, err := os.OpenFile(fetched, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("e2e-slow: create %s: %w", fetched, err)
	}
	// The first report is before any byte: the file has a size the moment the
	// fetch starts, and a bar that waits a tick for it would sweep when it
	// could fill.
	report(Progress{Stage: StageDownload, Done: 0, Total: total})

	err = writeChunks(ctx, file, source, total, report)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return fmt.Errorf("e2e-slow: close %s: %w", fetched, closeErr)
	}
	// The fetch is done, and reported as done: the source is whole for a moment
	// before the conversion starts, so a client polling twice a second sees the
	// download finish rather than the stage changing under 100%.
	if err := pause(ctx, e2eSettlePause); err != nil {
		return err
	}

	// The convert: three reports of the song's own length, so the numbers a
	// client draws are milliseconds of audio and the stage is visible for
	// longer than one poll.
	for i := 1; i <= e2eConvertReports; i++ {
		report(Progress{
			Stage: StageConvert,
			Done:  E2EDurationMs * int64(i) / int64(e2eConvertReports),
			Total: E2EDurationMs,
		})
		if err := pause(ctx, e2eConvertPause); err != nil {
			return err
		}
	}

	// The real file: the Opus the manager probes and stores is written onto
	// destPath from the fetched source.
	if err := ffmpeg.ToOpus(ctx, fetched, destPath); err != nil {
		return fmt.Errorf("e2e-slow: transcode: %w", err)
	}
	return nil
}

// Stream hands out a song's audio as it is asked for, and the request *is* the
// answer: a listener who asks for the middle gets the middle, and the rest of
// the song after it.
//
// The audio is a generated sine in an Ogg/Opus container - the format the
// download ends up in, so what a player hears while downloading is what it will
// hear from the file. The bytes go out a piece at a time with a pause between
// pieces, which is what makes "the song started before its file existed"
// observable: a stream is still arriving while the download beside it finishes.
//
// startMs begins the returned audio that far into the song, by cutting the head
// off the generated tone rather than regenerating it - what a provider's own
// `--download-sections` does. The offset is written to the process log, so a
// test can assert where the provider was told to start rather than only what a
// URL said.
func (e2eProvider) Stream(ctx context.Context, providerTrackID string, startMs int64) (io.ReadCloser, string, error) {
	if _, ok := e2eTrackByID(providerTrackID); !ok {
		return nil, "", fmt.Errorf("e2e-slow: unknown track %q", providerTrackID)
	}
	if startMs < 0 {
		startMs = 0
	}
	if startMs > E2EDurationMs {
		startMs = E2EDurationMs
	}
	// The process log is the server's, and this is an e2e-only fake: a line per
	// request is cheap, and it is the only place the offset a provider received
	// can be read from a browser test.
	fmt.Fprintf(os.Stderr, "%s%d\n", e2eStreamLogPrefix, startMs)

	audio, err := e2eSineSection(ctx, startMs)
	if err != nil {
		return nil, "", err
	}

	reader, writer := io.Pipe()
	go func() {
		for offset := 0; offset < len(audio); offset += e2eStreamChunk {
			end := offset + e2eStreamChunk
			if end > len(audio) {
				end = len(audio)
			}
			if _, err := writer.Write(audio[offset:end]); err != nil {
				return
			}
			if err := pause(ctx, e2eStreamPause); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
		}
		_ = writer.Close()
	}()
	return reader, e2eStreamMIME, nil
}

// e2eSineSection encodes the sine from startMs to the end of the song as
// Ogg/Opus, in one ffmpeg run whose output is held in memory: the fake is a
// provider, not a server, and the pacing is what makes it a stream.
func e2eSineSection(ctx context.Context, startMs int64) ([]byte, error) {
	total := float64(E2EDurationMs) / 1000
	start := float64(startMs) / 1000
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=440:sample_rate=%d:duration=%.3f", e2eSampleRate, total),
		"-af", fmt.Sprintf("atrim=start=%.3f,asetpts=PTS-STARTPTS", start),
		"-c:a", "libopus", "-b:a", e2eStreamBitrate,
		"-f", "ogg", "pipe:1",
	)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("e2e-slow: stream audio: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if out.Len() == 0 {
		return nil, fmt.Errorf("e2e-slow: stream audio: ffmpeg wrote nothing")
	}
	return out.Bytes(), nil
}

// writeChunks copies the fetched source into destPath a piece at a time,
// reporting the running total and waiting between pieces so the download takes
// long enough to watch.
func writeChunks(ctx context.Context, file *os.File, source []byte, total int64, report func(Progress)) error {
	size := (len(source) + e2eChunks - 1) / e2eChunks
	var written int64
	for offset := 0; offset < len(source); offset += size {
		if err := pause(ctx, e2eChunkPause); err != nil {
			return err
		}
		end := offset + size
		if end > len(source) {
			end = len(source)
		}
		n, err := file.Write(source[offset:end])
		written += int64(n)
		if err != nil {
			return fmt.Errorf("e2e-slow: write: %w", err)
		}
		report(Progress{Stage: StageDownload, Done: written, Total: total})
	}
	return nil
}

// pause waits, or gives up when the caller is done. A download that ignores its
// context keeps writing after the song was skipped.
func pause(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// sineWAV is a generated sine as an in-memory WAV: a header and PCM samples,
// the bytes a provider would have fetched. Same input, same bytes, every time.
func sineWAV(seconds, hz, rate int) []byte {
	samples := seconds * rate
	out := make([]byte, 44+samples*2)
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+samples*2))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)             // fmt chunk size
	binary.LittleEndian.PutUint16(out[20:], 1)              // PCM
	binary.LittleEndian.PutUint16(out[22:], 1)              // mono
	binary.LittleEndian.PutUint32(out[24:], uint32(rate))   // sample rate
	binary.LittleEndian.PutUint32(out[28:], uint32(rate*2)) // byte rate
	binary.LittleEndian.PutUint16(out[32:], 2)              // block align
	binary.LittleEndian.PutUint16(out[34:], 16)             // bits per sample
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(samples*2))
	for i := range samples {
		value := math.Sin(2 * math.Pi * float64(hz) * float64(i) / float64(rate))
		binary.LittleEndian.PutUint16(out[44+i*2:], uint16(int16(value*0.35*32767)))
	}
	return out
}
