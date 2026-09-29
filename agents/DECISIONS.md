# Decisions

## [2026-09-28] Module path
- Decision: `codeberg.org/kyleraykbs/prismusic` (same as v1)
- Why: v1 go.mod uses it; keeps the public SDK import path stable across versions.
- Reversible: no (touches every import)

## [2026-09-28] Naming: Prismusic inside `musoak/` dir
- Decision: keep Prismusic naming (binaries `prism`, `prismusicd`, config dir `prismusic`) even though the repo lives in `projects/musoak`.
- Why: plan is titled Prismusic v2; nothing references the directory name.
- Reversible: yes

## [2026-09-28] isrc column on variants
- Decision: add `isrc` to `variants` even though the plan's table list omits it.
- Why: ISRC is match signal #1 (Block 5) and Block 4 populates it; it must live on the provider-side row.
- Reversible: yes

## [2026-09-28] Providers deliver opus; media probes and stores
- Decision: `Provider.Download(id, destPath)` leaves an Ogg/Opus file at destPath (Block 3 says so explicitly); the media manager only probes, hashes and stores it, and does the transcode for local imports.
- Why: keeps one transcode implementation (`internal/ffmpeg`), which both the ytmusic provider and local import share.
- Reversible: yes

## [2026-09-28] Embedded support scripts are piped to `python3 -`
- Decision: `support/ytmusic/search.py` is fed to `python3 -` on stdin instead of being extracted to a temp dir (v1 extracted).
- Why: no temp files, no cleanup, same script.
- Reversible: yes

## [2026-09-28] Match score renormalizes over known signals
- Decision: signals the two sides do not both carry (missing artist, missing album, unknown duration) are dropped and the remaining weights renormalized.
- Why: otherwise an untagged local file (title + duration only) could never reach the 0.8 threshold, so imports would never merge.
- Reversible: yes

## [2026-09-28] Imports are adopted into existing tracks
- Decision: `Matcher.Adopt` re-points a freshly imported rendition at the best existing canonical track and deletes the throwaway track the import created (`DeleteTrack` refuses when variants still point at it).
- Why: Block 6 requires local variants to be "matched to canonical tracks by Block 5"; without adoption every import became its own track.
- Reversible: yes

## [2026-09-28] Rooms are in-memory and close when empty
- Decision: room state (queue, current track, readiness, votes-in-flight) lives in memory; votes are persisted to `votes` for stats. A room with no members left is deleted and announces `room_closed`.
- Why: the plan persists rankings/favourites/votes/match lookups relationally and says nothing about surviving restarts; a room with nobody in it is not worth keeping.
- Reversible: yes

## [2026-09-28] Room identity, controls and events
- Decision: authenticated members are their user id; guests get a server-generated member id returned by create/join and sent back in `X-Member-Id`. Commands are REST, events stream over `/api/v1/ws`. `Controls` is a per-room policy (`host` or `everyone`) applied to pause/resume/skip/seek/remove/reorder; anyone may enqueue. Added a `track_prepared` event beyond the plan's list so members learn their assigned rendition.
- Why: commands over REST stay curl-testable and keep the CLI and a bot on the same footing; the event stream is one-way, server to clients.
- Reversible: yes

## [2026-09-28] Ranking universe and cache
- Decision: the effective order for playback is the user's own ranking, else the aggregate; the aggregate's provider universe is the config default order plus every provider anyone ranked; users with no ranking contribute nothing; providers a user did not rank count as last for them; ties break by the configured default order then by name. The aggregate is cached and invalidated whenever a ranking changes.
- Why: matches the plan's mean-position rule while keeping the result stable and explainable.
- Reversible: yes

## [2026-09-28] Zero readiness timeout means "wait for everyone"
- Decision: `listenTogether.readyTimeoutSeconds = 0` disables the timeout entirely (the track starts once every member is ready).
- Why: least surprising for a listening party; a broken member stalls only if the host explicitly asked for no timeout.
- Reversible: yes

## [2026-09-28] One config file, two locations
- Decision: `--config` > `$PRISMUSIC_CONFIG` > `$XDG_CONFIG_HOME/prismusic/config.json`. The NixOS CLI module drops `/etc/prismusic/config.json` and sets the env var; the home-manager module writes the XDG file, which wins for that user.
- Why: NixOS cannot write into user homes, and a client should follow the user's own configuration when it exists.
- Reversible: yes

## [2026-09-28] Rate limits and media quota live in config
- Decision: `rateLimit.{searchPerMinute,loginPerMinute}` (per client, IP-keyed, 0 disables) and `media.quotaMB` (0 = unlimited, LRU eviction by last serve).
- Why: the plan asks for both; making them configurable keeps a LAN-only setup from paying for them.
- Reversible: yes

## [2026-09-28] The middleware stack is composed once
- Decision: `Server.Serve(ctx, listener)` is the only place that serves, and it always uses `handler()` (rate limits → auth → routes). `Run` and the CLI's embedded server both go through it.
- Why: the very first curl sweep caught the daemon serving the bare router, so authentication and rate limits were skipped over a real socket while the tests (using `Handler`) passed. A socket-level regression test now covers it.
- Reversible: no (this is a correctness invariant)

## [2026-09-28] Leaving the last member out of a room is a success
- Decision: `rooms.Manager.Leave` closes a room whose last member left and returns no error; the API answers 204.
- Why: the leave itself succeeded; reporting 404 made the curl sweep (and any client) treat a normal action as a failure.
- Reversible: yes
