#!/usr/bin/env bash
# Exercises the whole REST API with curl against a running prismusicd.
#
#   nix develop                                  # provides ffmpeg, jq, curl
#   prismusicd --config /tmp/smoke.json &        # or: prism serve --config ...
#   scripts/api-smoke.sh http://127.0.0.1:8080
#
# Every check prints ok/FAIL and the script exits non-zero if anything failed.
# Search hits the real providers, so run it with ytmusic enabled.
set -uo pipefail

base="${1:-http://127.0.0.1:8080}"
workdir="$(mktemp -d)"
tone="$workdir/track.opus"

ffmpeg -hide_banner -loglevel error -y \
  -f lavfi -i "sine=frequency=440:duration=0.6" \
  -c:a libopus -f opus "$tone"

auth=""
member=""
pass=0
fail=0

check() { # name want got
  if [ "$2" = "$3" ]; then
    pass=$((pass + 1))
    printf 'ok   %-38s %s\n' "$1" "$3"
  else
    fail=$((fail + 1))
    printf 'FAIL %-38s want %s, got %s\n' "$1" "$2" "$3"
    sed -n '1,3p' "$workdir/body" | sed 's/^/       /'
  fi
}

body() { cat "$workdir/body"; }
jqr() { jq -r "$1" < "$workdir/body"; }

req() { # method path [data] -> prints status code
  local method=$1 path=$2 data=${3:-}
  local -a args=(-s -o "$workdir/body" -w '%{http_code}' -X "$method")
  [ -n "${auth:-}" ] && args+=(-H "Authorization: Bearer $auth")
  [ -n "${member:-}" ] && args+=(-H "X-Member-Id: $member")
  if [ -n "$data" ]; then
    args+=(-H 'Content-Type: application/json' -d "$data")
  fi
  curl "${args[@]}" "$base$path"
}

echo "== basics =="
check "healthz" 200 "$(req GET /healthz)"
check "openapi.yaml" 200 "$(req GET /api/v1/openapi.yaml)"
check "providers" 200 "$(req GET /api/v1/providers)"
printf '     providers: %s\n' "$(jqr '[.providers[].name] | join(",")')"
check "clock" 200 "$(req GET /api/v1/clock)"

echo "== search (live providers) =="
check "search" 200 "$(req GET '/api/v1/search?q=Rick%20Astley%20Never%20Gonna%20Give%20You%20Up&limit=3')"
echo "     groups: $(jqr '.groups | length'), providerErrors: $(jqr '.providerErrors // [] | length')"

echo "== local library =="
check "library/import" 200 "$(req POST /api/v1/library/import "{\"path\":\"$tone\"}")"
local_track="$(jqr '.imported[0].track.id')"
variant="$(jqr '.imported[0].variant.id')"
printf '     imported %s as %s (%s)\n' "$(jqr '.imported[0].track.title')" "$variant" "$(jqr '.imported[0].variant.provider')"
check "library/import bad path" 400 "$(req POST /api/v1/library/import '{"path":"/nope/nothing.opus"}')"

echo "== tracks =="
check "tracks/{id}" 200 "$(req GET "/api/v1/tracks/$local_track")"
check "tracks/{id}/variants" 200 "$(req GET "/api/v1/tracks/$local_track/variants")"
printf '     variants: %s\n' "$(jqr '[.variants[].provider] | join(",")')"
check "tracks/{id}/resolve" 200 "$(req POST "/api/v1/tracks/$local_track/resolve")"
check "tracks/{id} unknown" 404 "$(req GET '/api/v1/tracks/00000000-0000-0000-0000-000000000000')"
check "tracks/{id} malformed" 400 "$(req GET '/api/v1/tracks/not-a-uuid')"

echo "== media =="
check "media status" 200 "$(req GET "/api/v1/media/$variant/status")"
check "media status ready" ready "$(jqr .state)"
check "media download (idempotent)" 200 "$(req POST "/api/v1/media/$variant/download?wait=1")"
code=$(curl -s -o "$workdir/media" -D "$workdir/headers" -w '%{http_code}' "$base/api/v1/media/$variant")
check "media bytes" 200 "$code"
check "media has ETag" yes "$(grep -qi '^etag:' "$workdir/headers" && echo yes || echo no)"
echo "     served $(stat -c %s "$workdir/media") bytes"
code=$(curl -s -o "$workdir/range" -w '%{http_code}' -H 'Range: bytes=0-99' "$base/api/v1/media/$variant")
check "media range" 206 "$code"
check "media range length" 100 "$(stat -c %s "$workdir/range")"
check "media file unknown" 404 "$(curl -s -o /dev/null -w '%{http_code}' "$base/api/v1/media/00000000-0000-0000-0000-000000000000")"

