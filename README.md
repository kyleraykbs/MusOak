# MusOak

A self-hosted music server, client and listening room. It searches several
providers, merges their results into one canonical library, downloads the audio
it needs as Ogg/Opus, and lets a room of people listen to the same thing at the
same time.

Two binaries, one module:

| binary | what it is |
| --- | --- |
| `musoakd` | the server: HTTP API, event stream, media cache, rooms |
| `musoak` | the CLI: search, queue, play, listen together |

The CLI is a plain API client, so a Discord bot (or anything else) is too. With
`client.serverURL` empty the CLI starts its own server in-process, which means a
single binary is a complete setup: no daemon, no separate configuration.

## Quick start

```sh
# Standalone: the CLI runs its own server, in-process.
nix run .#musoak -- search "Never Gonna Give You Up"
nix run .#musoak -- queue add 0
nix run .#musoak -- play

# Or run the server separately and point the client at it.
nix run .#musoakd -- --config config.json
nix run .#musoak -- --config config.json search "Something"
```

`config.example.json` is a complete, commented-by-example configuration. Unknown
keys are an error, so a typo can never be silently ignored.

## How it fits together

```
provider/          ytmusic, youtube (search + download), spotify (metadata only)
     |  search hits
     v
match/             ISRC and title/artist/album/duration scoring -> canonical tracks
     |  one track, many variants
     v
media/             download (singleflight, retries) -> ffmpeg -> sha256 -> media/<variant>.opus
     |              local imports, quota + LRU eviction, Range serving with ETag
     v
rooms/             per-member queues, mixed play order, host-sampled playback state
api/               REST + WebSocket, auth, ranking, rate limits
pkg/client         the public Go SDK: the CLI and any bot are clients of it
```

A **track** is the recording; a **variant** is one provider's (or one local
file's) rendition of it. Matching decides which variants belong to the same
track; the provider ranking decides which variant is played.

Listen Together details worth knowing before you rely on it:

* The host plays the room's mixed queue in its ordinary player. Its current
  queue item, position and pause state are reported on `/sync` when they change
  and once per second; the host player advances naturally to the next song.
* Everyone else follows one rule: a different song is loaded; the same song
  within two seconds of the room's position is left alone; a larger drift is
  corrected with a seek. Each member plays the version it can, so a shorter
  local file may end before the host's and stay silent until the room moves on.
* The server timestamps the host's position; clients estimate the server clock
  offset from WebSocket pings. The room does not advance on a server-side timer.
* Votes are 1 (bad) to 5 (great), one per member per track, changeable while the
  track plays. A skip fires when enough members have voted
  (`minVotersForSkip`), they are enough of the room (`voterFractionForSkip`) and
  the mean is below `skipThreshold`. Neutral (3) counts towards the mean.

## Playlists, radios, albums and artists

**Playlists** belong to an account: create one, append tracks, remove or reorder
by position, rename or delete it. `musoak playlist queue` feeds one into the
local queue and `musoak room queue playlist` into a listening room.

**Radios** are stations grown from a song. The caller picks the providers, and
the station interleaves them round-robin, so a two-provider radio really
alternates. Suggestions are matched into the canonical library on the way in, so
the seed never repeats and the same recording from two sources is one entry.

```sh
musoak radio 0 --providers ytmusic --length 25      # saved as a playlist
musoak radio 0 --providers ytmusic,spotify --play   # straight to the speakers
musoak radio 0 --room                               # into the room you are in
```

**Albums and artists** are canonical too. One album exists once, with each
provider's release hanging off it as a variant, and one artist exists once with
each provider's page as a variant. Searching matches across sources: the same
record found on two providers is one album with two variants.

**Syncing** goes the other way, from provider to library:

```sh
musoak album search "Whenever You Need Somebody"
musoak album sync 0 --providers ytmusic        # pull the tracklist, match every track
musoak album sync 0 --providers ytmusic,spotify --resolve
musoak artist search "Rick Astley"
musoak artist sync 0                           # their albums
musoak artist sync 0 --albums                  # and each album's tracklist
```

A sync fetches each selected release's tracklist, matches every track into the
canonical library (creating renditions as it goes) and attaches the result to
the album. That is what makes an album playable through *another* source's
renditions: sync the Spotify release and its tracks land on the same canonical
tracks as the YouTube Music ones. Syncing twice adds nothing twice. With
`resolve`, any track still lacking a playable rendition is matched against the
downloadable providers.

