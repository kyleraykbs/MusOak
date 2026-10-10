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


def search(yt, kind, query, limit):
    raw = yt.search(query, filter=kind, limit=limit) or []
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
    raw = yt.get_album(browse_id) or {}
    return {
        "id": browse_id,
        "title": raw.get("title", "") or "",
        "artists": artists_of(raw),
        "year": str(raw.get("year", "") or ""),
        "trackCount": count_of(raw.get("trackCount")),
        "artworkUrl": artwork_of(raw),
        "tracks": [song(track) for track in (raw.get("tracks") or []) if track.get("videoId")],
    }


def artist(yt, browse_id):
    raw = yt.get_artist(browse_id) or {}
    albums = []
    for section in ("albums", "singles", "ep"):
        block = raw.get(section) or {}
        for item in block.get("results") or []:
            if not item.get("browseId"):
                continue
            albums.append(
                {
                    "id": item.get("browseId", ""),
                    "title": item.get("title", "") or "",
                    "artists": artists_of(item),
                    "year": str(item.get("year", "") or ""),
                    "trackCount": count_of(item.get("trackCount")),
                    "artworkUrl": artwork_of(item),
                }
            )
    return {"name": raw.get("name", "") or "", "artworkUrl": artwork_of(raw), "albums": albums}


def radio(yt, video_id, limit):
    raw = yt.get_watch_playlist(videoId=video_id, limit=limit) or {}
    tracks = [song(item) for item in (raw.get("tracks") or [])]
    # The first entry is usually the seed itself; the caller already has it.
    return [track for track in tracks if track["id"] and track["id"] != video_id]


def playlists(yt, query, limit):
    # Search answers in whole pages (20, 40, ...), so trim to the asked-for size.
    raw = (yt.search(query, filter="playlists", limit=limit) or [])[:limit]
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
    raw = yt.get_playlist(browse_id, limit=None) or {}
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
