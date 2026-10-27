# ytmusicapi helper: search, album tracklists and radio.
#
# Invoked as `python3 - <command> [arguments]` with this source on stdin, so the
# Go side needs no temporary files.
#
#   search songs   QUERY [LIMIT]
#   search albums  QUERY [LIMIT]
#   search artists QUERY [LIMIT]
#   album  BROWSE_ID
#   artist BROWSE_ID
#   radio  VIDEO_ID [LIMIT]
#   playlists QUERY [LIMIT]
#   playlist  BROWSE_ID
#
# Every command prints one JSON document on stdout.
import json
import sys
import time

from ytmusicapi import YTMusic


def number(value):
    """An integer from a field the endpoint filled in, or 0.

    The endpoints are not consistent about types: a size arrives as "86K", a
    thumbnail dimension as "1280px", a length sometimes as a number and
    sometimes as a string, and the display form leaks through wherever
    ytmusicapi could not read it as digits. Nothing here is worth failing a
    search over - a song that plays with no duration shown beats a provider
    reported as broken - so every read of an endpoint's number goes through
    this, and anything unreadable is zero.
    """
    if isinstance(value, bool) or value is None:
        return 0
    if isinstance(value, (int, float)):
        return int(value)
    if not isinstance(value, str):
        return 0
    try:
        return int(float(value.strip()))
    except (ValueError, OverflowError):
        return 0


def count_of(value):
    """A size, however the endpoint words it.

    A count only decorates a list, so an unreadable one is none of them: this
    is `number` with the page's own suffixes understood.
    """
    if isinstance(value, str):
        text = value.strip().replace(",", "")
        if text:
            scale = {"k": 1000, "m": 1000000, "b": 1000000000}.get(text[-1].lower(), 1)
            if scale > 1:
                text = text[:-1]
            try:
                return int(float(text) * scale)
            except (ValueError, OverflowError):
                return 0
    return number(value)


def duration_ms(item):
    """Read a duration from whichever field this endpoint happens to use."""
    if not item:
        return 0
    seconds = item.get("duration_seconds")
    if seconds:
        # A length in seconds is the one place a fraction is real - 245.6
        # seconds is 245600 ms - so it is not read as a whole number, and a
        # length nobody can read is none rather than a guess.
        try:
            return int(float(seconds) * 1000)
        except (TypeError, ValueError, OverflowError):
            return 0
    for key in ("duration", "length"):
        raw = item.get(key)
        if not raw or not isinstance(raw, str):
            continue
        parts = raw.strip().split(":")
        try:
            numbers = [int(p.strip()) for p in parts]
        except ValueError:
            continue
        total = 0
        for part in numbers:
            total = total * 60 + part
        return total * 1000
    return 0


def artwork_of(item):
    """Pick the largest thumbnail these endpoints embed under several names."""
    best = ""
    best_area = -1
    for key in ("thumbnails", "thumbnail"):
        for image in item.get(key) or []:
            if not isinstance(image, dict):
                continue
            url = image.get("url") or ""
            if not url:
                continue
            area = number(image.get("width")) * number(image.get("height"))
            if area >= best_area:
                best, best_area = url, area
    return best


def artists_of(item):
    names = []
    for artist in item.get("artists") or []:
        name = artist.get("name") if isinstance(artist, dict) else artist
        if name:
            names.append(name)
    return names


def album_of(item):
    album = item.get("album")
    if isinstance(album, dict):
        return album.get("name", "") or ""
    return album or ""


def owner_of(item):
    """Playlist authors arrive as a string when searched and a dict when fetched."""
    author = item.get("author")
    if isinstance(author, dict):
        return author.get("name", "") or ""
    return author or ""


def song(item):
    # YT Music mixes songs and music videos in the same lists, and a video's
    # audio is the video: intros, skits and all. Only MUSIC_VIDEO_TYPE_ATV is the
    # album recording, so the caller is told which is which.
    video_type = (item.get("videoType") or "").strip()
    return {
        "id": item.get("videoId", "") or "",
        "title": item.get("title", "") or "",
        "artists": artists_of(item),
        "album": album_of(item),
        "durationMs": duration_ms(item),
        "artworkUrl": artwork_of(item),
        "video": bool(video_type) and video_type != "MUSIC_VIDEO_TYPE_ATV",
    }


