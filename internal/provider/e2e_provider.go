//go:build e2e

package provider

// A source that is always there and always the same, for the browser suite.
//
// The suite needs a download it can watch: every real provider is network,
// timing and luck, and a bar test written against one measures the internet.
// This one fetches nothing. It answers one query with one track, and its
// download is a fixed number of bytes written in fixed chunks with fixed
// pauses, reporting its own progress exactly the way a real provider does -
// bytes and a total while it downloads, milliseconds while it converts. The
// numbers are the same every run, so a test asserts that the bar moved and
// what it said, not that a race went its way.
//
// Built only with `-tags e2e`; see Extras in extras_e2e.go.

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
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
)

// NewE2E returns the fake source: the provider a test build has and a released
// one does not.
func NewE2E() Provider { return e2eProvider{} }

type e2eProvider struct{}

func (e2eProvider) Name() string { return E2EProviderName }

// Capabilities: it searches and it downloads. Nothing else - no albums, no
// playlists, no streaming - so a fan-out search that asks everything leaves no
// error line beside the results.
func (e2eProvider) Capabilities() Caps { return Caps{Search: true, Download: true} }

// Search answers the queries a test writes and nothing else, so the fake never
// shows up in a search the suite is about something else.
func (e2eProvider) Search(ctx context.Context, q string, opts SearchOpts) ([]Track, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !strings.Contains(strings.ToLower(q), "e2e") {
		return nil, nil
	}
	return []Track{{
		ProviderTrackID: E2ETrackID,
		Title:           E2ETitle,
		Artists:         []string{E2EArtist},
		DurationMs:      E2EDurationMs,
	}}, nil
}

// Download fetches nothing and takes seconds: it writes a generated sine to
// destPath in chunks, reporting what a real provider reports as it goes.
//
// The source is 30 s of 48 kHz mono PCM - a fixed number of bytes - so the
// download stage's Done and Total are real sizes that do not change between
// runs. The convert stage then reports the song's own length in milliseconds
// while the sine is transcoded to the Ogg/Opus the manager stores, and the
// finished file is moved onto destPath. A test can watch that whole story at
// the status endpoint and in the bar.
func (e2eProvider) Download(ctx context.Context, providerTrackID, destPath string, progress ProgressFunc) error {
	if providerTrackID != E2ETrackID {
		return fmt.Errorf("e2e-slow: unknown track %q", providerTrackID)
	}
	report := func(p Progress) {
		if progress != nil {
			progress(p)
		}
	}

	source := sineWAV(E2EDurationMs/1000, 440, e2eSampleRate)
	total := int64(len(source))

	file, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("e2e-slow: create %s: %w", destPath, err)
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
		return fmt.Errorf("e2e-slow: close %s: %w", destPath, closeErr)
	}
	// The fetch is done, and reported as done: the source is whole on disk for
	// a moment before the conversion starts, so a client polling twice a second
	// sees the download finish rather than the stage changing under 100%.
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

	// The real file: destPath still holds the fetched source, and the Opus the
	// manager probes and stores is written next to it and moved on top.
	opus := destPath + ".opus"
	defer func() { _ = os.Remove(opus) }()
	if err := ffmpeg.ToOpus(ctx, destPath, opus); err != nil {
		return fmt.Errorf("e2e-slow: transcode: %w", err)
	}
	if err := os.Rename(opus, destPath); err != nil {
		return fmt.Errorf("e2e-slow: move into place: %w", err)
	}
	return nil
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
