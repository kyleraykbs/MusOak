# Decisions

## [2026-09-30] Module path
- Decision: `codeberg.org/kyleraykbs/musoak`, renamed from the v1 path along with the rest of the project.
- Why: the project is called MusOak now, and the SDK import path follows it.
- Reversible: no (touches every import)

## [2026-09-30] Naming: MusOak everywhere
- Decision: name everything MusOak — binaries `musoak` and `musoakd`, the `musoak` config/data/cache dirs, the `musoak` CLI verb — superseding the earlier choice to keep the v1 naming even though the repo lives in `projects/musoak`.
- Why: the project is called MusOak; the web UI repo was renamed the same day, and nothing depends on the old directory name.
- Reversible: no (users type the CLI and daemon names, and the defaults moved)

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
- Decision: `--config` > `$MUSOAK_CONFIG` > `$XDG_CONFIG_HOME/musoak/config.json`. The NixOS CLI module drops `/etc/musoak/config.json` and sets the env var; the home-manager module writes the XDG file, which wins for that user.
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

## [2026-09-28] Collections: canonical albums and artists with provider variants
- Decision: an album's identity is (normalised title, normalised primary artist); an artist's is the normalised name. Every provider release hangs off the canonical row as a variant (`album_variants`, `artist_variants`), and an album's tracklist lives in `album_tracks`.
- Why: the plan wants albums and artists as first-class entities and "sync between sources"; this mirrors how tracks already work, so one album can be played through another source's renditions.
- Reversible: no (the `albums` table was rebuilt with the new identity in migration 5)

## [2026-09-28] Release editions merge; provider listings are taken at face value
- Decision: remasters/reissues/deluxe editions of one record share title and primary artist, so they merge into one album with several variants. An artist sync pulls whatever the provider lists, capped at fifty albums.
- Why: distinguishing editions needs per-edition metadata providers do not agree on; merging matches the plan's "one record, many renditions" model.
- Reversible: yes

## [2026-09-28] A radio is a playlist
- Decision: a generated station is saved as a playlist named after the seed (and the providers), unless the caller passes `save: false`. Guests may generate but not save, since playlists belong to an account.
- Why: makes a station durable, queueable into a room, and testable with the pieces that already exist.
- Reversible: yes

## [2026-10-01] A long import is a job; a playlist download streams under one
- Decision: `POST /me/playlists/import` starts the work and answers 202 with a job id; `GET /me/playlist-imports/{jobId}` reports `done` of `total` while it runs, and the new playlist when it ends. Downloading a playlist is the same shape: `POST /me/playlists/{playlistId}/archives` reads the playlist and answers with an archive to watch, `GET /me/playlist-archives/{archiveId}` reports how many songs have been written so far, and `GET /me/playlist-archives/{archiveId}/file` streams the zip, fetching anything the media store does not have yet on the way. One archive is downloaded once; a second reader is told to ask for a new one.
- Why: an import is a provider search per track, which outlives the client's patience - the web client aborts its fetch after thirty seconds, which is what killed a fifty-seven second import. A job is also the only honest place to report progress from, since the count of tracks matched is the whole story. The archive keeps streaming instead of being built into a file first, because a download is the one request a browser waits for however long it takes, and streaming means a cold playlist arrives as it is fetched rather than after; but a response cannot report progress and a playlist nothing has played is minutes of work, so a job counts the songs as they are written. Writing the zip is the work, which is why a second reader has to start over rather than share it.
- Also: the playlist search now returns `providerPlaylistId`. It was the one thing picking a search result needed, and its absence made the picker's import send this library's own id to the provider.
- Reversible: yes

## [2026-10-01] The server keeps the playlists downloaded; a member keeps the next three
- Decision: `media.prefetchPlaylists` (on by default) has the server walk every playlist in the background: one song at a time, a breath after each one that had to be fetched, resolving a playable rendition and fetching it. An import or an added song nudges the next pass, and a pass runs on its own every ten minutes. In a room, a participant fetches the room's next three songs alongside the one the server prepares for it.
- Why: a playlist is a promise about what will be played, and the first play of a song that is not on disk is a provider fetch - seconds of nothing. A client warming the tracks it is about to reach is the same idea at a smaller scale, and the room is where it showed: the server prepares one track at a time, so a member had exactly one song ready, and a skip waited on a download. The prefetch is deliberately paced and best-effort, and the media store's singleflight means a play that wants the same song shares the download rather than racing it.
- Reversible: yes