Two things worth knowing:

* Releases of one record (remasters, reissues, deluxe editions) share a title and
  primary artist, so they merge into one album with several variants. If you want
  editions kept apart, that is the place to change.
* An artist's album list is taken at face value: whatever the provider lists is
  what gets pulled, up to fifty albums per sync.

## Configuration

`--config FILE` wins, then `$MUSOAK_CONFIG`, then
`$XDG_CONFIG_HOME/musoak/config.json`. The same file configures the server and
the client; a missing file at the last location is not an error (defaults apply).

| key | default | meaning |
| --- | --- | --- |
| `listen` | `":4420"` | HTTP listen address (`musoak serve` uses it too). |
| `storageDir` | `$XDG_DATA_HOME/musoak` | Database and `media/` live here. |
| `requireLogin` | `false` | Reject anonymous requests outright. When false, anonymous callers are guests with read/playback access and no personal data. |
| `registrationOpen` | `true` | Whether `/auth/register` accepts new accounts. Passwords have no length rule beyond being non-empty: what makes guessing expensive is argon2id (64 MiB, 3 passes) plus `rateLimit.loginPerMinute`. |
| `providers.ytmusic.enabled` | `true` | YouTube Music: search and download. |
| `providers.youtube.enabled` | `true` | Plain YouTube: search and download through yt-dlp. Its hits are videos rather than recordings, so clients keep it out of their own filters until asked. |
| `providers.youtube.cookiesFromBrowser` / `cookiesFile` | `""` | YouTube sign-in cookies for yt-dlp, the same escape hatch YouTube Music takes; see `providers.ytmusic.*` above. |
| `providers.spotify.enabled` | `false` | Spotify: metadata only (audio is DRM-protected). |
| `providers.spotify.clientId` / `clientSecret` | `""` | Spotify application credentials. Keep the secret out of the Nix store; use `services.musoakd.configFile`. |
| `defaultProviderOrder` | `["ytmusic","spotify"]` | Fallback preference; every entry must be a known provider and cover the enabled ones. |
| `prefetchCount` | `3` | How many upcoming tracks a client keeps downloaded. |
| `match.threshold` | `0.8` | Score in (0,1] above which two renditions are the same recording. |
| `rateLimit.searchPerMinute` | `30` | Per-client search budget; 0 disables it. |
| `rateLimit.loginPerMinute` | `10` | Per-client register/login budget; 0 disables it. |
| `trustedProxies` | `[]` | CIDRs of reverse proxies whose `X-Forwarded-For` is believed, so rate limits key on the real client instead of the proxy. Leave empty when clients reach the server directly; set it only when something in front forwards to it (e.g. `["127.0.0.1/32", "10.0.0.0/8"]`). |
| `media.quotaMB` | `0` | Cap for `media/`; 0 means unlimited. Over it, least recently served renditions are evicted. |
| `media.prefetchPlaylists` | `true` | Keeps the songs in people's playlists downloaded before anybody plays them: one song at a time in the background, so a first play costs nothing. |
| `listenTogether.skipThreshold` | `2.0` | Mean vote below which the room skips. |
| `listenTogether.minVotersForSkip` | `2` | Voters needed before a skip can fire. |
| `listenTogether.voterFractionForSkip` | `0.5` | Fraction of the room that must have voted. |
| `client.serverURL` | `""` | Server to talk to. Empty runs the server inside the CLI. |
| `client.cacheDir` | `$XDG_CACHE_HOME/musoak` | Client-side media cache, queue and session token. |

Runtime dependencies (bundled by the Nix packages, otherwise on `PATH`):
`python3` with `ytmusicapi`, `yt-dlp`, `ffmpeg`/`ffprobe`, and `mpv` for the CLI.
The server logs a clear warning for anything missing at startup.

## CLI

```
musoak search <query>                  merged, cross-provider results
musoak queue add <track-id|index>      queue something (index = last search result)
musoak queue list|rm|clear
musoak play                            gapless playback, prefetching ahead
musoak library import <file|dir>       add local files; they match into the library
musoak fav add|list|rm <track-id|index>
musoak playlist create|list|show|add|rm|reorder|rename|delete|queue|play
musoak radio <track-id|index>          station from a song (--providers, --length, --play, --room)
musoak album search|show|sync|queue|play
musoak artist search|show|sync         (--albums pulls the discography's tracklists)
musoak providers [rank <a,b,c>]        provider capabilities and preference
musoak login <username> [--register]   password on stdin or --password
musoak me | logout
musoak room create|list|join <id>      rooms; join plays along with mpv
musoak room queue add|vote|skip|pause|resume|seek|now
musoak serve                           run the server in the foreground
```

