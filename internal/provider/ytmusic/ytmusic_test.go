package ytmusic

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kyleraykbs/MusOak/internal/provider"
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

// stubStreamYtdlp writes an executable that records its arguments, answers the
// --print-to-file request with the given container, and streams some audio, the
// way a real yt-dlp fetch would.
func stubStreamYtdlp(t *testing.T, record, ext string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yt-dlp")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + record + `
while [ $# -gt 0 ]; do
  if [ "$1" = "--print-to-file" ]; then printf '` + ext + `\n' > "$3"; fi
  shift
done
printf 'streamed-audio-bytes'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// stubSilentYtdlp exits cleanly without reporting a container.
func stubSilentYtdlp(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "yt-dlp")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// Stream must build the same command Download builds - same format, same
// cookies, same extractor args - but with -o - sending the audio to stdout and
// --print-to-file recording the container.
func TestStreamPipesAudioWithDownloadFlags(t *testing.T) {
	record := filepath.Join(t.TempDir(), "args")
	p := testProvider(t, stubPython(t, `exit 0`))
	p.ytdlp = stubStreamYtdlp(t, record, "webm")
	p.cookiesFile = "/home/you/cookies.txt"

	body, mimeType, err := p.Stream(context.Background(), "abc")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer body.Close()
	if mimeType != "audio/webm" {
		t.Errorf("mimeType = %q, want audio/webm", mimeType)
	}
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if string(got) != "streamed-audio-bytes" {
		t.Errorf("audio = %q", got)
	}

	args, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("yt-dlp was not run: %v", err)
	}
	for _, want := range []string{
		"--no-playlist\n",
		"-f\nbestaudio\n",
		"-o\n-\n",
		"--print-to-file\n%(ext)s\n",
		"--cookies\n/home/you/cookies.txt\n",
		"https://music.youtube.com/watch?v=abc\n",
	} {
		if !strings.Contains(string(args), want) {
			t.Errorf("yt-dlp was not given %q:\n%s", strings.TrimSpace(want), args)
		}
	}
	// The embedded client is the fallback: the first attempt - the one that works
	// in the common case - leaves yt-dlp on its own defaults, which measure at
	// half the wall time.
	if strings.Contains(string(args), "youtube:player_client=web_embedded") {
		t.Errorf("the first attempt used the web_embedded client:\n%s", args)
	}
}

// The container yt-dlp reports maps to the MIME type the listener is told.
func TestStreamMapsContainerToMIME(t *testing.T) {
	for _, tc := range []struct{ ext, want string }{
		{"m4a", "audio/mp4"},
		{"opus", "audio/ogg"},
		{"ogg", "audio/ogg"},
	} {
		p := testProvider(t, stubPython(t, `exit 0`))
		p.ytdlp = stubStreamYtdlp(t, filepath.Join(t.TempDir(), "args"), tc.ext)
		body, mimeType, err := p.Stream(context.Background(), "abc")
		if err != nil {
			t.Fatalf("Stream(%s): %v", tc.ext, err)
		}
		body.Close()
		if mimeType != tc.want {
			t.Errorf("container %s: mimeType = %q, want %q", tc.ext, mimeType, tc.want)
		}
	}
}

// A container we cannot map is an error, never a guess.
func TestStreamRejectsUnknownContainer(t *testing.T) {
	p := testProvider(t, stubPython(t, `exit 0`))
	p.ytdlp = stubStreamYtdlp(t, filepath.Join(t.TempDir(), "args"), "flv")

	body, _, err := p.Stream(context.Background(), "abc")
	if err == nil {
		if body != nil {
			body.Close()
		}
		t.Fatal("want an error for an unknown container, got nil")
	}
	if body != nil {
		t.Errorf("body = %v, want nil alongside the error", body)
	}
}

// A fetch that ends before reporting its container is an error, not an empty
// stream.
func TestStreamFailsWhenContainerIsNeverReported(t *testing.T) {
	p := testProvider(t, stubPython(t, `exit 0`))
	p.ytdlp = stubSilentYtdlp(t)
	if body, _, err := p.Stream(context.Background(), "abc"); err == nil {
		if body != nil {
			body.Close()
		}
		t.Fatal("want an error when the container is never reported")
	} else if !strings.Contains(err.Error(), "container") {
		t.Errorf("error should mention the container, got %v", err)
	}
}

func TestStreamFailsCleanlyWithoutYtdlp(t *testing.T) {
	p := testProvider(t, stubPython(t, `exit 0`))
	p.ytdlp = filepath.Join(t.TempDir(), "missing-yt-dlp")
	if body, _, err := p.Stream(context.Background(), "abc"); err == nil {
		body.Close()
		t.Fatal("want error, got nil")
	}
}

// The reader owns the fetch: Close stops a stream that is still running and
// reaps the process, so nothing is left behind.
func TestStreamCloseEndsAndReapsTheProcess(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	script := "#!/bin/sh\n" + `
echo $$ > ` + pidFile + `
while [ $# -gt 0 ]; do
  if [ "$1" = "--print-to-file" ]; then printf 'webm\n' > "$3"; fi
  shift
done
printf 'audio-bytes'
exec sleep 60
`
	path := filepath.Join(t.TempDir(), "yt-dlp")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := testProvider(t, stubPython(t, `exit 0`))
	p.ytdlp = path

	body, _, err := p.Stream(context.Background(), "abc")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	buf := make([]byte, len("audio-bytes"))
	if _, err := io.ReadFull(body, buf); err != nil {
		t.Fatalf("read stream: %v", err)
	}

	start := time.Now()
	if err := body.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Close took %v; it did not stop the fetch", elapsed)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse pid %q: %v", raw, err)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		t.Errorf("process %d is still alive after Close", pid)
	}
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
	_ = p.Download(context.Background(), "abc", filepath.Join(t.TempDir(), "out.opus"), nil)

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

	_ = p.Download(context.Background(), "abc", filepath.Join(t.TempDir(), "out.opus"), nil)

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

	err := p.Download(context.Background(), "abc", filepath.Join(t.TempDir(), "out.opus"), nil)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "yt-dlp") {
		t.Errorf("error should mention yt-dlp, got %v", err)
	}
}

