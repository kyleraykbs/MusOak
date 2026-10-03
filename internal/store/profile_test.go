package store

import (
	"context"
	"testing"
)

// A new picture has to be countable. Its URL never changes - the file is filed
// under the path it is served from - so the version is the only thing a client
// can compare to know it should fetch the icon again.
func TestSetIconCountsTheChange(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	user := mustUserNamed(t, db, "kyle")

	before, err := db.User(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.IconVersion != 0 {
		t.Fatalf("a new account starts at version %d, want 0", before.IconVersion)
	}

	icon := "/api/v1/artwork/user/" + user.ID.String()
	if err := db.SetIcon(ctx, user.ID, icon); err != nil {
		t.Fatalf("SetIcon: %v", err)
	}
	if err := db.SetIcon(ctx, user.ID, icon); err != nil {
		t.Fatalf("SetIcon again: %v", err)
	}

	after, err := db.User(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.IconVersion != 2 {
		t.Errorf("version = %d after two uploads, want 2", after.IconVersion)
	}
	if after.IconURL != icon {
		t.Errorf("iconUrl = %q, want %q", after.IconURL, icon)
	}

	// A new name is not a new picture, and must not make everybody fetch one.
	if err := db.UpdateProfile(ctx, user.ID, "Ada", after.IconURL); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}
	named, err := db.User(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if named.IconVersion != after.IconVersion {
		t.Errorf("a name change moved the icon version to %d", named.IconVersion)
	}
	if named.DisplayName != "Ada" {
		t.Errorf("display name = %q, want Ada", named.DisplayName)
	}
}
