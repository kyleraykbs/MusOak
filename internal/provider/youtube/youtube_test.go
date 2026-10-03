package youtube

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"codeberg.org/kyleraykbs/musoak/internal/provider"
)

// stubYtdlp writes an executable that runs body, so a test can stand in for the
// real yt-dlp.
func stubYtdlp(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yt-dlp")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func testProvider(t *testing.T, ytdlp string) *Provider {
	t.Helper()
	p := New(nil, Options{})
	p.ytdlp = ytdlp
	return p
}

// The fixture is real `yt-dlp --dump-json --flat-playlist` output: one JSON
// object per line. One entry has no duration and one has no title, which is not
// a usable hit.
func TestSearchParsesDumpJSON(t *testing.T) {
	const body = `{"id":"dQw4w9WgXcQ","title":"Never Gonna Give You Up","uploader":"Rick Astley","channel":"Rick Astley","duration":214.0,"thumbnails":[{"url":"https://i.ytimg.com/vi/dQw4w9WgXcQ/hq720.jpg","height":202},{"url":"https://i.ytimg.com/vi/dQw4w9WgXcQ/maxresdefault.jpg","height":404}]}
{"id":"noTitle","uploader":"Nobody","duration":100}
{"id":"noDuration","title":"Live Stream","channel":"Some Channel","duration":null}`

	fixture := filepath.Join(t.TempDir(), "hits.json")
	if err := os.WriteFile(fixture, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(t.TempDir(), "args")
	t.Setenv("YOUTUBE_FIXTURE", fixture)

	p := testProvider(t, stubYtdlp(t, `printf '%s\n' "$@" > `+record+`; cat "$YOUTUBE_FIXTURE"`))
	tracks, err := p.Search(context.Background(), "never gonna give you up", provider.SearchOpts{Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if len(tracks) != 2 {
		t.Fatalf("tracks = %d, want 2 (the entry without a title is dropped): %+v", len(tracks), tracks)
	}

	first := tracks[0]
	if first.ProviderTrackID != "dQw4w9WgXcQ" || first.Title != "Never Gonna Give You Up" {
		t.Errorf("first = %+v", first)
	}
	if len(first.Artists) != 1 || first.Artists[0] != "Rick Astley" {
		t.Errorf("artists = %v", first.Artists)
	}
	if first.DurationMs != 214_000 {
		t.Errorf("durationMs = %d, want 214000", first.DurationMs)
	}
	// The list is ordered smallest to largest; the best image is the last.
	if first.ArtworkURL != "https://i.ytimg.com/vi/dQw4w9WgXcQ/maxresdefault.jpg" {
		t.Errorf("artworkUrl = %q", first.ArtworkURL)
	}

	second := tracks[1]
	if second.ProviderTrackID != "noDuration" || second.Title != "Live Stream" {
		t.Errorf("second = %+v", second)
	}
	if second.DurationMs != 0 {
		t.Errorf("a missing duration must stay zero, got %d", second.DurationMs)
	}
	// uploader is absent, so the channel name stands in for the artist.
	if len(second.Artists) != 1 || second.Artists[0] != "Some Channel" {
		t.Errorf("second artists = %v", second.Artists)
	}

	args, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("yt-dlp was not run: %v", err)
	}
	for _, want := range []string{"--dump-json", "--flat-playlist", "ytsearch5:never gonna give you up"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("yt-dlp was not given %q:\n%s", want, args)
		}
	}
}

// Without an explicit limit the search still bounds itself, and the default
// feeds the ytsearchN: pseudo-URL.
func TestSearchUsesDefaultLimit(t *testing.T) {
	record := filepath.Join(t.TempDir(), "args")
	p := testProvider(t, stubYtdlp(t, `printf '%s\n' "$@" > `+record))
	if _, err := p.Search(context.Background(), "query", provider.SearchOpts{}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	args, _ := os.ReadFile(record)
	if !strings.Contains(string(args), "ytsearch"+strconv.Itoa(defaultSearchLimit)+":query") {
		t.Errorf("yt-dlp args = %s", args)
	}
}

// A missing binary is an ordinary error: it must never panic or come back nil.
func TestSearchFailsCleanlyWithoutYtdlp(t *testing.T) {
	p := testProvider(t, filepath.Join(t.TempDir(), "missing-yt-dlp"))
	tracks, err := p.Search(context.Background(), "query", provider.SearchOpts{})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if tracks != nil {
		t.Errorf("tracks = %+v, want nil alongside the error", tracks)
	}
	if !strings.Contains(err.Error(), "yt-dlp") {
		t.Errorf("error should mention yt-dlp, got %v", err)
	}
}

func TestSearchRejectsGarbageOutput(t *testing.T) {
	p := testProvider(t, stubYtdlp(t, `echo "not json"`))
	if _, err := p.Search(context.Background(), "query", provider.SearchOpts{}); err == nil {
		t.Fatal("want parse error, got nil")
	}
}

func TestSearchReportsYtdlpFailure(t *testing.T) {
	p := testProvider(t, stubYtdlp(t, `echo "boom" >&2; exit 1`))
	_, err := p.Search(context.Background(), "query", provider.SearchOpts{})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry yt-dlp's stderr, got %v", err)
	}
}

func TestDownloadPassesConfiguredCookies(t *testing.T) {
	record := filepath.Join(t.TempDir(), "args")
	p := testProvider(t, stubYtdlp(t, `printf '%s\n' "$@" > `+record+`; exit 1`))
	p.cookiesFromBrowser = "firefox:/home/you/.librewolf/abc.default-release"
	p.cookiesFile = "/home/you/cookies.txt"

	// The stub writes no audio, so this fails at the end; the command it ran is
	// what this test is about.
	_ = p.Download(context.Background(), "abc", filepath.Join(t.TempDir(), "out.opus"))

	args, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("yt-dlp was not run: %v", err)
	}
	for _, want := range []string{
		"--cookies-from-browser\nfirefox:/home/you/.librewolf/abc.default-release\n",
		"--cookies\n/home/you/cookies.txt\n",
	} {
		if !strings.Contains(string(args), want) {
			t.Errorf("yt-dlp was not given %q:\n%s", strings.TrimSpace(want), args)
		}
	}
}

func TestDownloadFailsCleanlyWithoutYtdlp(t *testing.T) {
	p := testProvider(t, filepath.Join(t.TempDir(), "missing-yt-dlp"))
	err := p.Download(context.Background(), "abc", filepath.Join(t.TempDir(), "out.opus"))
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "yt-dlp") {
		t.Errorf("error should mention yt-dlp, got %v", err)
	}
}

func TestMissingDepsListsYtdlp(t *testing.T) {
	p := testProvider(t, "/nonexistent/yt-dlp")
	if missing := p.MissingDeps(); !contains(missing, "/nonexistent/yt-dlp") {
		t.Errorf("missing = %v, want the yt-dlp path", missing)
	}
}

func TestCapabilitiesAndName(t *testing.T) {
	p := New(nil, Options{})
	if p.Name() != Name || Name != "youtube" {
		t.Errorf("name = %q, want youtube", p.Name())
	}
	caps := p.Capabilities()
	if !caps.Search || !caps.Download {
		t.Errorf("caps = %+v, want search and download", caps)
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
