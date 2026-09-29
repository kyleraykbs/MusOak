# Search YouTube Music and print the hits as JSON on stdout.
#
# Invoked as `python3 - QUERY [LIMIT]` with this source on stdin, so the Go
# side needs no temporary files.
import json
import sys

from ytmusicapi import YTMusic


def main() -> int:
    if len(sys.argv) < 2 or not sys.argv[1].strip():
        print("usage: search.py QUERY [LIMIT]", file=sys.stderr)
        return 2
    query = sys.argv[1]
    limit = int(sys.argv[2]) if len(sys.argv) > 2 else 20

    yt = YTMusic()
    raw = yt.search(query, filter="songs", limit=limit)

    hits = []
    for item in raw or []:
        video_id = item.get("videoId")
        if not video_id:
            continue
        artists = [a.get("name", "") for a in (item.get("artists") or []) if a.get("name")]
        album = (item.get("album") or {}).get("name", "") or ""
        duration_seconds = item.get("duration_seconds") or 0
        hits.append(
            {
                "id": video_id,
                "title": item.get("title", "") or "",
                "artists": artists,
                "album": album,
                "durationMs": int(duration_seconds * 1000),
            }
        )

    json.dump(hits, sys.stdout)
    return 0


if __name__ == "__main__":
    sys.exit(main())
