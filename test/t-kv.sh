source "$SP/dog.sh"
echo "=== kv: typed values, encryption, visibility ==="
aj -X POST "$B/v1/kv/dog.name"   -d '{"value":"Ada Corp","type":"string","public":true}' >/dev/null
aj -X POST "$B/v1/kv/dog.limits" -d '{"value":"{\"rpm\":60}","type":"json"}' >/dev/null
aj -X POST "$B/v1/kv/dog.secret" -d '{"value":"sk_live_dogfood","type":"encrypted"}' >/dev/null
chk "public read, no auth"       "$(curl -s "$B/v1/kv/dog.name" | python3 -c 'import json,sys;print(json.load(sys.stdin)["entry"]["value"])')" "Ada Corp"
chk "private is 404 unauthed"    "$(curl -s -o /dev/null -w '%{http_code}' "$B/v1/kv/dog.secret")" "404"
chk "missing is 404 too"         "$(curl -s -o /dev/null -w '%{http_code}' "$B/v1/kv/dog.nothing")" "404"
chk "encrypted round-trips"      "$(a "$B/v1/kv/dog.secret" | python3 -c 'import json,sys;print(json.load(sys.stdin)["entry"]["value"])')" "sk_live_dogfood"
chk "list hides encrypted value" "$(a "$B/v1/kv?prefix=dog." | python3 -c 'import json,sys
e=[x for x in json.load(sys.stdin)["entries"] if x["key"]=="dog.secret"][0]
print("hidden" if e["value"]=="" else "LEAKED")')" "hidden"
chk "invalid json rejected"      "$(aj -X POST "$B/v1/kv/dog.bad" -d '{"value":"{nope}","type":"json"}' | python3 -c 'import json,sys;print(json.load(sys.stdin).get("error",{}).get("type","none"))')" "validation_error"
chk "encrypted cannot be public" "$(aj -X POST "$B/v1/kv/dog.oops" -d '{"value":"x","type":"encrypted","public":true}' | python3 -c 'import json,sys;print(json.load(sys.stdin).get("error",{}).get("type","none"))')" "validation_error"
echo "   [$PASS passed, $FAIL failed]"
