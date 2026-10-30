# Decisions

## [2026-10-15] Audio is served only from files this server holds
- Decision: the provider-backed stream is gone — `Provider.Stream`, `Caps.Stream`, `GET /api/v1/tracks/{trackId}/stream`, the streaming branch of the media handler (`X-Musoak-Stream`), the `stream` capability in `/api/v1/providers`, and the client's stream surface. A request for a rendition that is not on disk is refused (`409`, naming the download to start) rather than answered with the provider's own fetch; a rendition on disk is served as before, with Range and `?start=`.
- Why: a stream from a provider is audio of unknown length that cannot be seeked, it competes with the download for the same upstream, and it makes a listener's play depend on the provider being reachable that second; a download that lands is durable and seekable, which is what the room's join-at-position needs.
- Reversible: no (deliberate)

## [2026-10-15] An imported file keeps the cover it carries
- Decision: `media.Import` reads the picture the tags hold (`Metadata.Picture()`) beside the title, artist and album it already read, and files it through a one-method seam the media package owns - `ArtworkStorer.StoreImportedArtwork(trackID uuid.UUID, contentType string, data []byte) (source string, err error)` - implemented in `internal/api` (`importArtwork`: the artwork fetcher, plus the naming) and wired into `media.New`. The picture lands under `local:<trackID>`, a pseudo-URL like an upload's `upload:<uploadID>`, and that source is what `SetTrackArtwork` records - the value `/api/v1/artwork/track/<id>` resolves to. `media.Store` gains `store.ArtworkRepo` for the single write.
- Why: `readTags` read title, artist and album and nothing else, so `track.artwork_url` stayed empty for everything imported from disk - a browser test measured "recorded artworkUrl for 0 of 3 imported songs" - while the upload path had filed covers all along. The tag library already carries the picture for MP3/FLAC/M4A/OGG, so the import was dropping what it had paid to read.
- Also: `internal/media` does not import `internal/artwork`. The storer answers the value to record rather than the media package building a path it cannot know: the cache, the source naming and the path clients fetch stay in the api package, and which suffix a picture is stored under is the cache's own `ExtensionFor` of the type the tag carried. A picture past `maxImportedArtworkBytes` (8 MB, the cap the upload endpoint applies) or one the cache refuses is logged at debug and skipped: no cover ever fails an import.
- Reversible: yes

