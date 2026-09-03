source "$SP/dog.sh"
echo "=== auth: full session lifecycle over HTTPS ==="
LOGIN=$(curl -s -X POST "$B/v1/auth/login" -H 'Content-Type: application/json' \
  -d '{"email":"  ADA@Dog.IO ","password":"dogfood-password-1","org":"dogcorp"}')
ACC=$(echo "$LOGIN" | python3 -c 'import json,sys;print(json.load(sys.stdin)["tokens"]["access_token"])')
REF=$(echo "$LOGIN" | python3 -c 'import json,sys;print(json.load(sys.stdin)["tokens"]["refresh_token"])')
chk "login normalizes the email"  "$(echo "$LOGIN" | python3 -c 'import json,sys;print(json.load(sys.stdin)["tokens"]["user"]["email"])')" "ada@dog.io"
chk "token is org-scoped"         "$(echo "$LOGIN" | python3 -c 'import json,sys;t=json.load(sys.stdin)["tokens"];print(t["org"]+"/"+t["org_role"])')" "dogcorp/owner"
chk "no password hash in payload" "$(echo "$LOGIN" | grep -c '\$2a\$')" "0"
chk "me resolves the live user"   "$(curl -s -H "Authorization: Bearer $ACC" "$B/v1/auth/me" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(d["user"]["email"]+"/"+str(len(d["memberships"])))')" "ada@dog.io/1"
chk "me without a token"          "$(curl -s -o /dev/null -w '%{http_code}' "$B/v1/auth/me")" "401"
chk "wrong password"              "$(curl -s -X POST "$B/v1/auth/login" -H 'Content-Type: application/json' -d '{"email":"ada@dog.io","password":"nope"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["error"]["type"])')" "bad_credentials"
chk "unknown user looks the same" "$(curl -s -X POST "$B/v1/auth/login" -H 'Content-Type: application/json' -d '{"email":"ghost@dog.io","password":"nope"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["error"]["type"])')" "bad_credentials"
REF2=$(curl -s -X POST "$B/v1/auth/refresh" -H 'Content-Type: application/json' -d "{\"refresh_token\":\"$REF\"}" | python3 -c 'import json,sys;print(json.load(sys.stdin)["tokens"]["refresh_token"])')
chk "refresh rotates"             "$( [ "$REF2" != "$REF" ] && echo rotated || echo same)" "rotated"
chk "old refresh is dead"         "$(curl -s -X POST "$B/v1/auth/refresh" -H 'Content-Type: application/json' -d "{\"refresh_token\":\"$REF\"}" | python3 -c 'import json,sys;print(json.load(sys.stdin)["error"]["type"])')" "session_invalid"
chk "switch-org needs membership" "$(curl -s -X POST "$B/v1/auth/switch-org" -H 'Content-Type: application/json' -d "{\"refresh_token\":\"$REF2\",\"org\":\"nonexistent\"}" | python3 -c 'import json,sys;print(json.load(sys.stdin)["error"]["type"])')" "not_found"
chk "logout"                      "$(curl -s -X POST "$B/v1/auth/logout" -H 'Content-Type: application/json' -d "{\"refresh_token\":\"$REF2\"}" | python3 -c 'import json,sys;print(json.load(sys.stdin)["logged_out"])')" "True"
chk "logout is idempotent"        "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$B/v1/auth/logout" -H 'Content-Type: application/json' -d "{\"refresh_token\":\"$REF2\"}")" "200"
chk "admin user list gated"       "$(curl -s -o /dev/null -w '%{http_code}' "$B/v1/auth/users")" "403"
chk "admin user list with token"  "$(a "$B/v1/auth/users" | python3 -c 'import json,sys;print(json.load(sys.stdin)["count"]>=2)')" "True"
echo "   [$PASS passed, $FAIL failed]"