def release_kind(item, section):
    """Whether a release is an album, a single or an EP.

    The endpoint says so in the entry's own `type` where it fills one in, and
    where it does not - the first page of an artist's albums section omits it
    while the continuation carries it - the section the entry came from says
    it: a release in `singles` is a single, one in `albums` is an album. EPs
    are singles with another name, which is why they live in that section.
    """
    kind = str(item.get("type", "") or "").strip().lower()
    if kind in ("album", "single", "ep"):
        return kind
    return "single" if section == "singles" else "album"


def release(item, section):
    """One release of an artist page, in the shape the caller stores."""
    return {
        "id": item.get("browseId", "") or "",
        "title": item.get("title", "") or "",
        "artists": artists_of(item),
        "year": str(item.get("year", "") or ""),
        "trackCount": count_of(item.get("trackCount")),
        "kind": release_kind(item, section),
        "artworkUrl": artwork_of(item),
    }


def with_retry(run, attempts=3, pause=0.5):
    """Run one call to YouTube Music, retrying a refused one.

    The endpoint answers with an empty body when it is asked for a lot at once
    - ytmusicapi surfaces that as a JSON decode error - and the refusal is over
    in a moment. A whole artist is dozens of calls, so one refused request must
    not lose an album: three tries with a growing pause turn a burst into a slow
    read instead of a failed one, and a failure that survives them is real.
    """
    for attempt in range(attempts):
        try:
            return run()
        except Exception:
            if attempt == attempts - 1:
                raise
            time.sleep(pause)
            pause *= 2


def search(yt, kind, query, limit):
    raw = with_retry(lambda: yt.search(query, filter=kind, limit=limit)) or []
    if kind == "songs":
        return [song(item) for item in raw if item.get("videoId")]
    if kind == "albums":
        return [
            {
                "id": item.get("browseId", "") or "",
                "title": item.get("title", "") or "",
                "artists": artists_of(item),
                "year": str(item.get("year", "") or ""),
                "trackCount": count_of(item.get("trackCount")),
                "kind": release_kind(item, "albums"),
                "artworkUrl": artwork_of(item),
            }
            for item in raw
            if item.get("browseId")
        ]
    if kind == "artists":
        return [
            {
                "id": item.get("browseId", "") or "",
                "name": item.get("artist", "") or item.get("title", "") or "",
                "artworkUrl": artwork_of(item),
            }
            for item in raw
            if item.get("browseId")
        ]
    raise SystemExit(f"unknown search kind: {kind}")


def album(yt, browse_id):
    raw = with_retry(lambda: yt.get_album(browse_id)) or {}
    return {
        "id": browse_id,
        "title": raw.get("title", "") or "",
        "artists": artists_of(raw),
        "year": str(raw.get("year", "") or ""),
        "trackCount": count_of(raw.get("trackCount")),
        "kind": release_kind(raw, "albums"),
        "artworkUrl": artwork_of(raw),
        # One call, the whole tracklist: get_album has no page size and no
        # continuation, so an album read is complete by construction.
        "tracks": [song(track) for track in (raw.get("tracks") or []) if track.get("videoId")],
    }


def section_releases(yt, block, section, artist_id):
    """One section of an artist page: its first page and everything after it.

    `get_artist` answers with the top of each section - ten releases - plus a
    `params` for the rest, and ytmusicapi's own continuation call is what turns
    those ten into the artist's whole discography. Pink Floyd's page is ten
    releases without it and thirty-seven with, and The Dark Side of the Moon is
    one of the twenty-seven the first page never had: the endpoint chooses the
    ten, and this is the only way to learn the rest.

    A continuation that fails is retried before it is given up on: the endpoint
    refuses requests in bursts when it is asked for a lot at once, and a burst
    is over in a moment. A continuation that still fails raises, because a
    silently short discography is the bug this whole read exists to fix - the
    caller records the failure and asks again later.
    """
    out = [release(item, section) for item in (block.get("results") or []) if item.get("browseId")]
    params = block.get("params") or ""
    browse = block.get("browseId") or artist_id
    if params and browse:
        more = with_retry(lambda: yt.get_artist_albums(browse, params, limit=None)) or []
        out.extend(release(item, section) for item in more if item.get("browseId"))
    # The continuation repeats the first page before continuing it, so the two
    # are the same list read twice at the seam.
    seen = set()
    unique = []
    for entry in out:
        if entry["id"] in seen:
            continue
        seen.add(entry["id"])
        unique.append(entry)
    return unique


