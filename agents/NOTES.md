# Notes

## [2026-10-14] Load/stability audit (musoak + musoak-web)

Six parallel read-only passes over media/providers, rooms/ws/api, store/match/ranking,
client/CLI, web server/boot, web player/rooms runtime. Everything below was re-read and
re-verified by hand; the top item was reproduced with a throwaway test.

Proven, not inferred:
- `media.Manager.EnsurePrimary` deadlock — reproduced by test (a play joining a background
  singleflight call that is blocked in `primaryGate.wait`). It wedges the variant's
  singleflight key *and* permanently closes the gate, so every later `Ensure` blocks forever.
  Trigger: `Ensure` of V queued behind another play's gate, then `EnsurePrimary` of V.
- `CandidateTracks` is a full scan + temp B-tree sort (EXPLAIN QUERY PLAN) — 0.73 / 2.85 /
  13.9 ms per call at 5k / 20k / 100k tracks, once per new search hit, under `Matcher.mu`.
  Index + `BETWEEN` rewrite: 29.7 → 3.8 ms on a 200k-row table.
- Web `sw.js` forces `fetch(request, { cache: "reload" })` for every shell file, so the
  server's ETag/304 path (which works — verified with curl) is never taken from a browser.
- Server 304 responses carry `Content-Encoding: gzip` (verified with curl) because
  `gzipWriter.WriteHeader` sets it for any status.
- `assetStamp()` walks + SHA-256s the whole frontend per page request: ≈1.1 ms/page here
  (page 1.37 ms vs asset 0.29 ms, single-process client).
- Both test suites are green: `go test ./...` in musoak, `go test ./...` + 221 node tests in
  musoak-web.

Traps for the next session:
- `context.WithoutCancel` in `media.ensure` (media.go:184) makes the comment at
  media.go:206-207 false — the gate wait cannot be cancelled by its caller, and it
  neutralises the prefetcher's own 30 m pass timeout.
- `_retryWanted` in web `player.js` gates on `isPaused()`, which is true exactly when the
  browser blocked autoplay, so the 4 s retry it documents never runs.
- `writeLocal()` persists `config` and `roomsShowing`; `loadLocal()` rehydrates neither.
- `rooms-state.warmAhead` runs on the 500 ms follow tick with no in-flight dedup in
  `player._warm`.