// The fetch is where a download spends its time, and it is where the bar used
// to sit still: yt-dlp's own progress lines have to come back as the bytes they
// count.
func TestFetchReportsYtdlpProgress(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "yt-dlp")
	script := "#!/bin/sh\n" +
		"printf '[download]  50.0%% of    4.00MiB at   2.10MiB/s ETA 00:02\\n'\n" +
		"printf 'audio' > " + filepath.Join(dir, "abc.webm") + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	p := testProvider(t, stubPython(t, `exit 0`))
	p.ytdlp = path

	var got []provider.Progress
	if _, err := p.fetch(context.Background(), "https://music.youtube.com/watch?v=abc", dir,
		func(pr provider.Progress) { got = append(got, pr) }); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	want := provider.Progress{Stage: provider.StageDownload, Done: 2097152, Total: 4194304}
	if len(got) != 1 || got[0] != want {
		t.Errorf("progress = %+v, want one report of %+v", got, want)
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
	if !caps.Search || !caps.Download || !caps.Stream {
		t.Errorf("caps = %+v, want search, download and stream", caps)
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
var _ provider.Streamer = (*Provider)(nil)
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

// YouTube Music sizes a playlist the way its page does: "11K", "2.2K", "1.2M",
// or plain digits. The helper used to read one with int(), so a single large
// playlist in the results took the whole command down and the provider reported
// a failure for a number nothing depends on.
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
            {"browseId": "VL22K", "title": "Two point two", "itemCount": "2.2K"},
            {"browseId": "VL12M", "title": "A million and change", "itemCount": "1.2M"},
            {"browseId": "VL1234", "title": "Plain", "itemCount": 1234},
            {"browseId": "VLnone", "title": "Nothing readable", "itemCount": "many"},
        ]
`)))

	hits, err := p.SearchPlaylists(context.Background(), "anything", provider.SearchOpts{Limit: 5})
	if err != nil {
		t.Fatalf("a displayed count failed the search: %v", err)
	}
	if len(hits) != 5 {
		t.Fatalf("playlists = %d, want 5", len(hits))
	}
	for i, want := range []int{11000, 2200, 1200000, 1234, 0} {
		if hits[i].TrackCount != want {
			t.Errorf("%s trackCount = %d, want %d", hits[i].Title, hits[i].TrackCount, want)
		}
	}
}

// The same endpoints spell a length as a number and a thumbnail as "1280px".
// None of it is worth failing a search over, so an unreadable one reads as
// nothing and the rest of the answer is still used.
func TestAlbumSearchSurvivesUnreadableNumbers(t *testing.T) {
	p := testProvider(t, shimPython(t, realPython(t), stubYTMusicAPI(t, `
class YTMusic:
    def __init__(self, *args, **kwargs):
        pass

    def get_album(self, browse_id):
        return {
            "title": "Album",
            "artists": [{"name": "Artist"}],
            "year": "1987",
            "trackCount": "1,234",
            "thumbnails": [
                {"url": "junk.jpg", "width": "1280px", "height": "720px"},
                {"url": "real.jpg", "width": 1280, "height": 720},
            ],
            "tracks": [
                {"videoId": "one", "title": "One", "duration_seconds": "245.6"},
                {"videoId": "two", "title": "Two", "duration": "3:45"},
                {"videoId": "three", "title": "Three", "duration": "3:4x"},
            ],
        }
`)))

	album, err := p.Album(context.Background(), "MPREb")
	if err != nil {
		t.Fatalf("an unreadable number failed the album: %v", err)
	}
	if album.TrackCount != 1234 {
		t.Errorf("trackCount = %d, want 1234", album.TrackCount)
	}
	if album.ArtworkURL != "real.jpg" {
		t.Errorf("artworkUrl = %q, want the thumbnail with a readable size", album.ArtworkURL)
	}
	want := []int64{245_600, 225_000, 0}
	if len(album.Tracks) != len(want) {
		t.Fatalf("tracks = %d, want %d", len(album.Tracks), len(want))
	}
	for i, duration := range want {
		if album.Tracks[i].DurationMs != duration {
			t.Errorf("%s duration = %d, want %d", album.Tracks[i].Title, album.Tracks[i].DurationMs, duration)
		}
	}
}
