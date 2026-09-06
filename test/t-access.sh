source "$SP/dog.sh"
echo "=== access: collection policies, scoped audiences, self-service (live over HTTPS) ==="
# Re-runnable: every identity and document this suite creates is scoped to one
# run, so a second run cannot trip over the first one's leftovers.
RUN=$(date +%s)
NS="acc$RUN"
py() { python3 -c "import json,sys;d=json.load(sys.stdin);print($1)"; }
tok() { python3 -c "import json,sys;print(json.load(sys.stdin).get('tokens',{}).get('access_token',''))"; }
u() { curl -s -H "Authorization: Bearer $1" -H 'Content-Type: application/json' "${@:2}"; }
ucode() { curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $1" "${@:2}"; }

# --- collections are declared over HTTP, which is how a deployment behind a
#     reverse proxy gets a policy at all.
DECL=$(aj -X PUT "$B/v1/store/$NS/notes" -d '{"access":{"owner_field":"user_id","rules":{"read":"owner","create":"owner","update":"owner","delete":"owner"}}}')
chk "declare an owner-scoped collection" "$(echo "$DECL" | py "d['collection']['access']['rules']['read']")" "owner"
aj -X PUT "$B/v1/store/$NS/docs"  -d '{"access":{"org_field":"org_id","rules":{"read":"org","create":"org"}}}' >/dev/null
aj -X PUT "$B/v1/store/$NS/pages" -d '{"access":{"rules":{"read":"public"}}}' >/dev/null
chk "a policy that enforces nothing is refused" "$(aj -X PUT "$B/v1/store/$NS/bad" -d '{"access":{"rules":{"read":"owner"}}}' | py "d['error']['type']")" "validation_error"
chk "an unknown audience is refused"            "$(aj -X PUT "$B/v1/store/$NS/bad" -d '{"access":{"rules":{"read":"everyone"}}}' | py "d['error']['type']")" "validation_error"

# --- two identities in one organization
ORG="org$RUN"
aj -X POST "$B/v1/auth/users" -d "{\"email\":\"ada-$RUN@dog.io\",\"password\":\"access-suite-pw-1\"}" >/dev/null
aj -X POST "$B/v1/auth/users" -d "{\"email\":\"bob-$RUN@dog.io\",\"password\":\"access-suite-pw-2\"}" >/dev/null
ADA=$(curl -s -X POST "$B/v1/auth/login" -H 'Content-Type: application/json' -d "{\"email\":\"ada-$RUN@dog.io\",\"password\":\"access-suite-pw-1\"}" | tok)
BOB=$(curl -s -X POST "$B/v1/auth/login" -H 'Content-Type: application/json' -d "{\"email\":\"bob-$RUN@dog.io\",\"password\":\"access-suite-pw-2\"}" | tok)
chk "both users signed in" "$( [ -n "$ADA" ] && [ -n "$BOB" ] && echo yes || echo no)" "yes"

# --- the tenancy field comes from the token, not from the body
ADADOC=$(u "$ADA" -X POST "$B/v1/store/$NS/notes" -d '{"title":"ada note","user_id":"SPOOFED"}')
chk "a create stamps its own owner" "$(echo "$ADADOC" | py "'SPOOFED' not in json.dumps(d['record'])")" "True"
AID=$(echo "$ADADOC" | py "d['record']['id']")
u "$BOB" -X POST "$B/v1/store/$NS/notes" -d '{"title":"bob note"}' >/dev/null

chk "a list shows only your own"   "$(u "$ADA" "$B/v1/store/$NS/notes" | py "str(d['total'])+','+d['records'][0]['title']")" "1,ada note"
chk "so does a rollup"             "$(u "$BOB" "$B/v1/store/$NS/notes?by=title" | py "str(d['total'])+','+d['buckets'][0]['key']")" "1,bob note"
chk "an admin still sees them all" "$(a "$B/v1/store/$NS/notes" | py "d['total']")" "2"

# --- the same document, asked for six different ways by somebody else
chk "cross-tenant get"    "$(ucode "$BOB" "$B/v1/store/$NS/notes/$AID")" "404"
chk "cross-tenant patch"  "$(ucode "$BOB" -X PATCH -H 'Content-Type: application/json' -d '{"title":"pwned"}' "$B/v1/store/$NS/notes/$AID")" "404"
chk "cross-tenant delete" "$(ucode "$BOB" -X DELETE "$B/v1/store/$NS/notes/$AID")" "404"
# A create policy stamps the caller's own id, so an upsert at somebody else's
# id would otherwise replace their document and relabel it as yours.
chk "cross-tenant upsert" "$(ucode "$BOB" -X POST -H 'Content-Type: application/json' -d '{"title":"stolen"}' "$B/v1/store/$NS/notes?id=$AID")" "404"
chk "a filter cannot widen the scope" "$(u "$BOB" "$B/v1/store/$NS/notes?title=ada%20note" | py "d['total']")" "0"
chk "the document is untouched"       "$(a "$B/v1/store/$NS/notes/$AID" | py "d['record']['title']")" "ada note"
BID=$(u "$BOB" "$B/v1/store/$NS/notes" | py "d['records'][0]['id']")
chk "you cannot hand away your own document" "$(u "$BOB" -X PATCH "$B/v1/store/$NS/notes/$BID" -d '{"user_id":"someone-else"}' | py "d['error']['type']")" "forbidden"
chk "your own patch still works"             "$(u "$BOB" -X PATCH "$B/v1/store/$NS/notes/$BID" -d '{"title":"edited"}' | py "d['record']['title']")" "edited"
chk "your own delete still works"            "$(ucode "$BOB" -X DELETE "$B/v1/store/$NS/notes/$BID")" "200"

# --- org scope: shared between members, refused to a token that has no org
aj -X POST "$B/v1/auth/orgs" -d "{\"slug\":\"$ORG\",\"name\":\"Access $RUN\"}" >/dev/null
aj -X POST "$B/v1/auth/orgs/$ORG/members" -d "{\"user\":\"ada-$RUN@dog.io\",\"role\":\"owner\"}"  >/dev/null
aj -X POST "$B/v1/auth/orgs/$ORG/members" -d "{\"user\":\"bob-$RUN@dog.io\",\"role\":\"member\"}" >/dev/null
ADAO=$(curl -s -X POST "$B/v1/auth/login" -H 'Content-Type: application/json' -d "{\"email\":\"ada-$RUN@dog.io\",\"password\":\"access-suite-pw-1\",\"org\":\"$ORG\"}" | tok)
BOBO=$(curl -s -X POST "$B/v1/auth/login" -H 'Content-Type: application/json' -d "{\"email\":\"bob-$RUN@dog.io\",\"password\":\"access-suite-pw-2\",\"org\":\"$ORG\"}" | tok)
u "$ADAO" -X POST "$B/v1/store/$NS/docs" -d '{"title":"org doc"}' >/dev/null
chk "an org member sees a colleague's document" "$(u "$BOBO" "$B/v1/store/$NS/docs" | py "str(d['total'])+','+d['records'][0]['title']")" "1,org doc"
# An orgless token must be refused rather than read as unscoped: falling through
# would hand every tenant's documents to somebody who forgot to pick one.
chk "an orgless token is refused, not widened" "$(ucode "$BOB" "$B/v1/store/$NS/docs")" "403"

# --- public reads, and the fact that opening one verb opens only that verb
a -X POST "$B/v1/store/$NS/pages" -H 'Content-Type: application/json' -d '{"slug":"home"}' >/dev/null
chk "anonymous read of a public collection" "$(curl -s -o /dev/null -w '%{http_code}' "$B/v1/store/$NS/pages")" "200"
chk "read=public does not open writes"      "$(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"slug":"evil"}' "$B/v1/store/$NS/pages")" "403"
chk "nor for a signed-in user"              "$(ucode "$ADA" -X POST -H 'Content-Type: application/json' -d '{"slug":"evil"}' "$B/v1/store/$NS/pages")" "403"

# --- a collection nobody declared a policy for behaves exactly as it always did
a -X POST "$B/v1/store/$NS/private" -H 'Content-Type: application/json' -d '{"secret":1}' >/dev/null
chk "undeclared: anonymous"    "$(curl -s -o /dev/null -w '%{http_code}' "$B/v1/store/$NS/private")" "403"
chk "undeclared: signed in"    "$(ucode "$ADA" "$B/v1/store/$NS/private")" "403"
chk "undeclared: admin"        "$(ucode "$ADMIN" "$B/v1/store/$NS/private")" "200"
# 401 says a credential would help and 403 says it would not, so a client knows
# whether to refresh or to give up.
chk "401 when a token would help" "$(curl -s -o /dev/null -w '%{http_code}' "$B/v1/store/$NS/notes")" "401"

# --- self-service password change
chk "wrong current password"  "$(ucode "$BOB" -X POST -H 'Content-Type: application/json' -d '{"current_password":"nope","new_password":"access-suite-pw-9"}' "$B/v1/auth/password")" "401"
chk "changing your own password" "$(u "$BOB" -X POST "$B/v1/auth/password" -d '{"current_password":"access-suite-pw-2","new_password":"access-suite-pw-9"}' | py "d['changed']")" "True"
chk "the old password stops working" "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$B/v1/auth/login" -H 'Content-Type: application/json' -d "{\"email\":\"bob-$RUN@dog.io\",\"password\":\"access-suite-pw-2\"}")" "401"
chk "the new one works"              "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$B/v1/auth/login" -H 'Content-Type: application/json' -d "{\"email\":\"bob-$RUN@dog.io\",\"password\":\"access-suite-pw-9\"}")" "200"
chk "anonymous password change"      "$(curl -s -o /dev/null -w '%{http_code}' -X POST "$B/v1/auth/password" -H 'Content-Type: application/json' -d '{"current_password":"x","new_password":"y"}')" "401"

echo "   [$PASS passed, $FAIL failed]"
