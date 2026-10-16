# TODO

Build order per plan: 1 → 2 → 3 → 6 → 5 → 8 → 7 → 9 → 10 → 11 → 4 → 12,
then the flake's packages and NixOS/home-manager modules, then verification.

- [x] Block 1: skeleton + config
- [x] Block 2: data model + store
- [x] Block 3: provider interface + ytmusic
- [x] Block 6: media manager
- [x] Block 5: aggregation + matching
- [x] Block 8: HTTP API + event stream
- [x] Block 7: accounts, favorites, provider ranking
- [x] Block 9: listen together
- [x] Block 10: public Go SDK
- [x] Block 11: CLI
- [x] Block 4: Spotify provider (live check needs real credentials)
- [x] Block 12: hardening + docs
- [x] Nix flake: packages, apps, service module, CLI modules (NixOS + HM)
- [x] Verification: go test, nix build/flake check, curl sweep (57/57)

Open, both needing input from Kyle:

- Spotify has only been exercised against a fake Web API; point a real
  clientId/clientSecret at it and re-run `go test ./internal/provider/...`.
- The NixOS/HM modules evaluate and build, but nothing wires them into the
  system flake yet; that is a `clan machines update <name>` away, by his call.

## The reliability audit's backend findings (2026-10-15)

- [x] `internal/api/searchanswers.go`: a waiter's own context bounds its wait,
      so a client that has gone stops waiting for somebody else's answer.
- [x] `internal/provider/provider.go`: `Album`, `ArtistAlbums` and `Playlist`
      carry a per-provider deadline, as the fan-outs already did.
- [x] `internal/api/playlists.go`: the playlist detail is one batched read for
      the whole list (`TracksFor` + `buildTracks`), not three queries a song.
- [ ] Declined, on purpose: batching the album and artist search rows'
      detail reads. It needs `AlbumVariantsFor` and a track-count read added to
      the narrow store interface, and what it saves is local SQLite point reads
      measured against a provider fan-out in the same handler - seconds against
      milliseconds. The measurement, not the principle, is why it stays.

## What the live server showed (2026-10-15, night)

From `kyle.properties/musoak` (nginx -> musoak-web -> musoakd, over a WAN): the
round trip is 27 ms and every API call is 0.05-0.2 s, so the slowness a listener
reports is server-side work on the critical path, not latency.

- The prefetch pass walks every playlist song every ten minutes and ran the
  whole download retry ladder for the ones already known to have failed, every
  pass. Fixed: the pass now skips a rendition the store reports as failed (a
  person pressing play still retries), and paces a failure the way it paces a
  fetch.
- `track still has no source after resolving ... hint="the rendition is attached
  to another track"` was logged for a search that found *nothing above the
  threshold* as well as for a real duplicate. Fixed: the two say which they are,
  with the title, so the next reader is not sent looking for duplicates that are
  not there.
- Still open, and felt as "a room that never starts": a song whose source cannot
  be found sits in the room's status line while the client asks again on a short
  beat. The client's backoff grows now (5 s doubling to 5 min), which stops the
  hammering, but a room whose *current* song cannot be sourced is still a room
  with nothing to play - it should be told, and the room should be able to move
  on.
- Still open, provider-side: age-restricted tracks fail the ladder every pass
  (yt-dlp needs cookies). The pass no longer repeats them, but the first attempt
  still costs ~15 s.
