#!/bin/bash
# Script run policy — self-contained.
#
# Unlike the other suites this one starts its own bkn on a random port with
# its own database, because a script can only be created by the CLI on the
# machine that holds the data. Asserting the policy matrix against a
# deployment would mean installing a fixture script there, and a production
# instance is not a place to leave one lying around.
set -u
BKN=${BKN_BIN:-$(cd "$(dirname "$0")/.." && pwd)/bin/bkn}
WORK=$(mktemp -d)
PORT=${PORT:-$((20000 + RANDOM % 20000))}
PASS=0; FAIL=0
cleanup() { [ -n "${PID:-}" ] && kill "$PID" 2>/dev/null; rm -rf "$WORK"; }
trap cleanup EXIT

chk() {
  if [ "$2" = "$3" ]; then PASS=$((PASS+1)); printf "   \033[32mok\033[0m   %-46s %s\n" "$1" "$2"
  else FAIL=$((FAIL+1)); printf "   \033[31mFAIL\033[0m %-46s got %s want %s\n" "$1" "$2" "$3"; fi
}

export BKN_DATA="$WORK/b.db"
export BKN_ADMIN_TOKEN="adm-$RANDOM$RANDOM"
export BKN_ENCRYPTION_KEY=$(printf 'a%.0s' $(seq 32))

cat > "$WORK/s.js" <<'JS'
function main(input) {
  return { ok: true, kind: bkn.caller.kind, sub: bkn.caller.sub, org: bkn.caller.org };
}
JS

"$BKN" auth user create dev@dog.io --password script-access-pw-1 >/dev/null 2>&1
"$BKN" script create closed --file "$WORK/s.js" >/dev/null
"$BKN" script create opened --file "$WORK/s.js" --run-access user >/dev/null
"$BKN" script create shared --file "$WORK/s.js" --run-access public >/dev/null
"$BKN" serve --host 127.0.0.1 --port "$PORT" >"$WORK/srv.log" 2>&1 &
PID=$!
B="http://127.0.0.1:$PORT"
for _ in $(seq 1 50); do curl -sf "$B/_health" >/dev/null && break; sleep 0.1; done

UT=$(curl -s -X POST "$B/v1/auth/login" -H 'Content-Type: application/json' \
  -d '{"email":"dev@dog.io","password":"script-access-pw-1"}' \
  | python3 -c "import json,sys;print(json.load(sys.stdin).get('tokens',{}).get('access_token',''))")

run() { # run <script> [token]
  if [ -n "${2:-}" ]; then
    curl -s -o "$WORK/r.json" -w '%{http_code}' -X POST "$B/v1/script/$1/run" \
      -H "Authorization: Bearer $2" -H 'Content-Type: application/json' -d '{}'
  else
    curl -s -o "$WORK/r.json" -w '%{http_code}' -X POST "$B/v1/script/$1/run" \
      -H 'Content-Type: application/json' -d '{}'
  fi
}
val() { python3 -c "import json,sys;print(json.load(open('$WORK/r.json'))['value']['$1'])"; }

echo "=== script run policy — local ==="

# An undeclared script keeps exactly the permissions it had before policies
# existed. This is the assertion that protects every script already deployed.
chk "undeclared refuses anonymous"    "$(run closed)"       "403"
chk "undeclared refuses a user token" "$(run closed "$UT")" "403"
chk "a wrong admin token is refused"   "$(run closed adm-wrong)" "403"
chk "the admin token still runs it"   "$(run closed "$BKN_ADMIN_TOKEN")" "200"

# run-access user: a signed-in caller, and a 401 rather than a 403 for anyone
# who has not signed in, because refreshing a token is what would help.
chk "user-access refuses anonymous"   "$(run opened)"       "401"
chk "user-access allows a user"       "$(run opened "$UT")" "200"
chk "user-access allows the admin"    "$(run opened "$BKN_ADMIN_TOKEN")" "200"

chk "public runs without a token"     "$(run shared)"       "200"
chk "public still runs with one"      "$(run shared "$UT")" "200"

# A script nobody may run must be indistinguishable from one that does not
# exist, or 404s become a way to enumerate what an operator installed.
chk "a missing script hides from anon" "$(run ghost)"       "403"
chk "a missing script hides from user" "$(run ghost "$UT")" "403"
chk "and is a plain 404 for the admin" "$(run ghost "$BKN_ADMIN_TOKEN")" "404"

# The policy says whether you may run it; the script decides what you see,
# which it cannot do without knowing who called.
run opened "$UT" >/dev/null
chk "the script sees a user caller"   "$(val kind)" "user"
chk "and their id"                    "$([ -n "$(val sub)" ] && echo yes || echo no)" "yes"
run shared >/dev/null
chk "an anonymous caller says so"     "$(val kind)" "anon"
chk "and carries no id"               "$([ -z "$(val sub)" ] && echo empty || echo leaked)" "empty"
run opened "$BKN_ADMIN_TOKEN" >/dev/null
chk "the admin token reads as admin"  "$(val kind)" "admin"

# The audience is visible where an operator would look for it.
chk "list shows the audience"         "$(curl -s -H "Authorization: Bearer $BKN_ADMIN_TOKEN" "$B/v1/script" \
  | python3 -c "import json,sys;print(next(s['run_access'] for s in json.load(sys.stdin)['scripts'] if s['name']=='opened'))")" "user"

# Closing it again takes effect on the next call, with no restart.
"$BKN" script update opened --run-access admin >/dev/null
chk "closing it takes effect at once" "$(run opened "$UT")" "403"

echo "   [$PASS passed, $FAIL failed]"
[ "$FAIL" -eq 0 ]
