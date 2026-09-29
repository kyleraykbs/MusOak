package store

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestVariantVoteLifecycle(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	kyle := mustUserNamed(t, db, "kyle")
	other := mustUserNamed(t, db, "someone")

	track := mustTrack(t, db, "Song")
	voted := mustVariant(t, db, track.ID, "ytmusic", "v1")
	rival := mustVariant(t, db, track.ID, "spotify", "v2")
	elsewhere := mustVariant(t, db, mustTrack(t, db, "Other").ID, "ytmusic", "v3")

	// A fresh vote lands as one up vote.
	if err := db.SetVariantVote(ctx, kyle.ID, voted.ID, 1); err != nil {
		t.Fatalf("SetVariantVote: %v", err)
	}
	mine, err := db.UserVariantVotes(ctx, kyle.ID, track.ID)
	if err != nil {
		t.Fatalf("UserVariantVotes: %v", err)
	}
	if len(mine) != 1 || mine[voted.ID] != 1 {
		t.Fatalf("votes = %v, want {voted: 1}", mine)
	}
	counts, err := db.VariantVoteCounts(ctx, track.ID)
	if err != nil {
		t.Fatalf("VariantVoteCounts: %v", err)
	}
	if counts[voted.ID] != (VariantVoteCount{Up: 1}) {
		t.Fatalf("counts = %v, want one up vote", counts)
	}

	// A second user adds their own; votes on another track stay out of the
	// tally.
	if err := db.SetVariantVote(ctx, other.ID, voted.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.SetVariantVote(ctx, other.ID, rival.ID, -1); err != nil {
		t.Fatal(err)
	}
	if err := db.SetVariantVote(ctx, other.ID, elsewhere.ID, 1); err != nil {
		t.Fatal(err)
	}
	counts, _ = db.VariantVoteCounts(ctx, track.ID)
	if len(counts) != 2 || counts[voted.ID] != (VariantVoteCount{Up: 2}) || counts[rival.ID] != (VariantVoteCount{Down: 1}) {
		t.Fatalf("counts = %v, want two up on voted and one down on rival", counts)
	}

	// Voting again changes the one vote instead of adding a second.
	if err := db.SetVariantVote(ctx, kyle.ID, voted.ID, -1); err != nil {
		t.Fatal(err)
	}
	mine, _ = db.UserVariantVotes(ctx, kyle.ID, track.ID)
	if len(mine) != 1 || mine[voted.ID] != -1 {
		t.Fatalf("votes after change = %v, want {voted: -1}", mine)
	}
	counts, _ = db.VariantVoteCounts(ctx, track.ID)
	if counts[voted.ID] != (VariantVoteCount{Up: 1, Down: 1}) {
		t.Fatalf("counts = %v, want one up and one down", counts)
	}

	// Withdrawing deletes the row, and doing it twice is fine.
	if err := db.SetVariantVote(ctx, kyle.ID, voted.ID, 0); err != nil {
		t.Fatal(err)
	}
	if err := db.SetVariantVote(ctx, kyle.ID, voted.ID, 0); err != nil {
		t.Fatalf("second withdraw: %v", err)
	}
	mine, _ = db.UserVariantVotes(ctx, kyle.ID, track.ID)
	if len(mine) != 0 {
		t.Fatalf("votes after withdraw = %v, want none", mine)
	}
	counts, _ = db.VariantVoteCounts(ctx, track.ID)
	if counts[voted.ID] != (VariantVoteCount{Up: 1}) {
		t.Fatalf("counts = %v, want only the other user's vote left", counts)
	}

	// Only -1, 0 and 1 are votes; anything else is a caller bug.
	if err := db.SetVariantVote(ctx, kyle.ID, voted.ID, 2); err == nil {
		t.Error("vote value 2 was accepted")
	}
	// Voting needs a variant that exists.
	if err := db.SetVariantVote(ctx, kyle.ID, uuid.New(), 1); !errors.Is(err, ErrConflict) {
		t.Errorf("vote on missing variant: err = %v, want conflict", err)
	}
}

func TestTrackPreference(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	kyle := mustUserNamed(t, db, "kyle")
	other := mustUserNamed(t, db, "someone")

	track := mustTrack(t, db, "Song")
	first := mustVariant(t, db, track.ID, "ytmusic", "v1")
	second := mustVariant(t, db, track.ID, "spotify", "v2")

	// Nobody has chosen anything yet.
	got, err := db.TrackPreference(ctx, kyle.ID, track.ID)
	if err != nil {
		t.Fatalf("TrackPreference: %v", err)
	}
	if got != uuid.Nil {
		t.Fatalf("preference = %v, want none", got)
	}

	if err := db.SetTrackPreference(ctx, kyle.ID, track.ID, first.ID); err != nil {
		t.Fatalf("SetTrackPreference: %v", err)
	}
	if got, _ = db.TrackPreference(ctx, kyle.ID, track.ID); got != first.ID {
		t.Fatalf("preference = %v, want %v", got, first.ID)
	}

	// Saving again replaces the choice.
	if err := db.SetTrackPreference(ctx, kyle.ID, track.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.TrackPreference(ctx, kyle.ID, track.ID); got != second.ID {
		t.Fatalf("preference = %v, want %v", got, second.ID)
	}

	// The preference is personal: nobody else's row is touched.
	if got, _ = db.TrackPreference(ctx, other.ID, track.ID); got != uuid.Nil {
		t.Errorf("other's preference = %v, want none", got)
	}

	// A preference names a variant that exists.
	if err := db.SetTrackPreference(ctx, kyle.ID, track.ID, uuid.New()); !errors.Is(err, ErrConflict) {
		t.Errorf("preference for missing variant: err = %v, want conflict", err)
	}
}

func TestVariantUploaders(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	kyle := &User{Username: "kyle", PasswordHash: "x", DisplayName: "Kyle Ray"}
	if err := db.CreateUser(ctx, kyle); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	track := mustTrack(t, db, "Song")
	upload := mustVariant(t, db, track.ID, "user", "upload-1")
	official := mustVariant(t, db, track.ID, "ytmusic", "v1")
	// The uploader column is set when the upload is created; seeding it
	// directly keeps this test about reading it.
	if _, err := db.db.ExecContext(ctx,
		`UPDATE variants SET uploader_user_id = ? WHERE id = ?`,
		kyle.ID.String(), upload.ID.String()); err != nil {
		t.Fatalf("seed uploader: %v", err)
	}

	uploaders, err := db.VariantUploaders(ctx, track.ID)
	if err != nil {
		t.Fatalf("VariantUploaders: %v", err)
	}
	if len(uploaders) != 1 {
		t.Fatalf("uploaders = %v, want only the user source", uploaders)
	}
	got, ok := uploaders[upload.ID]
	if !ok {
		t.Fatalf("uploaders = %v, missing the uploaded variant", uploaders)
	}
	if got.UserID != kyle.ID || got.Username != "kyle" || got.DisplayName != "Kyle Ray" {
		t.Errorf("uploader = %+v, want kyle's account", got)
	}
	if _, ok := uploaders[official.ID]; ok {
		t.Errorf("provider variant %v unexpectedly has an uploader", official.ID)
	}
}
