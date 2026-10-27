package media

import (
	"context"
	"os"
	"time"
)

// How long a stored song stays, by what has been done with it.
//
// Kyle's policy, in his words: a song in a playlist or a favourite is kept
// locally for ever; a played song is kept for a week; a song more than one
// person has played is kept for three months, and each new person who listens
// adds a month to that up to a year - at which point a listen only resets the
// timer - and a song that was fetched but never played is kept for a week from
// its download.
//
// Every clock here runs from the newest listen, so a song played again is kept
// again: that is what "every time a person plays the song it should reset the
// timer" means, and it is the same rule for the one-person tier.
const (
	// retentionNeverPlayed is how long a fetched song nobody has listened to
	// survives: long enough for a prefetched queue to be wanted, short enough
	// that a skipped song does not sit on the disk for ever.
	retentionNeverPlayed = 7 * 24 * time.Hour
	// retentionPlayed is the one-listener tier.
	retentionPlayed = 7 * 24 * time.Hour
	// retentionShared is the two-listener tier - the "more than 1 person" rule -
	// and retentionPerListener is what each further listener adds.
	retentionShared      = 90 * 24 * time.Hour
	retentionPerListener = 30 * 24 * time.Hour
	// retentionSharedCap is where adding months stops: a song everybody keeps
	// coming back to stays a year past the last person who played it, and a
	// listen at that point only resets that year.
	retentionSharedCap = 365 * 24 * time.Hour
)

// KeepUntil says when a stored song may be deleted, from the facts the store
// keeps about it. A zero answer means for ever.
//
// downloaded is when the file landed, lastPlay the newest recorded listen (zero
// when nobody has played it), listeners how many distinct accounts have played
// it ever, and pinned whether it is in a playlist or a favourite - which is what
// makes an answer of "for ever".
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
	if listeners == 1 {
		return lastPlay.Add(retentionPlayed)
	}
	span := retentionShared + time.Duration(listeners-2)*retentionPerListener
	if span > retentionSharedCap {
		span = retentionSharedCap
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
	now := time.Now()
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
		freed += row.Bytes
		m.logger.Info("media: removed a song whose retention ran out",
			"variant", row.VariantID, "bytes", row.Bytes, "listeners", row.Listeners,
			"lastPlay", row.LastPlay, "pinned", row.Pinned)
	}
	return freed
}

// SweepNow runs one full sweep - retention, then the quota - and answers how
// many bytes it freed. It is what the hourly timer calls, and what a test calls
// instead of waiting an hour.
func (m *Manager) SweepNow(ctx context.Context) int64 {
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
