# Structure

Go module `codeberg.org/kyleraykbs/prismusic`.

## Binaries

- `cmd/prismusicd` — the server.
- `cmd/prism` — the CLI. With `client.serverURL` empty it runs the server
  in-process (`internal/cli` starts `internal/api` on loopback); `prism serve`
  runs that same server in the foreground on `listen`.

## Server internals

- `internal/config` — strict JSON config (unknown keys are errors), defaults,
  `--config` > `$PRISMUSIC_CONFIG` > XDG path.
- `internal/store` — SQLite (modernc, pure Go). Migrations v1 schema, v2 votes,
  v3 media last-use. Narrow repository interfaces per aggregate.
- `internal/provider` — the plugin boundary: `Provider`, `Caps`, registry with
  concurrent fan-out, per-provider timeout and failure isolation.
  - `internal/provider/ytmusic` — search through the embedded `support/` python
    helper, download through yt-dlp + ffmpeg.
  - `internal/provider/spotify` — client-credentials search, metadata only.
- `internal/ffmpeg` — probe and the opus transcode/remux both providers and
  local imports use.
- `internal/match` — normalization, scoring (ISRC short-circuit, 5 s gate,
  renormalized signals), `Attach`/`Adopt`/`Group`/`Resolve`.
- `internal/media` — download pipeline (singleflight, retries, quota + LRU
  eviction), Range+ETag serving, local import/scan.
- `internal/auth` — argon2id PHC hashes, hashed bearer sessions, registration
  policy.
- `internal/ranking` — per-user provider order, cached mean-position aggregate,
  `Pick` for variant selection.
- `internal/rooms` — rooms, readiness gate, timeline, votes, event bus, clock.
- `internal/api` — REST + WebSocket surface, OpenAPI document (embedded and
  served), rate limits, auth middleware. `routeTable` is the single list of
  routes and is checked against the spec by a test.
- `internal/cli` — the prism command set, mpv IPC driver, gapless queue
  playback with prefetch.

## Public

- `pkg/client` — the SDK: REST, WebSocket, clock sync, `Participant`, `Sink`,
  local media `Cache`. The CLI and any Discord bot are clients of this.

## Support

- `support/` — python helpers embedded into the binary (ytmusicapi search).
- `nix/` — server option set shared by the modules, the prismusicd service
  module, and the NixOS/home-manager CLI modules.
- `agents/` — decisions, structure, terminology, notes.
