package ytmusic

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"codeberg.org/kyleraykbs/musoak/internal/provider"
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
	p := New(nil, Options{})
	p.python = python
	return p
}

// stubYtdlp writes an executable that records the arguments it was given.
func stubYtdlp(t *testing.T, record string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yt-dlp")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + "\nexit 1\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// YouTube answers yt-dlp with "Sign in to confirm you're not a bot" and there is
// nothing to download until it can show cookies: the ones the provider is
// configured with have to reach the command.
func TestDownloadPassesConfiguredCookies(t *testing.T) {
	record := filepath.Join(t.TempDir(), "args")
	p := testProvider(t, stubPython(t, `exit 0`))
	p.ytdlp = stubYtdlp(t, record)
	p.cookiesFromBrowser = "firefox:/home/you/.librewolf/abc.default-release"
	p.cookiesFile = "/home/you/cookies.txt"

	// The stub writes no audio, so this fails at the end; the command it ran is
	// what this test is about.
	_ = p.Download(context.Background(), "abc", filepath.Join(t.TempDir(), "out.opus"))

	got, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("yt-dlp was not run: %v", err)
	}
	args := string(got)
	for _, want := range []string{
		"--cookies-from-browser\nfirefox:/home/you/.librewolf/abc.default-release\n",
		"--cookies\n/home/you/cookies.txt\n",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("yt-dlp was not given %q:\n%s", strings.TrimSpace(want), args)
		}
	}
}

// Without them the command stays as it was, so a plain setup is unaffected.
func TestDownloadWithoutCookiesRunsThePlainCommand(t *testing.T) {
	record := filepath.Join(t.TempDir(), "args")
	p := testProvider(t, stubPython(t, `exit 0`))
	p.ytdlp = stubYtdlp(t, record)

	_ = p.Download(context.Background(), "abc", filepath.Join(t.TempDir(), "out.opus"))

	got, _ := os.ReadFile(record)
	if strings.Contains(string(got), "--cookies") {
		t.Errorf("cookies were passed with none configured:\n%s", got)
	}
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

// YT Music's radio mixes songs and music videos, and a video's audio is the
// video: intro and all. A song wins wherever there is one.
func TestSearchKeepsSongsOverMusicVideos(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "hits.json")
	const body = `[{"id":"vid1","title":"Song (Official Music Video)","video":true},
	                 {"id":"song","title":"Song"},
	                 {"id":"vid2","title":"Another video","video":true}]`
	if err := os.WriteFile(fixture, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YT_FIXTURE", fixture)

	p := testProvider(t, stubPython(t, `cat "$YT_FIXTURE"`))
	tracks, err := p.Search(context.Background(), "query", provider.SearchOpts{Limit: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(tracks) != 1 || tracks[0].ProviderTrackID != "song" || tracks[0].Video {
		t.Fatalf("tracks = %+v, want only the song", tracks)
	}
}

// With nothing but videos on offer they are still better than nothing.
func TestSearchKeepsMusicVideosWhenTheyAreAllThereIs(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "hits.json")
	if err := os.WriteFile(fixture, []byte(`[{"id":"vid","title":"Only a video","video":true}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YT_FIXTURE", fixture)

	p := testProvider(t, stubPython(t, `cat "$YT_FIXTURE"`))
	tracks, err := p.Search(context.Background(), "query", provider.SearchOpts{Limit: 3})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(tracks) != 1 || !tracks[0].Video {
		t.Fatalf("tracks = %+v, want the video kept and marked", tracks)
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
	caps := New(nil, Options{}).Capabilities()
	if !caps.Search || !caps.Download {
		t.Errorf("caps = %+v, want search and download", caps)
	}
	if New(nil, Options{}).Name() != Name {
		t.Errorf("name = %q", New(nil, Options{}).Name())
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

// realPython is the interpreter the provider would use, or a skip: the helper
// tests need it, and the sandbox that builds this module may not have one.
func realPython(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not on PATH")
	}
	return python
}

// stubYTMusicAPI writes a ytmusicapi module under a directory of its own, so
// the helper's imports resolve to it without the real package installed.
func stubYTMusicAPI(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ytmusicapi.py"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// shimPython is a python3 that puts the stub module first on the import path.
// The provider passes the helper on stdin, so the arguments go through as they
// are; only PYTHONPATH is added.
func shimPython(t *testing.T, python, moduleDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "python3")
	script := "#!/bin/sh\nexec env PYTHONPATH=" + moduleDir + " " + python + " \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// YouTube Music sizes a playlist the way its page does: "11K", "1.2M", or plain
// digits. The helper used to read one with int(), so a single large playlist in
// the results took the whole command down and the provider reported a failure
// for a number nothing depends on.
func TestPlaylistSearchReadsADisplayedCount(t *testing.T) {
	p := testProvider(t, shimPython(t, realPython(t), stubYTMusicAPI(t, `
class YTMusic:
    def __init__(self, *args, **kwargs):
        pass

    def search(self, query, filter=None, limit=None):
        if filter != "playlists":
            return []
        return [
            {"browseId": "VL11K", "title": "Eleven thousand", "itemCount": "11K"},
            {"browseId": "VL12M", "title": "A million and change", "itemCount": "1.2M"},
            {"browseId": "VL1234", "title": "Plain", "itemCount": 1234},
        ]
`)))

	hits, err := p.SearchPlaylists(context.Background(), "anything", provider.SearchOpts{Limit: 5})
	if err != nil {
		t.Fatalf("a displayed count failed the search: %v", err)
	}
	if len(hits) != 3 {
		t.Fatalf("playlists = %d, want 3", len(hits))
	}
	for i, want := range []int{11000, 1200000, 1234} {
		if hits[i].TrackCount != want {
			t.Errorf("%s trackCount = %d, want %d", hits[i].Title, hits[i].TrackCount, want)
		}
	}
}
