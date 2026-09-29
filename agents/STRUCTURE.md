# Structure

Top-level layout (Go module `codeberg.org/kyleraykbs/prismusic`):

- `cmd/prismusicd` — server binary
- `cmd/prism` — CLI binary
- `internal/` — server internals (config, store, provider, match, media, auth, rooms, api)
- `pkg/client` — public Go SDK (CLI + future Discord bot)
- `support/` — embedded python helpers (ytmusicapi)