echo "== accounts, favorites, ranking =="
username="curl-$(date +%s)"
check "auth register" 201 "$(req POST /api/v1/auth/register "{\"username\":\"$username\",\"password\":\"hunter2hunter2\"}")"
auth="$(jqr .token)"
check "me" 200 "$(req GET /api/v1/me)"
check "me authenticated" true "$(jqr .authenticated)"
check "favorites add" 204 "$(req POST /api/v1/me/favorites "{\"trackId\":\"$local_track\"}")"
check "favorites list" 200 "$(req GET /api/v1/me/favorites)"
check "favorites has one" 1 "$(jqr '.tracks | length')"
check "favorites remove" 204 "$(req DELETE "/api/v1/me/favorites/$local_track")"
check "favorites remove again" 404 "$(req DELETE "/api/v1/me/favorites/$local_track")"
check "ranking put" 200 "$(req PUT /api/v1/me/providers/ranking '{"ranking":["local","ytmusic"]}')"
check "ranking effective" local "$(jqr '.effective[0]')"
check "ranking get" 200 "$(req GET /api/v1/me/providers/ranking)"
check "ranking unknown provider" 400 "$(req PUT /api/v1/me/providers/ranking '{"ranking":["deezer"]}')"
check "auth login" 200 "$(req POST /api/v1/auth/login "{\"username\":\"$username\",\"password\":\"hunter2hunter2\"}")"
check "auth login wrong password" 401 "$(req POST /api/v1/auth/login "{\"username\":\"$username\",\"password\":\"wrong-password\"}")"

echo "== rooms =="
check "rooms create" 201 "$(req POST /api/v1/rooms '{"name":"curl smoke","controls":"everyone"}')"
room="$(jqr .room.id)"
printf '     room %s, host %s\n' "$room" "$(jqr .memberId)"
check "rooms list" 200 "$(req GET /api/v1/rooms)"
check "rooms get" 200 "$(req GET "/api/v1/rooms/$room")"
check "rooms get unknown" 404 "$(req GET '/api/v1/rooms/does-not-exist')"

saved_auth="$auth"
auth=""
member=""
check "rooms join (guest)" 200 "$(req POST "/api/v1/rooms/$room/join")"
member="$(jqr .memberId)"
check "rooms queue (guest)" 200 "$(req POST "/api/v1/rooms/$room/queue" "{\"trackId\":\"$local_track\"}")"
current_track="$(jqr '.current.item.trackId // .queue[0].trackId')"
check "rooms ready (guest)" 200 "$(req POST "/api/v1/rooms/$room/ready" "{\"trackId\":\"$current_track\",\"variantId\":\"$variant\",\"durationMs\":600}")"
check "rooms vote (guest)" 200 "$(req POST "/api/v1/rooms/$room/vote" '{"score":4}')"
check "rooms leave (guest)" 204 "$(req POST "/api/v1/rooms/$room/leave")"

auth="$saved_auth"
member=""
check "rooms queue (host)" 200 "$(req POST "/api/v1/rooms/$room/queue" "{\"trackId\":\"$local_track\"}")"
check "rooms pause before start" 409 "$(req POST "/api/v1/rooms/$room/pause")"
check "rooms ready (host)" 200 "$(req POST "/api/v1/rooms/$room/ready" "{\"trackId\":\"$current_track\",\"variantId\":\"$variant\",\"durationMs\":600}")"
check "rooms vote (host)" 200 "$(req POST "/api/v1/rooms/$room/vote" '{"score":5}')"
check "rooms seek (host)" 200 "$(req POST "/api/v1/rooms/$room/seek" '{"positionMs":100}')"
check "rooms pause after start" 200 "$(req POST "/api/v1/rooms/$room/pause")"
check "rooms resume" 200 "$(req POST "/api/v1/rooms/$room/resume")"
items="$(jqr '[.queue[].id] | reverse | @json')"
check "rooms reorder" 200 "$(req POST "/api/v1/rooms/$room/queue/reorder" "{\"itemIds\":$items}")"
check "rooms reorder mismatch" 400 "$(req POST "/api/v1/rooms/$room/queue/reorder" '{"itemIds":[]}')"
check "rooms skip" 200 "$(req POST "/api/v1/rooms/$room/skip")"
check "rooms leave (host, closes the room)" 204 "$(req POST "/api/v1/rooms/$room/leave")"
check "rooms get after close" 404 "$(req GET "/api/v1/rooms/$room")"

echo "== auth revocation =="
check "auth logout" 204 "$(req POST /api/v1/auth/logout)"
check "favorites after logout" 401 "$(req GET /api/v1/me/favorites)"

echo
echo "curl sweep: $pass passed, $fail failed"
rm -rf "$workdir"
[ "$fail" -eq 0 ]