## [2026-10-15] A held search answer is re-stamped on read, not shortened
- Decision: `searchAnswers.doLive(key, fn, restamp)` holds the tracks search's answer for the same 45 s (`SearchAnswerTTL`), but hands `restamp` the held value on every read; `Server.freshSearchMedia` answers with a copy of it where each rendition's `media.state` has been asked for again (one `MediaFilesFor` query for the listing plus the media manager's own memory lookups, `variantMediaStatus`). `do` is `doLive` with no restamp, which is what the album, artist and playlist answers want: nothing in them is live.
- Why: a rendition downloaded a moment ago was reported as still downloading for the rest of the window, because the answer being read was built before the download finished. A shorter TTL was rejected: the window is what stops every prefix typed from being a provider process, so 45 s → 5 s multiplies provider calls for the same typing and still shows a stale state, just for a shorter while. Leaving readiness out of the held answer was rejected as the same work done worse: the held copy would then be a lie that nothing should read, and the first reader would have to stamp it too. Re-stamping on read keeps the expensive, unchanging part of the answer (the provider round trip and the matching) held for the window and makes the live part live.
- Also: the restamp must not mutate the held value - several readers are served the same answer at once - so it copies the groups and each group's renditions before writing states into them. The hook is a parameter of the cache rather than the cache knowing about variants: only the caller knows what in its answer goes stale.
- Reversible: yes

## [2026-10-15] A lyrics source that has just failed is left alone, and a failure is still not an answer
- Decision: `lyrics.Service` gets a failure breaker (2 failures in a row, 30 s cooldown) and a four-slot bound on lookups in flight, and bounds each lookup itself (`fetchTimeout`, 10 s) as well as through the caller's context. While the breaker is out, `Resolve` answers nil without asking the source and without writing anything to the cache; after the cooldown one lookup goes through, and a single answer closes the breaker.
- Why: a dead or hanging LRCLIB cost one source call per lyric-less song *per open*, each held open for the caller's whole patience, because a failure deliberately caches nothing - so a burst of songs opened together piled up one request each on the same dead host. That a failure is not an answer is unchanged and still true: the cooldown is about not asking, not about remembering an answer, and nothing is written while it is out.
- Also: the negative answer stays cached per rendition (`sourceLRCLIBNone`), which is a real answer - "asked and there are none" - like an instrumental. A failed lookup leaves the cache exactly as it found it.
- Reversible: yes

## [2026-10-15] A station that cannot be built says why, and a provider that cannot build one is refused
- Decision: `POST /api/v1/radio` refuses a named provider whose capabilities say it cannot build a station (400, naming it), names the providers in the 409 that says none of them has a rendition of the seed, and answers an empty station with a failure rather than `200 {"tracks": []}`: 502 carrying each failed provider and its own words when one failed, 409 with "the providers had no suggestions for this seed" when none did. An empty station is also not saved - the playlist that the default `save` would have created held nothing.
- Why: an empty list says nothing about the reason, and the reasons are different. A provider that cannot build a station was silently dropped from the station, or - when it was the only one asked - reported as having no rendition of the seed, which is not why. A provider that could not be reached is worth another try (5xx, which the SDK retries); one that had nothing to suggest is not, and repeating the request will not produce a station either (409). The client's empty-station message already reads `providerErrors`, so a 200 with nothing in it was the one case it could not explain.
- Also: the `var _ = provider.ErrRadioUnsupported` and its import are gone from the handler - the capability check is the API's own now, and the registry's own `ErrRadioUnsupported` still covers a provider that claims `Caps.Radio` without implementing `RadioProvider`.
- Reversible: yes

## [2026-10-14] A download reports its own progress, and -1 means nothing to count
- Decision: `Provider.Download` takes a `provider.ProgressFunc`, and a provider reports `Progress{Stage, Done, Total}` — stage `download` (bytes arriving) or `convert` (the file being written from the fetched source), with `Done`/`Total` in that stage's own unit (bytes, or milliseconds for a conversion). The manager stores `Status{Stage, Bytes, TotalBytes, Progress}` and serves it from `/media/{id}/status`, where `progress` is the current stage's fraction or **-1 for "nothing countable"** and `totalBytes <= 0` means the total is unknown. A provider that says nothing gets the manager's own fallback: the file being written is stat'ed four times a second, so the number a client draws is always real, never invented.
- Why: the old `Progress` was three invented values (0.10 at the start of a fetch, 0.70 after it, 1 when ready), and a client's loading bar had nothing to show for the part that actually takes the time — yt-dlp's startup and transfer. Only the provider can answer this: it is the one running the tool, so it is the one that knows the expected size and when the fetch becomes a conversion. `-1` is the honest third answer beside 0 and 1: a sweep rather than a fill, for a live stream with no size, or a stage with no bytes of its own.
- Also: the fake provider the browser suite watches a bar move with lives behind `//go:build e2e` and `provider.Extras()`, which returns nil in a released build; the suite builds the server with `-tags e2e` and prints that it did.
- Reversible: yes

## [2026-10-14] The browser suite drives clicks, never modules
- Decision: `musoak-web/e2e` is Selenium + Firefox against the real `musoakd` and the real frontend, and every test drives the app the way a person does — clicks, DOM reads and API reads. No test imports the app's own modules into the page. The player's media element is in the document and named (`data-e2e=audio` for the element in hand, `audio-spare` for the other, with the names following a hand-over), and a reload is `location.reload()`, never `driver.refresh()`.
- Why: a page-side `import('/js/player.js')` re-instantiates the app's module graph under the service worker and boots a *second* app — empty `state`, its own rooms, its own audio element — so a test that reached into the modules measured an app the user was not using (and the harness's first version did exactly that for a day). `driver.refresh()` is a hard reload: the worker is not controlling the new document and every byte comes off the wire again (298 KB against a warm reload's 1.8 KB), so a cache test written on it reports a defect that does not exist. The audio element was the one thing neither a test nor a person with devtools could point at, which is why the app now says which element is which.
- Also: console errors are collected twice, from BiDi and from Firefox's own console output in the driver log, because Selenium's typed BiDi parse raises on an event with no stack trace inside the socket thread where nothing sees it; a self-check test provokes both an error and an uncaught exception and fails if either collector is blind.
- Reversible: yes
- Superseded 2026-10-16 (musoak-web): there is one element again. The song being
  played is downloaded once into memory and the element is handed that copy
  (musoak-web's DECISIONS, "the queue's own download is what plays"), so the
  spare element, the hand-over and `audio-spare` are gone; `data-e2e=audio` is
  the only name, and `ui.player()` still lists every `audio` in the document.

## [2026-10-14] A cover request waits two seconds, and a source that just refused is remembered
- Decision: `artwork.Fetch` no longer waits for the whole download. It waits `waitForImage` (2 s), then answers `ErrPending` (404 + `Cache-Control: no-store`) while the fetch carries on unattended on a context that outlives the request; the next render is served from disk. The provider transfers are queued through a 4-slot semaphore, and a source that failed is left alone for `failedTTL` (5 min, bounded map, dropped on the next success). Every way a cover can be absent — nothing known, still fetching, the CDN refusing — is now answered `no-store`.
- Why: covers are asked for by `<img>` elements, a page asks for a dozen at once, and the handler held the request for up to `fetchTimeout` (20 s). Over HTTP/1.1 the browser answers that by blocking every other image on the same six connections behind it, so one slow cover made the page's artwork — the cached covers included — arrive after it, and a room repainting its rows re-attempted a dead CDN at twenty seconds a time, once a second. A miss the browser keeps is also a cover that cannot appear when the CDN comes back.
- Also: `ErrFetch` still answers 502 on the first failure, so a genuinely broken source is still visible in the log and the client; only repeats inside the TTL are answered as "not here yet".
- Reversible: yes

## [2026-10-14] The frontend's own files are cached and revalidated, not re-fetched
- Decision: the Go frontend server compresses a body only when it writes one (a 304 carries no `Content-Encoding`), memoises each asset's bytes and its tag, computes the frontend digest once for the embedded copy (2 s TTL for `-web-dir`), and marks *versioned* asset URLs (`?v=`, the stamped stylesheet) `public, max-age=31536000, immutable` while the unversioned ones stay `no-cache` so they revalidate. The service worker makes navigation (`/`, `/index.html`) network-first with the last good copy as the fallback, and serves every other shell file cache-first while revalidating it in the background — no `cache: "reload"`, and no timeout on a request that has no cached copy to fall back to.
- Why: `cache: "reload"` on every shell file bypassed both the browser's cache and the server's ETag, so a reload re-downloaded ~600 KB of modules, the font and the icon in full — the "long refresh" the worker was written to remove — and the server's revalidation path was unreachable from a browser. Marking *unversioned* module URLs immutable was rejected: a client without the worker would freeze on one revision for a year.
- Also: the page's `<link rel="icon">` and the brand mark point at a 64 px `icon-64.png` (5 KB) and `apple-touch-icon.png` (25 KB) instead of the 1080 px `icon.png` (357 KB) that is now only the `og:image` a crawler reads; the brand was drawing a 357 KB image at 26 px.
- Reversible: yes

## [2026-10-14] The room's play order is last-turn, and the server sends it
- Decision: the pending play order is rebuilt by who last had a turn — the member whose last turn is oldest goes first, a member who has never played goes before everybody who has, and equally old turns fall back to join order — replacing the played-count-first rule. The order rides every `queue_updated` event as `master` (not the once-a-second `playback` heartbeat), and clients render it rather than deriving one.
- Why: the client derived its own order (round-robin across members in join order) and it disagreed with the server's fairness rule, so after any queue event it advanced to songs the server never chose — the skip that thrashed. Giving the order one authority removes the divergence; last-turn is the rule the user asked for.
- Reversible: yes

## [2026-09-30] Module path
- Decision: `github.com/kyleraykbs/MusOak`, renamed from the v1 path along with the rest of the project.
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

## [2026-10-05] Rooms are rebuilt around the host's player as the clock
- Decision: the room's current song is an anchor — a position, the server instant it is true at, `started` and `paused` — and nothing else. The host's player is the clock: `POST /started` puts the room at the position its file has actually reached and starts the song, `POST /ended` moves the room off it. Everybody else follows one rule: a different song is loaded, the same song within two seconds of the room's position is left alone, the same song further out is seeked. Each client plays the rendition it likes; the room assigns none.
- Why: the readiness gate, per-member variant assignment, dual clock modes and the shortest-file timeline each existed to make members agree about a song nobody had started. The host's player starting it is the agreement, and everything else was machinery that could disagree with it. The old engine was 2,200 lines with 2,400 of tests; the new one owns the queue, the mix and the anchor, and the tests are behavioural.
- Also: the config knobs that belonged to it (`listenTogether.readyFraction`, `readyTimeoutSeconds`) are gone — the config is strict, so an existing config.json with them must drop both keys.
- Reversible: no (the API surface and both clients are built on it)

## [2026-10-05] Six room events, and the play order is spliced rather than sent
- Decision: `member_joined`, `member_left`, `host_changed`, `queue_updated` (one member's queue), `playback` (the whole current state) and `room_closed`. Votes travel inside `playback`, so one event type carries start, pause, resume, seek, vote and advance. The play order is never sent: a client derives it from the member queues and the join order, and when the room moves off a song it removes that item from its queue and its order exactly as the server dropped it — spliced, never rebuilt, because rebuilding restarts the round-robin and would make the next song a different one from the room's.
- Why: state a client can compute is state that can arrive stale. The derivation is only sound if both sides apply the same two transitions, which is why the splice is a contract and not an optimisation. Events also carry a per-room `seq` now: the bus drops events for a client that cannot keep up, and a gap in the counter is what tells it to refetch the snapshot.
- Reversible: yes

## [2026-10-05] The room's clock ends a song when it reaches the host's length
- Decision: the position/length shown in the room is the end metric. On start, resume and seek the server arms an end timer for `durationMs - positionMs + 250ms`; pause disarms it. The host's `/ended` is an early report, not the sole way to advance. End reports name the queue entry as well as the track, so an old end for one copy cannot skip a later copy of the same song. Votes still score 1–5, persist, and the existing threshold rule advances the room.
- Why: the host's player remains the clock, but waiting only on its `ended` event left the room sitting at `2:30 / 2:30` when the report was lost or the player stalled. The displayed clock already had the exact answer; letting the server act on it removes the dead-air window and makes the host report a fast path rather than a single point of failure.
- Also: the timer is armed only after the host starts the song; an unstarted song and a paused song do not run out. The timer and a host report race safely because either one removes the current entry and the other can only name that now-stale entry.
- Reversible: yes

## [2026-10-05] Votes score a song and can skip it; nothing else waits on anybody
- Decision: 1–5 per member per song, mean on screen, persisted for stats, and the existing threshold rule (`skipThreshold`, `minVotersForSkip`, `voterFractionForSkip`) still advances the room.
- Why: scoring and vote-to-skip were explicitly kept; neither asks a client to agree on a rendition or a timeline.
- Reversible: yes

## [2026-10-05] Four live browser checks found four bugs the unit tests could not
- Decision: the rebuild was verified in two real browsers (a host tab and a follower tab, separate profiles) against a real server, and the four bugs that turned up there were fixed and pinned with tests: the clock-sync pong's stamps are top-level fields (reading them from `data` gave every client a garbage offset); a load never seeks to position zero (so a file this client already held at its end played out instantly); the follower's "my file is over, wait" guard paused the *host's* file a hair before its natural end (so the room never heard it end); and a re-queued song whose copy this client still held played out instead of starting again.
- Why: each one is invisible to a reducer test and fatal in a browser. The rule they share: the room's behaviour lives in what the elements do, so the check has to be a real element.
- Reversible: yes
## [2026-10-06] The host's mixed queue is the sole natural progression source
- Decision: the host plays the room's generated mixed queue as its ordinary queue. `POST /sync` reports the current queue item, position, duration and pause state on player changes and once per second; the server timestamps and broadcasts it but never advances on a duration timer. Followers load a different track and seek the same track only beyond two seconds of drift.
- Why: separate room start/end endpoints and a server end timer created competing progression authorities. The host's actual queue already knows when to advance; mirroring that state keeps playlist-style playback simple and makes room playback the same mechanism.
- Also: mixed per-member queues, member identity/board, invites, votes and explicit room controls remain. Readiness endpoints and unused readiness state are removed.
- Reversible: no; both web playback and the Go participant use the sync contract.


## [2026-10-07] A size is read the way YouTube writes it, and an empty station says which provider failed
- Decision: every number the ytmusic helper reads out of an endpoint goes through one of two functions - `count_of`, which reads "86K", "2.2K", "1.2M" and "1,234" as well as plain digits, and `number`, which reads everything else and answers 0 for anything unreadable. Thumbnail dimensions, lengths in seconds and the command's own limit all go through them, so no single unreadable field can take a command down. A length in seconds keeps its fraction (245.6 s is 245600 ms); a count does not. The web client's Radio menu names the providers that failed when a station comes back empty, instead of the bare "Nothing came back to build a radio from."
- Why: ytmusicapi hands a playlist's size through as the display string whenever it cannot read it as digits, and `int("86K")` took the whole helper down - one unreadable count failed playlist search, album search and artist browsing alike (`exit status 1: ... invalid literal for int() with base 10: '86K'`), so a provider that had answered perfectly well was reported as having nothing. A radio that came back empty was the same story from the other end: the server was already sending `providerErrors` explaining itself, and the toast threw them away.
- Reversible: yes

## [2026-10-09] The server drives the room while the host is away, and idle rooms close themselves
- Decision: a room's clock is the host's player while the host has a live event socket, and the server's otherwise. With the host gone the server arms an end timer from the length the host last reported, moves the room on when it fires, and starts the next song itself; the host returning (its socket, or a sync) takes the clock back, and `PlaybackView.drivenBy` says which end is running it. The server drives only while at least one member of the room is listening. Separately, a room that has stopped making progress - nothing playing, the song paused, or a room the server cannot time because nobody is listening - closes itself after two hours.
- Why: a host on a train, a laptop that slept or a page that was closed used to leave the room frozen at the last sync, which is the exact moment a road trip wants music. The old build had a server end timer for every song; this one has it only for the case the host cannot cover, so the two clocks never run at once - which is what the timer was removed for in the first place.
- Also: the publish funnel re-reads the room's clock after every change (`reticLocked`), so a new mutation cannot forget a deadline. Both deadlines run on the injected `Clock`, so tests fire a two-hour close without waiting for one.
- Reversible: yes

## [2026-10-11] The server is the room's clock; clients self-advance and correct
- Decision: the host-as-clock model is deleted. `Manager` runs one tick loop (a goroutine started by the server, one `time.Ticker` a second, stopped by `Close`) and each beat, for every room with a listening member and a current item, advances past finished songs and republishes the anchored `playback` view. `POST /api/v1/rooms/{id}/sync`, `SyncState`, `Manager.Sync`, the `drivenBy` field and every `Clock.AfterFunc` room deadline (`endTimer`, `idleTimer`) are gone; the idle close is computed inside the tick. A song with no known length advances after a fallback window instead of never. Queue changes a member makes are announced immediately; the queue the server's own advance drops rides the beat as a `queue_updated`. Each client is correction-only and self-advancing: it loads a different song on change, seeks only beyond two seconds, and moves to the next item of the derived order by itself when its own file ends — a stale server state naming an item it already advanced past is ignored, not applied.
- Why: the host's player as the clock made the host a single point of failure and made song switching depend on a report arriving; the product wants the server to hold the song and the time, and the download pipeline already makes the switch deterministic. Hosts now only permission playback commands (`Controls`).
- Also: the SDK `Participant` measures its load time (the fetch/prepare) and starts a load early by that much so the audible start lands on the room's beat; the sink's own start latency is not observable through the `Sink` interface, so the file load is what is measured. The web `rooms-state.js` follows every member through one path and advances on the element's `ended`; the view keeps the clock label.
- Reversible: no (both clients and the REST surface are built on it)

## [2026-10-11] The room's download window is a priority, re-derived every time it is read
- Decision: `keepOffline` re-derives the fetch order from the room's current order on every call - a pending song that has moved further out gives its turn to the ones now in front of it, and one that has left the window releases its slot - instead of appending to the order first read. A fetch already under way keeps its slot, is never cancelled and is never queued twice.
- Why: as a list that only grew, songs added by other members waited behind everything queued earlier, so the next song was ready only while the room's order stayed put. That is the difference between one member's queue, where the order never changes under the window, and a room's.
- Also: on the client, a song that has run out before the room's own boundary advances locally rather than pausing. The pause fired a beat before the element's `ended` (the room starts its item at the boundary while the file starts after its load delay, and a trimmed file is shorter than the length the room was told), so `hasEnded()` never became true and the self-advance, which is gated on it, never ran: that was the stall at the end of a song.
- Reversible: yes

## [2026-10-11] A room keeps its own time while somebody is in it, socket or not
- Decision: the tick's gate changed from "a member of this room has a live event socket" to "the room has members". A room with a queue and a member in it starts and advances by itself even when no socket is registered.
- Why: that gate is what a real deployment fell over. With `requireLogin` set, a browser whose socket never registered left a room with a full queue sitting at "nothing playing" indefinitely - observed on amanita: 41 queued, 1 member, no current, and the room began the instant a socket registered as that member. A socket blinking (a reload, a sleep, a proxy hanging up, a browser that cannot hold a websocket) must not be the thing that stops the music; membership is the durable fact, since the grace after a socket closes is what removes a member and that same path closes an empty room.
- Also: the "do not spend the queue on an empty room" intent is kept - an empty room does not exist, it is closed when its last member leaves.
- Reversible: yes

## [2026-10-12] The order holds what comes next; the song on is held apart
- Decision: the room's `master` no longer contains the current item. `beginLocked` splices it off the head as it becomes current, and `recomputeMasterLocked` excludes it while interleaving the pending queues by a fairness ledger (least songs heard first, then longest waiting, then join order) instead of the old fixed pass-per-member round robin.
- Why: the old rebuild ran around the in-flight song, so a queue edit mid-song placed fresh items ahead of the song on — one member's songs showed (and prefetched) wrongly and played twice in a row after it ended ("two of my songs before my friend's"). The ledger reproduces the old order in the no-edit case and alternates correctly after a mid-song edit.
- Reversible: yes (clients tolerate both shapes: the web slices/filters at the current id, the SDK's Pending filters it out).

## [2026-10-15] A shared search answer's waiter is bounded by its own context
- Decision: `searchAnswers.do`/`doLive` take the caller's context and select on it while waiting for somebody else's in-flight answer; the work itself still runs under `context.WithoutCancel`.
- Why: a bare `<-done` meant a client that had hung up waited for an answer it would never read, and a question that never landed held every asker behind it. The cancellation applies to the waiting only: whoever asked first is still owed the answer, and the fan-out is a provider process.
- Also: the leader was left unbounded on purpose. Its provider calls already carry `ProviderTimeout` (15 s each) and what follows is local matching, so a separate deadline here would be a knob with nothing behind it.
- Reversible: yes

## [2026-10-15] The single-provider fetches carry a provider deadline
- Decision: `Registry.Album`, `ArtistAlbums` and `Playlist` wrap the call in `context.WithTimeout(ctx, r.timeout)`, exactly as `searchArtists` and the other fan-outs do.
- Why: a provider that accepts the call and never answers held the request - and the artist sync behind it - until the client gave up, which is the one shape a per-provider timeout exists to stop.
- Reversible: yes

## [2026-10-15] The playlist detail is read in a batch
- Decision: `writePlaylistDetail` loads the entries' tracks with one `TracksFor` and builds them with `buildTracks`, instead of `Track` + `TrackArtists` + `TrackAlbums` + artwork per entry.
- Why: a playlist is the longest list the API renders, and it cost 2N+ round trips. The single-track artwork fallback (a header read before the cover was learned) is kept for the entries the batch leaves bare, so nothing is lost to the batching.
- Reversible: yes

## [2026-10-15] The album and artist search rows keep their per-row detail read
- Decision: the N+1 in `albumSearchAnswer`/`artistSearchAnswer` stays. It is recorded in `agents/TODO.md` as declined.
- Why: removing it needs `AlbumVariantsFor` and a track count added to the narrow store interface, and it saves local SQLite point reads - milliseconds - in a handler whose cost is a provider fan-out of seconds. Surface area in the store has to be paid for by something measurable.
- Reversible: yes (the batch methods can be added whenever a slower store or a bigger page makes it worth it)

## [2026-10-15] Audio is served only from files this server holds
- Decision: provider-backed streaming is removed. Playback resolves a rendition, starts its download, waits for the complete file, then serves the stored file; held files retain normal ranges and `?start=` offsets.
- Why: provider audio has unknown length, cannot be seeked reliably, depends on that provider being reachable at that second, and competes with the download that would make the song durable. A stored file is seekable and is what room join-at-position needs.

## [2026-10-16] Favorites are decimal personal ratings, and room auto-vote is opt-in
- Decision: favorite values are stored as integer tenths and exposed as JSON numbers from 0.1 through 5.0. A room member may automatically submit their saved rating for a newly started song, but automatic ratings only participate in auto-skip when the client explicitly sends `autoSkip: true`; manual votes keep the existing room rule.
- Why: a binary star cannot express the listener's opinion, while decimal tenths preserve exact UI intent without floating-point storage ambiguity. Auto-rating should not silently make a room skip a song; that requires an explicit room control.
- Reversible: yes.

## [2026-10-16] Provider discovery is public when library login is required
- Decision: `/api/v1/providers` and the provider search endpoints for tracks,
  albums, artists and playlists are public reads, even with `requireLogin` on.
  Personal library reads, mutations and account data remain authenticated.
- Why: a guest must be able to search before deciding whether this server has
  anything worth creating an account for. The old middleware treated search as
  library access and the web UI consequently opened a sign-in prompt for every
  guest search.
- Reversible: yes.
## [2026-10-27] Artist catalogues are paged, classified, and read in the background
- Decision: follow YT Music artist-section continuations instead of accepting its first ten releases; carry album/single/EP kind; show albums newest-first by earliest release year; collect an artist's singles into one synthetic `Singles` album; start a paced, resumable catalogue read when an artist opens; and make an album GET wait for a complete tracklist, including adopting a provider release for a track-born album.
- Why: Pink Floyd's first page omitted 27 albums including The Dark Side of the Moon, singles polluted the album list, and a track-born Dark Side of the Moon held only the song that created it. YT Music has no bulk all-songs call: `get_album` is complete per release, so the artist-wide read must be background work.
- Reversible: yes

## 2026-10-27 A cover is served at the width a page draws it
- Decision: `GET /api/v1/artwork/{kind}/{id}?w=` answers a downscaled copy at
  one of 64/112/160/256/336/512 (the nearest at least as wide), cached beside the
  original as `<hash>@<w>.jpg`, answered as image/jpeg with its own ETag and the
  same immutable lifetime. Only downscaling (`scale='min(iw,W)':-2`), one resize
  per (source, width) shared by singleflight, and a failed resize serves the
  original. The singles container no longer emits `artworkUrl`.
- Why: the cache holds 544px provider art next to 4-megapixel phone photos and a
  2.7 MB GIF, and a 32px song row downloaded whatever was there. The container's
  cover was a single's, shown under the container's name.
- Reversible: yes

## 2026-10-27 Songs are kept by what the household does with them
- Decision: the media cache sweeps hourly (and once at startup, so a restart
  does not postpone it): a track in any playlist or favourited by anybody is
  kept for ever and is never swept, quota or not; a played track is kept a week
  after its newest play; each further account that has played it adds a month -
  one listener a month, two two, three three - up to a year (at which point a
  play only resets the timer); a fetched track nobody played is kept a week from
  its download. `local`/`upload` renditions are never swept -
  they are the only copy. The quota stays soft and off by default (0): when set,
  it evicts least-recently-served renditions that are not pinned.
- Why: Kyle's policy, verbatim: "If song is in a playlist -> Keep locally
  forever... played -> a week... played by more than 1 person -> 3 months (every
  time a person plays the song it should reset the 3 month timer)", extended to
  "for each new person that listens ... add a month up until ... 1 year in which
  case each listen should still just reset the timer", "keep 1 week from
  download" for the never-played, and "By default there should be no quota ...
  this is a local media server, I want it to be able to store as much shit as we
  want". The three-month floor for two listeners is his earlier rule and is what
  the year cap is reached from.
- Reversible: yes

## 2026-10-28 A play stops every other download, whatever its age
- Decision: `enterPrimary` cancels every other background fetch, not only those
  younger than `resumeWindow` (20s, now gone). What is stopped goes on the paused
  list and is resumed when the play is over, as before.
- Why: the age exemption let a long-running background download keep half the
  provider pipe while the song somebody was waiting to hear was fetched - the
  web player's own row for the old song kept its download bar, and the new song
  waited behind it. Kyle's rule: the queue's transfers follow the song being
  played, exactly.
- Reversible: yes

## 2026-10-30 The retention sweep reads a clock a test can move
- Decision: `media.Manager` carries `now func() time.Time` (set to `time.Now` by
  `New`), and the sweep reads it. `TestRetentionWalksTheCalendar` walks it through
  a year - +8d, +31d, +91d, +361d - and asserts exactly which songs survive, with
  a played song proving a play resets the clock.
- Why: a policy counted in months cannot be exercised by waiting, and a sweep
  that calls `time.Now()` directly is a sweep no test can walk a year of. Kyle
  chose the seam over the pure-function table alone.
- Reversible: yes
