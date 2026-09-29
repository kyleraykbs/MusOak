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


def duration_ms(item):
    """Read a duration from whichever field this endpoint happens to use."""
    if not item:
        return 0
    seconds = item.get("duration_seconds")
    if seconds:
        return int(seconds * 1000)
    for key in ("duration", "length"):
        raw = item.get(key)
        if not raw or not isinstance(raw, str):
            continue
        parts = raw.strip().split(":")
        try:
            numbers = [int(p) for p in parts]
        except ValueError:
            continue
        total = 0
        for number in numbers:
            total = total * 60 + number
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
            area = int(image.get("width") or 0) * int(image.get("height") or 0)
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
    return {
        "id": item.get("videoId", "") or "",
        "title": item.get("title", "") or "",
        "artists": artists_of(item),
        "album": album_of(item),
        "durationMs": duration_ms(item),
        "artworkUrl": artwork_of(item),
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
                "trackCount": int(item.get("trackCount") or 0),
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
        "trackCount": int(raw.get("trackCount") or 0),
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
                    "trackCount": int(item.get("trackCount") or 0),
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
            "trackCount": int(item.get("itemCount") or 0),
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
        "trackCount": int(raw.get("trackCount") or len(tracks)),
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
        limit = int(argv[4]) if len(argv) > 4 else 20
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
        limit = int(argv[3]) if len(argv) > 3 else 25
        result = radio(yt, argv[2], limit)
    elif command == "playlists":
        if len(argv) < 3:
            raise SystemExit("usage: playlists QUERY [LIMIT]")
        limit = int(argv[3]) if len(argv) > 3 else 20
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
