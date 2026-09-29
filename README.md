# Prismusic

A self-hosted music server, client and listening room. It searches several
providers, merges their results into one canonical library, downloads the audio
it needs as Ogg/Opus, and lets a room of people listen to the same thing at the
same time.

Two binaries, one module:

| binary | what it is |
| --- | --- |
| `prismusicd` | the server: HTTP API, event stream, media cache, rooms |
| `prism` | the CLI: search, queue, play, listen together |

The CLI is a plain API client, so a Discord bot (or anything else) is too. With
`client.serverURL` empty the CLI starts its own server in-process, which means a
single binary is a complete setup: no daemon, no separate configuration.

## Quick start

```sh
# Standalone: the CLI runs its own server, in-process.
nix run .#prism -- search "Never Gonna Give You Up"
nix run .#prism -- queue add 0
nix run .#prism -- play

# Or run the server separately and point the client at it.
nix run .#prismusicd -- --config config.json
nix run .#prism -- --config config.json search "Something"
```

`config.example.json` is a complete, commented-by-example configuration. Unknown
keys are an error, so a typo can never be silently ignored.

## How it fits together

```
provider/          ytmusic (search + download), spotify (metadata only)
     |  search hits
     v
match/             ISRC and title/artist/album/duration scoring -> canonical tracks
     |  one track, many variants
     v
media/             download (singleflight, retries) -> ffmpeg -> sha256 -> media/<variant>.opus
     |              local imports, quota + LRU eviction, Range serving with ETag
     v
rooms/             the room owns the timeline; clients follow startedAt
api/               REST + WebSocket, auth, ranking, rate limits
pkg/client         the public Go SDK: the CLI and any bot are clients of it
```

A **track** is the recording; a **variant** is one provider's (or one local
file's) rendition of it. Matching decides which variants belong to the same
track; the provider ranking decides which variant is played.

Listen Together details worth knowing before you rely on it:

* The server is the source of truth. A track starts once every member reports
  ready, or when `readyTimeoutSeconds` passes; members who were not ready are
  marked *catching up* and may still join mid-track.
* The room's timeline is the longest rendition in the room. A shorter file is
  padded with silence, a longer one is cut at the timeline, and nobody ends the
  track early.
* Clients ping the server to estimate their clock offset (NTP-style, lowest
  round trip wins) and correct drift with small seeks.
* Votes are 1 (bad) to 5 (great), one per member per track, changeable while the
  track plays. A skip fires when enough members have voted
  (`minVotersForSkip`), they are enough of the room (`voterFractionForSkip`) and
  the mean is below `skipThreshold`. Neutral (3) counts towards the mean.

## Configuration

`--config FILE` wins, then `$PRISMUSIC_CONFIG`, then
`$XDG_CONFIG_HOME/prismusic/config.json`. The same file configures the server and
the client; a missing file at the last location is not an error (defaults apply).

| key | default | meaning |
| --- | --- | --- |
| `listen` | `":8080"` | HTTP listen address (`prism serve` uses it too). |
| `storageDir` | `$XDG_DATA_HOME/prismusic` | Database and `media/` live here. |
| `requireLogin` | `false` | Reject anonymous requests outright. When false, anonymous callers are guests with read/playback access and no personal data. |
| `registrationOpen` | `true` | Whether `/auth/register` accepts new accounts. |
| `providers.ytmusic.enabled` | `true` | YouTube Music: search and download. |
| `providers.spotify.enabled` | `false` | Spotify: metadata only (audio is DRM-protected). |
| `providers.spotify.clientId` / `clientSecret` | `""` | Spotify application credentials. Keep the secret out of the Nix store; use `services.prismusicd.configFile`. |
| `defaultProviderOrder` | `["ytmusic","spotify"]` | Fallback preference; every entry must be a known provider and cover the enabled ones. |
| `prefetchCount` | `3` | How many upcoming tracks a client keeps downloaded. |
| `match.threshold` | `0.8` | Score in (0,1] above which two renditions are the same recording. |
| `rateLimit.searchPerMinute` | `30` | Per-client search budget; 0 disables it. |
| `rateLimit.loginPerMinute` | `10` | Per-client register/login budget; 0 disables it. |
| `media.quotaMB` | `0` | Cap for `media/`; 0 means unlimited. Over it, least recently served renditions are evicted. |
| `listenTogether.skipThreshold` | `2.0` | Mean vote below which the room skips. |
| `listenTogether.minVotersForSkip` | `2` | Voters needed before a skip can fire. |
| `listenTogether.voterFractionForSkip` | `0.5` | Fraction of the room that must have voted. |
| `listenTogether.readyTimeoutSeconds` | `30` | How long to wait for readiness; `0` waits for everybody. |
| `client.serverURL` | `""` | Server to talk to. Empty runs the server inside the CLI. |
| `client.cacheDir` | `$XDG_CACHE_HOME/prismusic` | Client-side media cache, queue and session token. |

Runtime dependencies (bundled by the Nix packages, otherwise on `PATH`):
`python3` with `ytmusicapi`, `yt-dlp`, `ffmpeg`/`ffprobe`, and `mpv` for the CLI.
The server logs a clear warning for anything missing at startup.

## CLI

