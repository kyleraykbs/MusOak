package ranking

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"codeberg.org/kyleraykbs/musoak/internal/store"
)

var defaults = []string{"ytmusic", "spotify"}

func newTestService(t *testing.T) (*Service, *store.DB) {
	t.Helper()
	db, err := store.Open("file:ranking-" + uuid.NewString() + "?mode=memory&cache=shared")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db, defaults, slog.New(slog.DiscardHandler)), db
}

func mustUser(t *testing.T, db *store.DB, name string) uuid.UUID {
	t.Helper()
	user := &store.User{Username: name, PasswordHash: "x"}
	if err := db.CreateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	return user.ID
}

func assertOrder(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

func TestFallsBackToDefaultsWithoutRankings(t *testing.T) {
	svc, _ := newTestService(t)
	order, err := svc.Aggregate(context.Background())
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	// The reserved slots lead the configured default: your own copy first,
	// the household's favourite second.
	assertOrder(t, order, []string{"self", "uploaded", "ytmusic", "spotify"})
}

func TestUserPrefersOwnRanking(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	ranked := mustUser(t, db, "ranked")
	unranked := mustUser(t, db, "unranked")

	if err := svc.Set(ctx, ranked, []string{"local", "spotify"}); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := svc.User(ctx, ranked)
	if err != nil {
		t.Fatal(err)
	}
	assertOrder(t, got, []string{"self", "uploaded", "local", "spotify"})

	got, err = svc.User(ctx, unranked)
	if err != nil {
		t.Fatal(err)
	}
	// The unranked user follows the aggregate, which spans the defaults too.
	assertOrder(t, got, []string{"self", "uploaded", "local", "spotify", "ytmusic"})
}

func TestAggregateShiftsAsRankingsChange(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	a := mustUser(t, db, "a")
	if err := svc.Set(ctx, a, []string{"spotify", "ytmusic", "local"}); err != nil {
		t.Fatal(err)
	}

	order, err := svc.Aggregate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// spotify 0, ytmusic 1/3, local 2/3.
	assertOrder(t, order, []string{"self", "uploaded", "spotify", "ytmusic", "local"})

	b := mustUser(t, db, "b")
	if err := svc.Set(ctx, b, []string{"local", "spotify", "ytmusic"}); err != nil {
		t.Fatal(err)
	}

	order, err = svc.Aggregate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// spotify (0+1/3)/2=0.167, local (2/3+0)/2=0.333, ytmusic (1/3+2/3)/2=0.5.
	assertOrder(t, order, []string{"self", "uploaded", "spotify", "local", "ytmusic"})

	// The aggregate is cached until a ranking changes.
	cached, err := svc.Aggregate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertOrder(t, cached, order)

	if err := svc.Set(ctx, b, []string{"spotify"}); err != nil {
		t.Fatal(err)
	}
	order, err = svc.Aggregate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// spotify (0+0)/2=0, ytmusic (1/3+1)/2=0.667, local (2/3+1)/2=0.833.
	assertOrder(t, order, []string{"self", "uploaded", "spotify", "ytmusic", "local"})
}

func TestUnrankedProvidersCountAsLast(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()

	a := mustUser(t, db, "a")
	if err := svc.Set(ctx, a, []string{"ytmusic"}); err != nil {
		t.Fatal(err)
	}

	order, err := svc.Aggregate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// ytmusic 0; spotify was not ranked by the only ranked user, so it counts
	// as last. "local" is not in the universe at all: nobody ranked it and it
	// is not in the configured defaults.
	assertOrder(t, order, []string{"self", "uploaded", "ytmusic", "spotify"})

	// Ranking local brings it into the universe, at the end of that user's
	// order until they rank it better.
	if err := svc.Set(ctx, a, []string{"ytmusic", "local"}); err != nil {
		t.Fatal(err)
	}
	order, err = svc.Aggregate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertOrder(t, order, []string{"self", "uploaded", "ytmusic", "local", "spotify"})
}

func TestSetValidatesInput(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	user := mustUser(t, db, "a")

	for name, providers := range map[string][]string{
		"empty":     {},
		"unknown":   {"deezer"},
		"duplicate": {"ytmusic", "ytmusic"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := svc.Set(ctx, user, providers); !errors.Is(err, ErrInvalidRanking) {
				t.Fatalf("err = %v, want ErrInvalidRanking", err)
			}
		})
	}
}