### Player keys

`musoak play` keeps a status line while it plays, and takes single keys without
Enter:

| key | action |
| --- | --- |
| `k` / `↑` | volume up |
| `j` / `↓` | volume down |
| `l` / `→` | next track |
| `h` / `←` | previous track |
| `space` | pause / resume |
| `q` | quit |

Skipping to a track that is still downloading is remembered and applied as soon
as it is ready. Without a terminal on stdin, playback stays non-interactive and
prints one line per track.

Headless machines can play through a null sink:
`MUSOAK_MPV_ARGS="--ao=null --no-video" musoak play`.

## HTTP API

Everything the server does is an API call; `internal/api/openapi.yaml` is the
reference and is served at `/api/v1/openapi.yaml`. A test asserts that the
specification and the routes cannot drift apart.

* `GET /api/v1/search?q=` — merged results plus `providerErrors` for providers
  that failed (one failure never fails the search).
* `GET /api/v1/tracks/{id}`, `/variants`, `POST /tracks/{id}/resolve` — resolve
  attaches a downloadable rendition when a track only has metadata-only ones.
* `POST /api/v1/media/{variantId}/download` — start or join a download
  (idempotent); `GET /api/v1/media/{variantId}` streams the file with Range and
  a sha256 ETag, and never starts work.
* `POST /api/v1/library/import` — local files become first-class variants.
* `GET/POST /api/v1/me/playlists...` — playlists, including positional edits.
* `POST /api/v1/radio` — a station from a seed, mixed from the providers you name.
* `GET /api/v1/albums/search`, `GET /api/v1/albums/{id}`,
  `POST /api/v1/albums/{id}/sync` — canonical albums and their sync.
* `GET /api/v1/artists/search`, `GET /api/v1/artists/{id}`,
  `POST /api/v1/artists/{id}/sync` — canonical artists and their sync.
* `GET /api/v1/ws` — room events plus clock-sync pings.

## Nix

```nix
{
  inputs.musoak.url = "git+https://codeberg.org/kyleraykbs/musoak";

  # Server
  services.musoakd = {
    enable = true;
    listen = ":4420";
    storageDir = "/var/lib/musoakd";        # default; a StateDirectory
    openFirewall = true;
    providers.spotify.enable = true;
    # The generated config lands in the world-readable store, so secrets come
    # from a file instead:
    configFile = "/run/secrets/musoakd.json";
  };

  # CLI, on NixOS (system-wide defaults + MUSOAK_CONFIG) ...
  programs.musoak = {
    enable = true;
    serverURL = "http://127.0.0.1:4420";
    cacheDir = null;                            # $XDG_CACHE_HOME/musoak
    server.storageDir = null;                   # only used in standalone mode
  };

  # ... or in home-manager:
  # home-manager.users.kyle.programs.musoak = { enable = true; serverURL = "http://127.0.0.1:4420"; };
}
```

The flake exposes `packages.{musoak,musoakd}`, matching `apps`, and
`nixosModules.musoakd`, `nixosModules.musoak`, `homeManagerModules.musoak`. Both
binaries are wrapped with the tooling they need, so `nix run` works with no
environment setup. `nix build` also runs the whole Go test suite.

## Writing a provider

A provider is a plugin behind one interface; nothing outside
`internal/provider` knows what YT Music or Spotify are.

```go
type Provider interface {
    Name() string
    Capabilities() Caps   // {Search, Download}
    Search(ctx context.Context, q string, opts SearchOpts) ([]Track, error)
    // Download leaves an Ogg/Opus file at destPath (parent directory exists).
    // It is only called when Capabilities().Download is true.
    Download(ctx context.Context, providerTrackID, destPath string) error
}
```

Rules that keep the rest of the server honest:

1. **Return errors, never nil results** — the registry reports a provider's
   failure alongside the other providers' results.
2. **Fill in `ISRC` when you have it.** It is the only conclusive match signal.
3. `Track.DurationMs` must be the real length: the 5-second matching gate and
   the room timeline depend on it.