```
prism search <query>                  merged, cross-provider results
prism queue add <track-id|index>      queue something (index = last search result)
prism queue list|rm|clear
prism play                            gapless playback, prefetching ahead
prism library import <file|dir>       add local files; they match into the library
prism fav add|list|rm <track-id|index>
prism providers [rank <a,b,c>]        provider capabilities and preference
prism login <username> [--register]   password on stdin or --password
prism me | logout
prism room create|list|join <id>      rooms; join plays along with mpv
prism room queue add|vote|skip|pause|resume|seek|now
prism serve                           run the server in the foreground
```

Headless machines can play through a null sink:
`PRISM_MPV_ARGS="--ao=null --no-video" prism play`.

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
* `GET /api/v1/ws` — room events plus clock-sync pings.

## Nix

```nix
{
  inputs.prismusic.url = "git+https://codeberg.org/kyleraykbs/prismusic";

  # Server
  services.prismusicd = {
    enable = true;
    listen = ":8080";
    storageDir = "/var/lib/prismusicd";        # default; a StateDirectory
    openFirewall = true;
    providers.spotify.enable = true;
    # The generated config lands in the world-readable store, so secrets come
    # from a file instead:
    configFile = "/run/secrets/prismusicd.json";
  };

  # CLI, on NixOS (system-wide defaults + PRISMUSIC_CONFIG) ...
  programs.prism = {
    enable = true;
    serverURL = "http://127.0.0.1:8080";
    cacheDir = null;                            # $XDG_CACHE_HOME/prismusic
    server.storageDir = null;                   # only used in standalone mode
  };

  # ... or in home-manager:
  # home-manager.users.kyle.programs.prism = { enable = true; serverURL = "http://127.0.0.1:8080"; };
}
```

The flake exposes `packages.{prism,prismusicd}`, matching `apps`, and
`nixosModules.prismusicd`, `nixosModules.prism`, `homeManagerModules.prism`. Both
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
5. Shelling out? Implement `MissingDeps()` so startup reports your tooling.
6. Register it in `Server.registerProviders` and add its config section.

Test it with `httptest` against the provider's own API shape (see
`internal/provider/spotify/spotify_test.go`), plus a `//go:build live` test when
the real service is reachable.

## Writing a Sink (Discord bot)

A **Sink** is playback: the CLI's sink drives mpv, a bot's sink streams the
rendition into a voice channel. Everything else — what to play, when to start,
how to stay in sync — is `client.Participant`.

```go
type Sink interface {
    // Play starts track and returns when this client's own file has played
    // out (not when the room's slot ends). Play ctx cancellation = stop.
    Play(ctx context.Context, track client.SinkTrack) error
    Stop(ctx context.Context) error
}
// Optional capabilities the participant uses when available:
type Pauser  interface{ Pause(ctx) error; Resume(ctx) error }
type Seeker  interface{ Seek(ctx, positionMs int64) error }
type Positioner interface{ PositionMs(ctx) (int64, error) } // enables drift correction
```

A bot looks like this:

```go
c := client.New(serverURL, client.WithToken(token))
room, err := c.JoinRoom(ctx, roomID)
cache, err := client.OpenCache("/var/lib/bot/cache")
participant := client.NewParticipant(room, cache, myVoiceSink, logger)
err = participant.Run(ctx)     // downloads, reports ready, follows the timeline
```

What the participant does for you, so a sink does not have to: it syncs the
clock, downloads the rendition the server assigned (or reuses a local copy it
already has, reporting that variant's real duration), reports readiness, starts
playback at the room's `startedAt`, pads silence when its own file is shorter
than the timeline, corrects drift with small seeks, and forwards pause/resume/
seek/skip.

Your sink only has to answer "play this file from this position" and "where are
you now". `SinkTrack.OffsetMs` is reserved for per-variant alignment (different
masters have different intros) and is always zero today. Discord voice needs its
own library and a process outside this repository; the boundary is
`pkg/client`.

## Testing it yourself

```sh
nix develop                       # go, mpv, ffmpeg, yt-dlp, python3+ytmusicapi, curl, jq
go test ./...                     # unit + integration (drives real mpv, real ffmpeg)
go test -tags live ./internal/provider/ytmusic/   # downloads a real track from YT Music

# Standalone: no daemon, the CLI runs its own server in-process.
nix run .#prism -- search "Never Gonna Give You Up"
nix run .#prism -- queue add 0
PRISM_MPV_ARGS="--ao=null --no-video" nix run .#prism -- play   # headless playback

# Server + client, then the whole REST API with curl.
nix run .#prismusicd -- --config /tmp/config.json &
scripts/api-smoke.sh http://127.0.0.1:8080        # 57 checks, exits non-zero on failure

# Listen together with two clients (two terminals, separate cache dirs).
prism room create --name party
prism room queue add 0
prism room join <room-id>          # plays along; the second client joins mid-track
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

* **Timeline alignment**: platform renditions can differ by an intro even when
  their lengths match; only length differences are handled today (see
  `SinkTrack.OffsetMs`).
* **yt-dlp** breaks periodically; it is a runtime dependency, wrapped by the Nix
  packages so updating it is a one-line change.
* **Spotify** needs application credentials and is metadata-only by design.
* **Discord** voice lives outside this repository, on top of `pkg/client`.
