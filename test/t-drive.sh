#!/bin/bash
# Shared drive — self-contained.
#
# Starts its own bkn, because the drive is two scripts and a hook, and those
# can only be installed by the CLI on the machine holding the data.
set -u
BKN=${BKN_BIN:-$(cd "$(dirname "$0")/.." && pwd)/bin/bkn}
EX=$(cd "$(dirname "$0")/.." && pwd)/examples/drive
WORK=$(mktemp -d)
PORT=${PORT:-$((20000 + RANDOM % 20000))}
PASS=0; FAIL=0
cleanup() { [ -n "${PID:-}" ] && kill "$PID" 2>/dev/null; rm -rf "$WORK"; }
trap cleanup EXIT

chk() {
  if [ "$2" = "$3" ]; then PASS=$((PASS+1)); printf "   \033[32mok\033[0m   %-50s %s\n" "$1" "$2"
  else FAIL=$((FAIL+1)); printf "   \033[31mFAIL\033[0m %-50s want %s got %s\n" "$1" "$2" "$3"; fi
}
chkin() {
  case "$3" in *"$2"*) PASS=$((PASS+1)); printf "   \033[32mok\033[0m   %-50s ~%s\n" "$1" "$2";;
  *) FAIL=$((FAIL+1)); printf "   \033[31mFAIL\033[0m %-50s want ~%s got %s\n" "$1" "$2" "$3";; esac
}
j() { python3 -c "import json,sys
d=json.load(sys.stdin)
for k in '$1'.split('.'):
    if d is None: break
    d = d[int(k)] if isinstance(d,list) else d.get(k)
print('' if d is None else ('true' if d is True else ('false' if d is False else d)))" 2>/dev/null; }

setup() { # every fixture must succeed, loudly
  if ! out=$("$@" 2>&1); then
    printf "   \033[31mSETUP FAILED\033[0m %s\n   %s\n" "$*" "$(printf '%s' "$out" | head -c 200)"
    exit 1
  fi
}

export BKN_DATA="$WORK/b.db"
export BKN_ADMIN_TOKEN="adm-$RANDOM$RANDOM"
export BKN_ENCRYPTION_KEY=$(printf 'a%.0s' $(seq 32))

setup "$BKN" auth user create alice@drive.test --password drive-pw-alice-1
setup "$BKN" auth user create bob@drive.test --password drive-pw-bob-1
setup "$BKN" auth org create acme Acme
setup "$BKN" auth member add acme alice@drive.test --role admin
setup "$BKN" auth member add acme bob@drive.test --role member
setup "$BKN" files ns create drive-blobs --signing-key auto
setup "$BKN" script create drive --file "$EX/drive.js" --run-access user
setup "$BKN" script create drive-upload --file "$EX/drive-upload.js"
setup "$BKN" hooks create drive-upload --script drive-upload --max-bytes 26214400

"$BKN" serve --host 127.0.0.1 --port "$PORT" >"$WORK/srv.log" 2>&1 &
PID=$!
B="http://127.0.0.1:$PORT"
for _ in $(seq 1 50); do curl -sf "$B/_health" >/dev/null && break; sleep 0.1; done

login() {
  curl -s -X POST "$B/v1/auth/login" -H 'Content-Type: application/json' \
    -d "{\"email\":\"$1\",\"password\":\"$2\"}" | j tokens.access_token
}
ALICE=$(login alice@drive.test drive-pw-alice-1)
BOB=$(login bob@drive.test drive-pw-bob-1)
ALICE_ID=$("$BKN" auth user show alice@drive.test 2>/dev/null | j user.id)

# op <token> <json> -> the script's value, or its error document
op() {
  curl -s -X POST "$B/v1/script/drive/run" -H "Authorization: Bearer $1" \
    -H 'Content-Type: application/json' -d "$2"
}
adminop() { op "$BKN_ADMIN_TOKEN" "$1"; }
code() { curl -s -o /dev/null -w '%{http_code}' -X POST "$B/v1/script/drive/run" \
  -H "Authorization: Bearer $1" -H 'Content-Type: application/json' -d "$2"; }

upload() { # upload <token> <drive> <path> <name> <content>
  local b64; b64=$(printf '%s' "$5" | base64 -w0)
  curl -s -X POST "$B/v1/hooks/drive-upload" -H "Authorization: Bearer $1" \
    -H 'Content-Type: application/json' \
    -d "{\"drive\":\"$2\",\"path\":\"$3\",\"name\":\"$4\",\"content_base64\":\"$b64\",\"content_type\":\"text/plain\"}"
}
upcode() { local b64; b64=$(printf '%s' "$5" | base64 -w0)
  curl -s -o /dev/null -w '%{http_code}' -X POST "$B/v1/hooks/drive-upload" \
    -H "Authorization: Bearer $1" -H 'Content-Type: application/json' \
    -d "{\"drive\":\"$2\",\"path\":\"$3\",\"name\":\"$4\",\"content_base64\":\"$b64\"}"; }

echo "-- personal drive"
chk "an empty drive lists nothing"        "0"       "$(op "$ALICE" '{"op":"ls","drive":"user:me"}' | j value.count)"
chk "mkdir creates a folder"              "true"    "$(op "$ALICE" '{"op":"mkdir","drive":"user:me","path":"/","name":"reports"}' | j value.created)"
chk "the folder is listed"                "reports" "$(op "$ALICE" '{"op":"ls","drive":"user:me"}' | j value.entries.0.name)"
chkin "a duplicate name is refused"       "already exists" "$(op "$ALICE" '{"op":"mkdir","drive":"user:me","path":"/","name":"reports"}')"
chkin "a missing parent is refused"       "does not exist" "$(op "$ALICE" '{"op":"mkdir","drive":"user:me","path":"/nope","name":"x"}')"
chkin "path traversal is refused"         "may not be .."  "$(op "$ALICE" '{"op":"mkdir","drive":"user:me","path":"/../etc","name":"x"}')"
chkin "a name with a slash is refused"    "may not contain" "$(op "$ALICE" '{"op":"mkdir","drive":"user:me","path":"/","name":"a/b"}')"

echo "-- upload and download"
chk "upload lands in the folder"          "/reports/q3.txt" "$(upload "$ALICE" user:me /reports q3.txt 'hello drive' | j entry.path)"
chk "and is sized by decoded bytes"       "11"      "$(op "$ALICE" '{"op":"stat","drive":"user:me","path":"/reports/q3.txt"}' | j value.entry.size)"
chk "usage reflects the upload"           "11"      "$(op "$ALICE" '{"op":"quota","drive":"user:me"}' | j value.usage.used_bytes)"
chk "a duplicate upload is a conflict"    "409"     "$(upcode "$ALICE" user:me /reports q3.txt 'again')"
chk "an unauthenticated upload is 401"    "401"     "$(upcode "" user:me / x.txt 'x')"
chkin "download returns a signed url"     "sig="    "$(op "$ALICE" '{"op":"download","drive":"user:me","path":"/reports/q3.txt"}' | j value.url)"

echo "-- isolation between users"
chk "bob cannot list alice's drive"       "422"     "$(code "$BOB" "{\"op\":\"ls\",\"drive\":\"user:$ALICE_ID\"}")"
chkin "and is told why"                   "no access" "$(op "$BOB" "{\"op\":\"ls\",\"drive\":\"user:$ALICE_ID\"}")"
chk "bob cannot upload there"             "403"     "$(upcode "$BOB" "user:$ALICE_ID" / sneak.txt 'x')"
chk "bob's own drive is his own"          "0"       "$(op "$BOB" '{"op":"ls","drive":"user:me"}' | j value.count)"

echo "-- sharing one entry, not the drive"
chk "alice shares the file with bob"      "read"    "$(op "$ALICE" '{"op":"share","drive":"user:me","path":"/reports/q3.txt","user":"bob@drive.test"}' | j value.access)"
chkin "bob can download the shared file"  "sig="    "$(op "$BOB" "{\"op\":\"download\",\"drive\":\"user:$ALICE_ID\",\"path\":\"/reports/q3.txt\"}" | j value.url)"
chk "but still cannot list the drive"     "422"     "$(code "$BOB" "{\"op\":\"ls\",\"drive\":\"user:$ALICE_ID\"}")"
chk "unshare revokes it"                  "true"    "$(op "$ALICE" '{"op":"unshare","drive":"user:me","path":"/reports/q3.txt","user":"bob@drive.test"}' | j value.existed)"
chk "and the download stops working"      "422"     "$(code "$BOB" "{\"op\":\"download\",\"drive\":\"user:$ALICE_ID\",\"path\":\"/reports/q3.txt\"}")"

echo "-- quotas"
chk "admin sets a small drive quota"      "20"      "$(adminop "{\"op\":\"policy-set\",\"target\":\"user:$ALICE_ID\",\"max_storage_bytes\":20}" | j value.applied.max_storage_bytes)"
chk "the limit is reported to the user"   "20"      "$(op "$ALICE" '{"op":"quota","drive":"user:me"}' | j value.limits.max_storage_bytes)"
chk "and names the rule that bound it"    "user"    "$(op "$ALICE" '{"op":"quota","drive":"user:me"}' | j value.limits.source.max_storage)"
chk "free bytes account for usage"        "9"       "$(op "$ALICE" '{"op":"quota","drive":"user:me"}' | j value.usage.free_bytes)"
chk "an upload over quota is refused"     "413"     "$(upcode "$ALICE" user:me / big.txt '0123456789012345')"
chk "and nothing was charged for it"      "11"      "$(op "$ALICE" '{"op":"quota","drive":"user:me"}' | j value.usage.used_bytes)"
chk "nor did it leave a phantom entry"    "1"       "$(op "$ALICE" '{"op":"ls","drive":"user:me"}' | j value.count)"
chk "a per-upload cap is separate"        "5"       "$(adminop "{\"op\":\"policy-set\",\"target\":\"user:$ALICE_ID\",\"max_upload_bytes\":5,\"max_storage_bytes\":100000}" | j value.applied.max_upload_bytes)"
chk "an oversized single file is refused" "413"     "$(upcode "$ALICE" user:me / med.txt '0123456789')"
chk "a file within it is accepted"        "201"     "$(upcode "$ALICE" user:me / tiny.txt 'abc')"
chk "a non-admin cannot set policy"       "422"     "$(code "$ALICE" '{"op":"policy-set","target":"global","max_storage_bytes":1}')"

echo "-- deletion returns the bytes"
chk "rm removes the file"                 "/reports/q3.txt" "$(op "$ALICE" '{"op":"rm","drive":"user:me","path":"/reports/q3.txt"}' | j value.removed)"
chk "usage drops by its size"             "3"       "$(op "$ALICE" '{"op":"quota","drive":"user:me"}' | j value.usage.used_bytes)"
chk "the name is reusable afterwards"     "201"     "$(upcode "$ALICE" user:me /reports q3.txt 'ab')"
chkin "a non-empty folder is protected"   "not empty" "$(op "$ALICE" '{"op":"rm","drive":"user:me","path":"/reports"}')"

echo "-- moving"
chk "mv renames within a drive"           "/reports/q4.txt" "$(op "$ALICE" '{"op":"mv","drive":"user:me","path":"/reports/q3.txt","to_name":"q4.txt"}' | j value.to)"
chk "the old name is free again"          "201"     "$(upcode "$ALICE" user:me /reports q3.txt 'cd')"
chkin "a folder cannot move into itself"  "inside itself" "$(op "$ALICE" '{"op":"mv","drive":"user:me","path":"/reports","to_path":"/reports/deep"}')"

echo "-- group drives"
GID=$(op "$ALICE" '{"op":"group-create","name":"eng","org":"acme"}' | j value.group.id)
chk "an org admin creates a group"        "1"       "$([ -n "$GID" ] && echo 1 || echo 0)"
chk "the creator can write to it"         "201"     "$(upcode "$ALICE" "group:$GID" / spec.txt 'group file')"
chk "a non-member cannot read it"         "422"     "$(code "$BOB" "{\"op\":\"ls\",\"drive\":\"group:$GID\"}")"
chk "adding bob as reader lets him list"  "reader"  "$(op "$ALICE" "{\"op\":\"group-add\",\"group\":\"$GID\",\"user\":\"bob@drive.test\",\"role\":\"reader\"}" | j value.role)"
chk "bob now sees the group file"         "spec.txt" "$(op "$BOB" "{\"op\":\"ls\",\"drive\":\"group:$GID\"}" | j value.entries.0.name)"
chk "but a reader cannot write"           "403"     "$(upcode "$BOB" "group:$GID" / bobs.txt 'nope')"
chk "promoting him to member allows it"   "member"  "$(op "$ALICE" "{\"op\":\"group-add\",\"group\":\"$GID\",\"user\":\"bob@drive.test\",\"role\":\"member\"}" | j value.role)"
chk "and now he can write"                "201"     "$(upcode "$BOB" "group:$GID" / bobs.txt 'yes')"
chk "groups lists his membership"         "eng"     "$(op "$BOB" '{"op":"groups"}' | j value.groups.0.name)"
chk "removing him revokes access"         "true"    "$(op "$ALICE" "{\"op\":\"group-remove\",\"group\":\"$GID\",\"user\":\"bob@drive.test\"}" | j value.removed)"
chk "and he can no longer list"           "422"     "$(code "$BOB" "{\"op\":\"ls\",\"drive\":\"group:$GID\"}")"

echo "-- org drives"
chk "an org admin writes to the org drive" "201"    "$(upcode "$ALICE" org:acme / shared.txt 'org file')"
chk "an org member can read it"           "shared.txt" "$(op "$BOB" '{"op":"ls","drive":"org:acme"}' | j value.entries.0.name)"
chk "but a plain member cannot write"     "403"     "$(upcode "$BOB" org:acme / member.txt 'nope')"
chk "group quota falls back to the org"   "org"     "$(adminop '{"op":"policy-set","target":"org:acme","max_storage_bytes":50000}' >/dev/null; op "$ALICE" "{\"op\":\"quota\",\"drive\":\"group:$GID\"}" | j value.limits.source.max_storage)"

echo "-- concurrent uploads race for the last bytes"
# The quota is reserved with an atomic $inc before the blob is written, so a
# check-then-write race cannot let two uploads jointly exceed the limit. Ten at
# once into room for four is the cheapest way to find out if that is true.
RID=$("$BKN" auth user create racer@drive.test --password drive-pw-racer-1 2>/dev/null | j user.id)
RACER=$(login racer@drive.test drive-pw-racer-1)
adminop "{\"op\":\"policy-set\",\"target\":\"user:$RID\",\"max_storage_bytes\":40,\"max_upload_bytes\":1000}" >/dev/null
RACERS=""
for i in $(seq 1 10); do
  ( upcode "$RACER" user:me / "r$i.txt" '0123456789' > "$WORK/race.$i" ) &
  RACERS="$RACERS $!"
done
# Wait for the racers ONLY: a bare `wait` also waits on the bkn server started
# earlier in this shell, which never exits, so the suite hangs forever.
for pid in $RACERS; do wait "$pid"; done
WON=$(grep -l '^201$' "$WORK"/race.* 2>/dev/null | wc -l)
OVER=$(grep -l '^413$' "$WORK"/race.* 2>/dev/null | wc -l)
chk "exactly four 10-byte files fit in 40"  "4"  "$WON"
chk "the other six are refused"             "6"  "$OVER"
chk "usage stops exactly at the limit"      "40" "$(op "$RACER" '{"op":"quota","drive":"user:me"}' | j value.usage.used_bytes)"
chk "and the drive holds only what it charged for" "4" "$(op "$RACER" '{"op":"ls","drive":"user:me"}' | j value.count)"

echo
echo "[$PASS passed, $FAIL failed]"
[ "$FAIL" -eq 0 ]