def artist(yt, browse_id):
    raw = with_retry(lambda: yt.get_artist(browse_id)) or {}
    # Albums and singles come back apart because they are apart: a single is how
    # a song was released, not a record, and it is the caller's to place. Either
    # way every entry carries its own kind.
    return {
        "name": raw.get("name", "") or "",
        "artworkUrl": artwork_of(raw),
        "albums": section_releases(yt, raw.get("albums") or {}, "albums", browse_id),
        "singles": section_releases(yt, raw.get("singles") or {}, "singles", browse_id),
    }


def radio(yt, video_id, limit):
    raw = with_retry(lambda: yt.get_watch_playlist(videoId=video_id, limit=limit)) or {}
    tracks = [song(item) for item in (raw.get("tracks") or [])]
    # The first entry is usually the seed itself; the caller already has it.
    return [track for track in tracks if track["id"] and track["id"] != video_id]


def playlists(yt, query, limit):
    # Search answers in whole pages (20, 40, ...), so trim to the asked-for size.
    raw = (with_retry(lambda: yt.search(query, filter="playlists", limit=limit)) or [])[:limit]
    return [
        {
            "id": item.get("browseId", "") or "",
            "title": item.get("title", "") or "",
            "owner": owner_of(item),
            "description": item.get("description", "") or "",
            "trackCount": count_of(item.get("itemCount")),
            "artworkUrl": artwork_of(item),
        }
        for item in raw
        if item.get("browseId")
    ]


def playlist(yt, browse_id):
    # limit=None fetches the whole tracklist; the default stops at 100.
    raw = with_retry(lambda: yt.get_playlist(browse_id, limit=None)) or {}
    tracks = [song(track) for track in (raw.get("tracks") or []) if track.get("videoId")]
    return {
        "id": browse_id,
        "title": raw.get("title", "") or "",
        "owner": owner_of(raw),
        "description": raw.get("description", "") or "",
        "trackCount": count_of(raw.get("trackCount")) or len(tracks),
        "artworkUrl": artwork_of(raw),
        "tracks": tracks,
    }


def main(argv):
    if len(argv) < 2:
        raise SystemExit("usage: ytmusic.py <command> ...")
    yt = YTMusic()
    command = argv[1]

    if command == "search":
        if len(argv) < 4:
            raise SystemExit("usage: search <songs|albums|artists> QUERY [LIMIT]")
        limit = number(argv[4]) if len(argv) > 4 else 20
        result = search(yt, argv[2], argv[3], limit)
    elif command == "album":
        if len(argv) < 3:
            raise SystemExit("usage: album BROWSE_ID")
        result = album(yt, argv[2])
    elif command == "artist":
        if len(argv) < 3:
            raise SystemExit("usage: artist BROWSE_ID")
        result = artist(yt, argv[2])
    elif command == "radio":
        if len(argv) < 3:
            raise SystemExit("usage: radio VIDEO_ID [LIMIT]")
        limit = number(argv[3]) if len(argv) > 3 else 25
        result = radio(yt, argv[2], limit)
    elif command == "playlists":
        if len(argv) < 3:
            raise SystemExit("usage: playlists QUERY [LIMIT]")
        limit = number(argv[3]) if len(argv) > 3 else 20
        result = playlists(yt, argv[2], limit)
    elif command == "playlist":
        if len(argv) < 3:
            raise SystemExit("usage: playlist BROWSE_ID")
        result = playlist(yt, argv[2])
    else:
        raise SystemExit(f"unknown command: {command}")

    json.dump(result, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