## [2026-10-02] Matching survives how each provider spells a title and its credits
- Decision: `NormalizeTitle` drops a trailing romanisation from a title that has no Latin letters of its own ("トーキョーレギー - Tokyo Reggie" is the same song) and strips a remaster however the year is written ("- Remastered 2011", "- 2018 Remaster"); `artistSimilarity` falls back to comparing the words of the credits when the credit sets share nothing, which covers both a joined line split into two names and a feature credited on one side only. A title written in Latin letters keeps its version suffix, so "Song - Live at Wembley" stays a different recording.
- Why: a Spotify playlist imported through the public embed page left about a tenth of its tracks unplayable, which showed up as "source not found". The spellings were all ours to fix, not Spotify's: YouTube Music appends a romanisation to non-Latin titles and drops featured artists from its credits, while the embed page gives one joined credit line per track, so a name with a comma in it arrives as two people. The title signal is a word overlap and a non-Latin title is a single word, so a romanisation took two thirds of it.
- Also: when the matched rendition already belongs to another canonical track, `Resolve` merges the two. That case is a library holding one recording twice - the rendition on one row, the playlist entry on the other - and joining them is what makes the entry playable. `MergeTracks` moves variants, credits, ordered entries and favorites onto the kept row, drops what would exist twice, and deletes the row left empty.
- Reversible: yes (the merge is not)

## [2026-10-02] YouTube is a platform; clients decide what to ask for
- Decision: a plain YouTube provider (`providers.youtube`) joins ytmusic and spotify, enabled by default, searching and downloading through yt-dlp. Every search endpoint takes an optional `providers` query parameter naming the platforms to ask; absent means every enabled one, and a name that is not enabled is a 400. The web's search page and the source dialog carry a platform filter that leaves YouTube out until somebody ticks it.
- Why: YouTube is where a song that no music service carries is still findable, which is what the source dialog needs - but its hits are videos, not recordings, so asking it by default would bury everything else. Splitting the two decisions keeps the platform available without making it noise: the server enables it, each client decides whether to ask.
- Also: a pick made in the source dialog attaches to the song it was made for, and when that rendition already belongs to another row for the same recording the two rows are joined, the same way resolution does it.
- Reversible: yes (the merge is not)

## [2026-10-02] Which platforms a search asks is the account's choice
- Decision: an account carries `searchPlatforms` (on `/me`, written through `PATCH /me`): the platforms a search asks by default. Unset (an empty list) means the client's own default, which is every searchable platform except YouTube. The web's search filter and the source dialog both start from it every time they open, and the account page sets it with a checkbox per platform. Separately, the effective provider order now ends with every enabled provider the order did not already name.
- Why: a filter remembered in one browser is not a setting, and the only sane defaults differ per person - one wants YouTube as a fallback, another does not want it at all. The account is where that belongs, and the search page re-reading it is what keeps two browsers on the same account agreeing. The order completion fixes a smaller version of the same problem: a server whose configured order predates a provider handed back an order without it, so the account page could not show or rank it and variant picking never considered it.
- Also: Spotify is not offered in either place. It reports `search: false`, so a search asking it could only ever add an error line beside the results.
- Reversible: yes

## [2026-10-02] An account takes over the membership its browser made
- Decision: `rooms.Manager.Join` takes a `supersedes` member id. When an authenticated caller joins with a member id its browser was using as a guest, the account's membership takes over that guest's queue and the guest's membership is dropped (with a `member_left`). The web re-joins on a login (`rejoinRoom`), and the API names the browser's member id when an authenticated request still carries one.
- Why: a membership is made at the join and carries the identity it was made with, and an authenticated caller is its account - a different member id from the browser's own. Re-joining therefore added a second member beside the first: the room showed the same person twice, once as "web" with no picture, and whatever they had queued belonged to the ghost. Leaving and rejoining instead would have thrown that queue away, which is why the join adopts it.
- Also: the room page asks for a fresh snapshot when it opens, rather than painting whatever it holds until the next event says otherwise.
- Reversible: yes