4. If downloading is impossible (DRM, subscription-only), set
   `Caps{Search: true, Download: false}` and return
   `provider.ErrDownloadUnsupported` from `Download`. Tracks like that are
   played through a matched rendition from a downloadable provider, and the
   server refuses to try downloading them directly.
5. Collections are optional: implement `AlbumSearcher` and `ArtistSearcher` to
   let an album or artist be browsed and synced, and `RadioProvider` to feed
   stations. Set the matching flags in `Caps` — a provider that cannot download
   can still browse (Spotify does), and a provider that cannot build a station
   is simply left out of a radio mix.
6. Shelling out? Implement `MissingDeps()` so startup reports your tooling.
7. Register it in `Server.registerProviders` and add its config section.

Test it with `httptest` against the provider's own API shape (see
`internal/provider/spotify/spotify_test.go`), plus a `//go:build live` test when
the real service is reachable.

## Writing a Sink (Discord bot)

A **Sink** is playback: the CLI's sink drives mpv, a bot's sink streams the
rendition into a voice channel. Everything else — what to play, when to start,
how to stay in sync — is `client.Participant`.

```go
type Sink interface {
    // Play starts this client's file and returns when it ends or ctx is canceled.
    Play(ctx context.Context, track client.SinkTrack) error
    Stop(ctx context.Context) error
}
// Optional capabilities the participant uses when available:
type Pauser   interface{ Pause(ctx) error; Resume(ctx) error }
type Seeker   interface{ Seek(ctx, positionMs int64) error }
type Positioner interface{ PositionMs(ctx) (int64, error) }
```

A bot looks like this:

```go
c := client.New(serverURL, client.WithToken(token))
room, err := c.JoinRoom(ctx, roomID)
cache, err := client.OpenCache("/var/lib/bot/cache")
participant := client.NewParticipant(room, cache, myVoiceSink, logger)
err = participant.Run(ctx) // host advances the mixed queue; followers sync to it
```

The participant downloads a locally playable rendition, syncs the host's
current queue item and position, and keeps followers within two seconds of the
room position. A host advances its mixed queue when its file ends; a follower
whose local file ends early waits for the next room item. Pause, seek, skip and
vote commands remain room operations.

Your sink only has to answer "play this file from this position" and "where are
you now". Discord voice needs its own library and a process outside this
repository; the boundary is
`pkg/client`.

## Testing it yourself

```sh
nix develop                       # go, mpv, ffmpeg, yt-dlp, python3+ytmusicapi, curl, jq
go test ./...                     # unit + integration (drives real mpv, real ffmpeg)
go test -tags live ./internal/provider/ytmusic/   # downloads a real track from YT Music

# Standalone: no daemon, the CLI runs its own server in-process.
nix run .#musoak -- search "Never Gonna Give You Up"
nix run .#musoak -- queue add 0
MUSOAK_MPV_ARGS="--ao=null --no-video" nix run .#musoak -- play   # headless playback

# Server + client, then the whole REST API with curl.
nix run .#musoakd -- --config /tmp/config.json &
scripts/api-smoke.sh http://127.0.0.1:4420        # 57 checks, exits non-zero on failure

# Listen together with two clients (two terminals, separate cache dirs).
musoak room create --name party
musoak room queue add 0
musoak room join <room-id>          # plays along; the second client joins mid-track
```

The Nix packages are wrapped with the tooling they need, so `nix run` works from
a bare shell. A binary built by hand needs `python3`+`ytmusicapi`, `yt-dlp`,
`ffmpeg` and (for the CLI) `mpv` on `PATH`; the server logs what is missing.

## Development

```sh
nix develop            # go, mpv, ffmpeg, yt-dlp, python3+ytmusicapi, gopls, ...
go test ./...
go test -tags live ./internal/provider/ytmusic/   # real search and download
nix build              # builds both binaries and runs the tests in the sandbox
```

`agents/` holds the running decisions, structure and terminology notes for this
repository.

## Known limits

* **Rendition alignment**: members may play different masters of the same track.
  Room sync compares track identity and playhead position; it does not offset
  different intros within versions of one canonical track.
* **yt-dlp** breaks periodically; it is a runtime dependency, wrapped by the Nix
  packages so updating it is a one-line change.
* **Spotify** needs application credentials and is metadata-only by design.
* **Discord** voice lives outside this repository, on top of `pkg/client`.
