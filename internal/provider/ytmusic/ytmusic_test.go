package ytmusic

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeberg.org/kyleraykbs/prismusic/internal/provider"
)

// stubPython writes an executable that ignores its stdin and runs body.
func stubPython(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "python3")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testProvider(t *testing.T, python string) *Provider {
	t.Helper()
	p := New(nil)
	p.python = python
	return p
}

func TestSearchParsesHelperOutput(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "hits.json")
	const body = `[{"id":"abc","title":"Song","artists":["Artist"],"album":"Album","durationMs":180000},
	                 {"id":"","title":"No id"} ,
	                 {"id":"def","title":"Other","artists":["A","B"],"album":"","durationMs":0}]`
	if err := os.WriteFile(fixture, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YT_FIXTURE", fixture)

	p := testProvider(t, stubPython(t, `cat "$YT_FIXTURE"`))
	tracks, err := p.Search(context.Background(), "query", provider.SearchOpts{Limit: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(tracks) != 2 {
		t.Fatalf("tracks = %d, want 2 (entries without an id are dropped)", len(tracks))
	}
	first := tracks[0]
	if first.ProviderTrackID != "abc" || first.Title != "Song" || first.Album != "Album" || first.DurationMs != 180_000 {
		t.Errorf("first = %+v", first)
	}
	if len(first.Artists) != 1 || first.Artists[0] != "Artist" {
		t.Errorf("artists = %v", first.Artists)
	}
	if len(tracks[1].Artists) != 2 {
		t.Errorf("second artists = %v", tracks[1].Artists)
	}
}

func TestSearchReportsHelperFailure(t *testing.T) {
	p := testProvider(t, stubPython(t, `echo "boom" >&2; exit 1`))
	_, err := p.Search(context.Background(), "query", provider.SearchOpts{})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry the helper's stderr, got %v", err)
	}
}

func TestSearchRejectsGarbageOutput(t *testing.T) {
	p := testProvider(t, stubPython(t, `echo "not json"`))
	if _, err := p.Search(context.Background(), "query", provider.SearchOpts{}); err == nil {
		t.Fatal("want parse error, got nil")
	}
}

func TestDownloadFailsCleanlyWithoutYtdlp(t *testing.T) {
	p := testProvider(t, stubPython(t, `exit 0`))
	p.ytdlp = filepath.Join(t.TempDir(), "missing-yt-dlp")

	err := p.Download(context.Background(), "abc", filepath.Join(t.TempDir(), "out.opus"))
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "yt-dlp") {
		t.Errorf("error should mention yt-dlp, got %v", err)
	}
}

func TestMissingDepsListsBinaries(t *testing.T) {
	p := testProvider(t, "/nonexistent/python3")
	p.ytdlp = "/nonexistent/yt-dlp"

	missing := p.MissingDeps()
	if len(missing) == 0 {
		t.Fatal("want missing binaries")
	}
	if !contains(missing, "/nonexistent/python3") || !contains(missing, "/nonexistent/yt-dlp") {
		t.Errorf("missing = %v", missing)
	}
}

func TestCapabilities(t *testing.T) {
	caps := New(nil).Capabilities()
	if !caps.Search || !caps.Download {
		t.Errorf("caps = %+v, want search and download", caps)
	}
	if New(nil).Name() != Name {
		t.Errorf("name = %q", New(nil).Name())
	}
}

func TestFirstAudioFileSkipsThumbnailsAndParts(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"abc.webp", "abc.part", "abc.ytdl", "abc.webm"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := firstAudioFile(dir)
	if err != nil {
		t.Fatalf("firstAudioFile: %v", err)
	}
	if filepath.Base(got) != "abc.webm" {
		t.Errorf("got %s, want abc.webm", got)
	}

	if _, err := firstAudioFile(t.TempDir()); err == nil {
		t.Error("empty dir must be an error")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

var _ provider.Provider = (*Provider)(nil)
var _ provider.DependencyChecker = (*Provider)(nil)

var errUnused = errors.New("")

func TestSearchKeepsArtwork(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "hits.json")
	const body = `[{"id":"abc","title":"Song","artists":["Artist"],"album":"Album","durationMs":180000,
	                "artworkUrl":"https://lh3.googleusercontent.com/cover=w544-h544"}]`
	if err := os.WriteFile(fixture, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YT_FIXTURE", fixture)

	tracks, err := testProvider(t, stubPython(t, `cat "$YT_FIXTURE"`)).
		Search(context.Background(), "query", provider.SearchOpts{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(tracks) != 1 || tracks[0].ArtworkURL != "https://lh3.googleusercontent.com/cover=w544-h544" {
		t.Fatalf("tracks = %+v", tracks)
	}
}

func TestAlbumAndArtistArtwork(t *testing.T) {
	albums := filepath.Join(t.TempDir(), "albums.json")
	if err := os.WriteFile(albums, []byte(`[{"id":"MPREb","title":"Album","artists":["Artist"],"year":"1987","trackCount":10,"artworkUrl":"https://yt.example/album.jpg"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YT_FIXTURE", albums)

	p := testProvider(t, stubPython(t, `cat "$YT_FIXTURE"`))
	found, err := p.SearchAlbums(context.Background(), "album", provider.SearchOpts{})
	if err != nil {
		t.Fatalf("SearchAlbums: %v", err)
	}
	if len(found) != 1 || found[0].ArtworkURL != "https://yt.example/album.jpg" {
		t.Fatalf("albums = %+v", found)
	}

	artists := filepath.Join(t.TempDir(), "artists.json")
	if err := os.WriteFile(artists, []byte(`[{"id":"UC1","name":"Artist","artworkUrl":"https://yt.example/artist.jpg"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YT_FIXTURE", artists)

	found2, err := p.SearchArtists(context.Background(), "artist", provider.SearchOpts{})
	if err != nil {
		t.Fatalf("SearchArtists: %v", err)
	}
	if len(found2) != 1 || found2[0].ArtworkURL != "https://yt.example/artist.jpg" {
		t.Fatalf("artists = %+v", found2)
	}
}