## [2026-10-02] A friend request is a notification, and a profile change reaches the room
- Decision: sending a friend request notifies the other side (`friend-request`), accepting one notifies the sender (`friend-accepted`). A notification that cannot be recorded is logged and the action still stands. The web's notifications describe both kinds, and a request opens the Requests tab rather than the person's page. The bell's count is red, and the Requests tab carries the same count as a badge. Saving a display name or an icon re-joins the room you are in.
- Why: a request nobody notices is a request nobody answers, and the bell was already able to count them - there was simply nothing to count but shares. A membership carries the name and picture it was made with, which is why the room kept the old ones after a profile change; joining again is what makes it show the new ones, the same way signing in does.
- Also: animated png and gif icons already worked end to end (the bytes are stored and served as they arrive, so the frames and the loop extension survive). The upload's hint now says so, because nothing else did.
- Reversible: yes

## [2026-10-02] Two positions in the provider order are not providers
- Decision: a provider ranking may name `self` and `uploaded` alongside real providers. `self` is the caller's own upload of a track; `uploaded` is the highest-voted upload by somebody else. The default order starts with them. Where the order decides which rendition plays, a slot resolves to that rendition and is skipped when there is none; the variant payload carries `slot` so a client ranks by `slot || provider`.
- Why: an upload is often the best copy of a song - it is the one somebody bothered to put there - and the order is already the thing that decides which copy plays. Making them positions rather than a separate preference keeps one list and one rule.
- Also: clients rank by name, not by the order the server returns, so the slot has to travel in the payload. My first contract said "make the endpoint's order authoritative and clients need no change", which was wrong: the web's and the CLI's `pickVariant` both build a rank map from provider names, and a slot would have sorted last.
- Reversible: yes

## [2026-10-02] One association per user per song, and the queuer's file is the room's length
- Decision: associating an upload with a song releases the same user's earlier association there - the earlier upload goes back to standing on its own, on a track of its own. A new endpoint reports which upload would be replaced so a dialog can say so first. Separately, a room's timeline is the duration of the file played by the member who queued the track, and it moves when that member switches sources.
- Why: a user uploading a second copy of a song they already associated had two sources of their own on it, and "which of mine is this song" has one answer. The room's length was the longest rendition in the room, which meant queueing a 4:14 file next to somebody's 7:11 rendition left the queuer in silence for three minutes after their file ended. The person who queued the song chose a version of it; that version is how long the song lasts. Members with longer files are cut at the timeline end, as before.
- Also: the fallback is unchanged - without the queuer's report the longest rendition still decides, then the canonical track's duration. A queuer switching to a file shorter than the position already reached ends the track, which is the same rule seen from the other side.
- Reversible: yes

## [2026-10-02] A track starts after the renditions are assigned, not before
- Decision: `maybeStartLocked` waits for the prepare to finish as well as for the members to report ready, and the readiness timeout only marks the laggards skipped - whichever of the two finishes last starts the track. A client's report with no length is a placeholder: it is sent once to unblock the room, and the measured report that follows is a second, different report, so the room's length moves to it.
- Why: the room's length is the queuer's file, and that file is known from the rendition the room handed them. Assignment runs in a goroutine, so a member's readiness could start the track before the assignments existed, leaving the timeline to fall back to the longest file in the room - the bug that kept a queued 4:14 song running to 7:11. Reports with no length are unavoidable (a file's length lags the download by a moment) and were previously final, because the client only reported once per track.
- Also: the snapshot now carries `prepared`, so a client can tell "waiting on the room" from "waiting on me". The fixtures' enqueue helper waits for it, since a test that reports readiness synchronously would otherwise race the assignment.
- Reversible: yes

## [2026-10-03] The shortest file in the room ends the song, and a member can sit one out
- Decision: a room's timeline is the shortest file any member holds, not the queuer's and not the longest. A member who has not measured their file yet still counts, by the length of the rendition the room handed them. A member may also sit a track out: their file stops being what the song is measured by, the room stops waiting for them, and they stay in the room.
- Why: the queuer's-file rule still left the members holding shorter copies in silence at the end of their own song - the exact complaint, twice. Ending when the first file ends means nobody ever sits in silence, and a longer copy is cut at that point, which the room already did. The escape hatch exists because one member's short or broken file would otherwise end the song for everybody.
- Also: `retimeLocked` runs on every ready report, on a leave and on a late assignment, so the end of the track follows whoever is in the room now; the timer that advances the room moves with it. `ready_state` carries `timelineMs` and `out`, so clients follow without refetching.
- Reversible: yes