// TestEffectiveOrderCoversEnabledProviders is the account page's problem: a
// build enables a provider the configured default order predates, so the order
// the user gets must still name it.
func TestEffectiveOrderCoversEnabledProviders(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	// The build enables youtube, which the configured defaults do not name.
	svc.SetEnabledProviders([]string{"ytmusic", "youtube", "spotify"})

	// With nobody ranked, the configured default keeps its order and the missing
	// enabled provider is appended, behind the reserved slots.
	order, err := svc.Aggregate(ctx)
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	assertOrder(t, order, []string{"self", "uploaded", "ytmusic", "spotify", "youtube"})

	// A user's own ranking is completed the same way.
	user := mustUser(t, db, "ranked")
	if err := svc.Set(ctx, user, []string{"spotify", "ytmusic"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := svc.User(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	assertOrder(t, got, []string{"self", "uploaded", "spotify", "ytmusic", "youtube"})

	// The aggregate that had rankings is completed too.
	order, err = svc.Aggregate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertOrder(t, order, []string{"self", "uploaded", "spotify", "ytmusic", "youtube"})

	// An order that already names every enabled provider is left alone.
	svc.SetEnabledProviders([]string{"ytmusic", "spotify"})
	got, err = svc.User(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	assertOrder(t, got, []string{"self", "uploaded", "spotify", "ytmusic"})
}

// TestStoredRankingGainsSlots is the upgrade path: a ranking saved before the
// reserved slots existed comes back with self and uploaded at the front, and
// keeps the order its owner chose.
func TestStoredRankingGainsSlots(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	user := mustUser(t, db, "ranked")
	// Written straight to the store, the way an order saved by an older build
	// looks: no slots.
	if err := db.SetRanking(ctx, user, []string{"local", "spotify"}); err != nil {
		t.Fatalf("SetRanking: %v", err)
	}
	got, err := svc.User(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	assertOrder(t, got, []string{"self", "uploaded", "local", "spotify"})
}

// TestReservedSlotsAreRankable: the two reserved names are positions a user may
// rank themselves, kept exactly where they put them.
func TestReservedSlotsAreRankable(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	user := mustUser(t, db, "ranked")
	if err := svc.Set(ctx, user, []string{"uploaded", "spotify", "self", "ytmusic"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := svc.User(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	assertOrder(t, got, []string{"uploaded", "spotify", "self", "ytmusic"})
}

// TestAggregateIgnoresReservedSlots: slots are positions in a user's own list,
// not providers, so they never move a provider in the shared order.
func TestAggregateIgnoresReservedSlots(t *testing.T) {
	svc, db := newTestService(t)
	ctx := context.Background()
	withSlots := mustUser(t, db, "with")
	plain := mustUser(t, db, "plain")
	if err := svc.Set(ctx, withSlots, []string{"self", "uploaded", "spotify", "ytmusic"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Set(ctx, plain, []string{"spotify", "ytmusic"}); err != nil {
		t.Fatal(err)
	}
	order, err := svc.Aggregate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertOrder(t, order, []string{"self", "uploaded", "spotify", "ytmusic"})
}

func TestPickSelectsByEffectiveOrder(t *testing.T) {
	ytmusic := store.Variant{Provider: "ytmusic", Downloadable: true}
	local := store.Variant{Provider: "local", Downloadable: true}
	spotify := store.Variant{Provider: "spotify", Downloadable: false}

	// Metadata-only variants are never picked for playback.
	got, ok := Pick([]string{"spotify", "local", "ytmusic"}, []store.Variant{ytmusic, local, spotify})
	if !ok || got.Provider != "local" {
		t.Fatalf("Pick = %+v, %v; want local", got, ok)
	}

	got, ok = Pick([]string{"ytmusic", "local"}, []store.Variant{local, ytmusic})
	if !ok || got.Provider != "ytmusic" {
		t.Fatalf("Pick = %+v, %v; want ytmusic", got, ok)
	}

	// Providers outside the order come last, deterministically by name.
	got, ok = Pick(defaults, []store.Variant{local, ytmusic})
	if !ok || got.Provider != "ytmusic" {
		t.Fatalf("Pick = %+v, %v; want ytmusic", got, ok)
	}
	got, ok = Pick(defaults, []store.Variant{local})
	if !ok || got.Provider != "local" {
		t.Fatalf("Pick = %+v, %v; want the only downloadable variant", got, ok)
	}

	if _, ok := Pick(defaults, []store.Variant{spotify}); ok {
		t.Error("Pick must not return a non-downloadable variant")
	}
	if _, ok := Pick(defaults, nil); ok {
		t.Error("Pick must not return anything for an empty track")
	}
}
