package media

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/google/uuid"

	"github.com/kyleraykbs/MusOak/internal/ffmpeg"
	"github.com/kyleraykbs/MusOak/internal/store"
)

// How long a stored song stays, by what has been done with it.
//
// Kyle's policy: a song in a playlist or a favourite is kept locally for ever; a
// played song is kept a month for each account that has played it - one listener
// a month, two two months, three three - up to a year; a song that was fetched
// but never played is kept a week from its download.
//
// Every clock runs from the newest listen, so a song played again is kept again:
// that is what "every time a person plays the song it should reset the timer"
// means.
const (
	// retentionNeverPlayed is how long a fetched song nobody has listened to
	// survives: long enough for a prefetched queue to be wanted, short enough
	// that a skipped song does not sit on the disk for ever.
	retentionNeverPlayed = 7 * 24 * time.Hour
	// retentionPerListener is what each account that has played a song adds:
	// one listener, one month.
	retentionPerListener = 30 * 24 * time.Hour
	// retentionCap is where adding months stops - twelve of them - and past
	// which a listen only resets the clock.
	retentionCap = 12 * retentionPerListener
)

// KeepUntil says when a stored song may be deleted, from the facts the store
// keeps about it. A zero answer means for ever.
//
// downloaded is when the file landed, lastPlay the newest recorded listen (zero
// when nobody has played it), listeners how many distinct accounts have played
// it ever, and pinned whether it is in a playlist or a favourite - which is what
// makes an answer of "for ever". One listener keeps a song a month, and every
// further listener another, up to twelve.
func KeepUntil(downloaded, lastPlay time.Time, listeners int, pinned bool) time.Time {
	if pinned {
		return time.Time{}
	}
	if listeners <= 0 || lastPlay.IsZero() {
		if downloaded.IsZero() {
			return time.Time{}
		}
		return downloaded.Add(retentionNeverPlayed)
	}
	span := time.Duration(listeners) * retentionPerListener
	if span > retentionCap {
		span = retentionCap
	}
	return lastPlay.Add(span)
}

// sweepRetention deletes every stored song whose retention has run out, and
// answers how many bytes it freed.
//
// A song somebody is in the middle of hearing is skipped and looked at again
// next hour: the bytes of a file being served are not the cache's to take, and
// a listener cut off mid-word pays for them again.
func (m *Manager) sweepRetention(ctx context.Context) int64 {
	rows, err := m.db.MediaRetention(ctx)
	if err != nil {
		m.logger.Warn("media: retention check failed", "error", err)
		return 0
	}
	now := m.now()
	freed := int64(0)
	for _, row := range rows {
		if ctx.Err() != nil {
			break
		}
		keep := KeepUntil(row.DownloadedAt, row.LastPlay, row.Listeners, row.Pinned)
		if keep.IsZero() || now.Before(keep) {
			continue
		}
		if m.isServing(row.VariantID) {
			continue
		}
		if err := os.Remove(row.Path); err != nil && !os.IsNotExist(err) {
			m.logger.Warn("media: could not remove an expired file", "path", row.Path, "error", err)
			continue
		}
		if err := m.db.DeleteMediaFile(ctx, row.VariantID); err != nil {
			m.logger.Warn("media: could not forget an expired file", "variant", row.VariantID, "error", err)
			continue
		}
		// The remembered "ready" goes with the file (see sweepQuota).
		m.setStatus(Status{VariantID: row.VariantID, State: StateNone})
		freed += row.Bytes
		m.logger.Info("media: removed a song whose retention ran out",
			"variant", row.VariantID, "bytes", row.Bytes, "listeners", row.Listeners,
			"lastPlay", row.LastPlay, "pinned", row.Pinned)
	}
	return freed
}

// loudnessBatch is how many files one hourly pass measures that were stored
// before loudness was, or that could not be measured when they arrived: about
// a second of ffmpeg each, so a library catches up over a few days without the
// pass ever taking long.
const loudnessBatch = 120

// measure records a file's loudness on it before it is stored. Best effort: a
// file that cannot be measured is stored all the same, unlevelled, and the
// hourly pass tries it again.
func (m *Manager) measure(ctx context.Context, file *store.MediaFile) {
	lufs, err := ffmpeg.Loudness(ctx, file.Path)
	if err != nil {
		m.logger.Debug("media: could not measure loudness", "variant", file.VariantID, "error", err)
		return
	}
	file.LoudnessLUFS, file.LoudnessKnown = lufs, true
}

// MeasureStored measures a file that was stored some other way than a download
// or an import - an upload writes its own row - so it is levelled from its
// first play rather than from the next hourly pass.
func (m *Manager) MeasureStored(ctx context.Context, variantID uuid.UUID, path string) {
	lufs, err := ffmpeg.Loudness(ctx, path)
	if err != nil {
		m.logger.Debug("media: could not measure loudness", "variant", variantID, "error", err)
		return
	}
	if err := m.db.SetMediaLoudness(ctx, variantID, lufs); err != nil {
		m.logger.Warn("media: record loudness", "variant", variantID, "error", err)
	}
}

// measureBacklog measures up to limit stored files that have no loudness yet,
// the most recently played first.
func (m *Manager) measureBacklog(ctx context.Context, limit int) {
	files, err := m.db.MediaWithoutLoudness(ctx, limit)
	if err != nil {
		m.logger.Warn("media: loudness backlog", "error", err)
		return
	}
	for _, file := range files {
		if ctx.Err() != nil {
			return
		}
		lufs, err := ffmpeg.Loudness(ctx, file.Path)
		if err != nil {
			continue
		}
		if err := m.db.SetMediaLoudness(ctx, file.VariantID, lufs); err != nil && !errors.Is(err, store.ErrNotFound) {
			m.logger.Warn("media: record loudness", "variant", file.VariantID, "error", err)
		}
	}
}

// SweepNow runs one full sweep - retention, then the quota - and answers how
// many bytes it freed. It is what the hourly timer calls, and what a test calls
// instead of waiting an hour.
func (m *Manager) SweepNow(ctx context.Context) int64 {
	m.measureBacklog(ctx, loudnessBatch)
	freed := m.sweepRetention(ctx)
	if m.quotaBytes <= 0 {
		return freed
	}
	before, err := m.db.MediaBytes(ctx)
	if err != nil {
		m.logger.Warn("media: quota check failed", "error", err)
		return freed
	}
	m.sweepQuota(ctx)
	after, err := m.db.MediaBytes(ctx)
	if err != nil {
		return freed
	}
	if before > after {
		freed += before - after
	}
	return freed
}

// StartRetention sweeps once now, then every hour until ctx is done.
//
// The first sweep is not an hour away on purpose: a server is restarted far more
// often than a year, and a cache full of songs whose retention ran out months ago
// would otherwise only be looked at by the next person who downloaded something.
func (m *Manager) StartRetention(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = time.Hour
	}
	go func() {
		sweep := func() {
			sctx, cancel := context.WithTimeout(ctx, evictionTimeout)
			defer cancel()
			m.SweepNow(sctx)
		}
		sweep()
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				sweep()
			}
		}
	}()
}
