package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLyricsRoundTripByRendition(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	track := mustTrack(t, db, "Song")
	variant := mustVariant(t, db, track.ID, "ytmusic", "abc")

	if _, err := db.Lyrics(ctx, track.ID, variant.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty cache: err = %v, want ErrNotFound", err)
	}

	fetched := time.UnixMilli(1_700_000_000_000)
	saved := &Lyrics{
		TrackID:    track.ID,
		VariantID:  variant.ID,
		DurationMs: 245_000,
		Synced:     []LyricLine{{AtMs: 0, Text: "first"}, {AtMs: 12_500, Text: "second"}},
		Plain:      "first\nsecond",
		Source:     "lrclib",
		FetchedAt:  fetched,
	}
	if err := db.SaveLyrics(ctx, saved); err != nil {
		t.Fatalf("SaveLyrics: %v", err)
	}

	got, err := db.Lyrics(ctx, track.ID, variant.ID)
	if err != nil {
		t.Fatalf("Lyrics: %v", err)
	}
	if got.DurationMs != 245_000 || got.Plain != "first\nsecond" || got.Source != "lrclib" {
		t.Errorf("got %+v", got)
	}
	if !got.FetchedAt.Equal(fetched) {
		t.Errorf("fetched at %v, want %v", got.FetchedAt, fetched)
	}
	if len(got.Synced) != 2 || got.Synced[1].AtMs != 12_500 || got.Synced[1].Text != "second" {
		t.Errorf("synced round-trip: %+v", got.Synced)
	}
}

func TestLyricsCacheSeparatesRenditions(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	track := mustTrack(t, db, "Song")
	variant := mustVariant(t, db, track.ID, "ytmusic", "abc")

	// The same track asked about without naming a copy is a different answer:
	// it is filed under the zero id, not under the variant's.
	if err := db.SaveLyrics(ctx, &Lyrics{
		TrackID: track.ID, VariantID: uuid.Nil, DurationMs: 180_000,
		Plain: "track words", Source: "lrclib",
	}); err != nil {
		t.Fatalf("SaveLyrics(no variant): %v", err)
	}
	if err := db.SaveLyrics(ctx, &Lyrics{
		TrackID: track.ID, VariantID: variant.ID, DurationMs: 245_000,
		Plain: "variant words", Source: "lrclib",
	}); err != nil {
		t.Fatalf("SaveLyrics(variant): %v", err)
	}

	bare, err := db.Lyrics(ctx, track.ID, uuid.Nil)
	if err != nil {
		t.Fatalf("Lyrics(no variant): %v", err)
	}
	if bare.Plain != "track words" || bare.DurationMs != 180_000 {
		t.Errorf("bare rendition = %+v", bare)
	}
	named, err := db.Lyrics(ctx, track.ID, variant.ID)
	if err != nil {
		t.Fatalf("Lyrics(variant): %v", err)
	}
	if named.Plain != "variant words" || named.DurationMs != 245_000 {
		t.Errorf("named rendition = %+v", named)
	}
}

func TestSaveLyricsReplacesAnEarlierAnswer(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	track := mustTrack(t, db, "Song")

	// An instrumental is an answer with no words, so saving one after words
	// must take their place rather than leaving the stale words behind.
	if err := db.SaveLyrics(ctx, &Lyrics{
		TrackID: track.ID, VariantID: uuid.Nil, Synced: []LyricLine{{AtMs: 0, Text: "words"}},
		Plain: "words", Source: "lrclib",
	}); err != nil {
		t.Fatalf("SaveLyrics: %v", err)
	}
	if err := db.SaveLyrics(ctx, &Lyrics{
		TrackID: track.ID, VariantID: uuid.Nil, DurationMs: 211_000, Source: "lrclib-instrumental",
		FetchedAt: time.UnixMilli(1),
	}); err != nil {
		t.Fatalf("SaveLyrics(instrumental): %v", err)
	}

	got, err := db.Lyrics(ctx, track.ID, uuid.Nil)
	if err != nil {
		t.Fatalf("Lyrics: %v", err)
	}
	if got.Source != "lrclib-instrumental" || len(got.Synced) != 0 || got.Plain != "" {
		t.Errorf("instrumental did not replace words: %+v", got)
	}
}
