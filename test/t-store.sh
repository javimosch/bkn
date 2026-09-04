source "$SP/dog.sh"
echo "=== store: the six verbs + the query surface (live over HTTPS) ==="
a -X DELETE "$B/v1/store/dog/products/x" >/dev/null 2>&1
for r in '{"name":"widget","price":30,"stock":5,"status":"live"}' \
         '{"name":"gizmo","price":10,"stock":0,"status":"draft"}' \
         '{"name":"doohickey","price":50,"stock":2,"status":"live"}' \
         '{"name":"thing","price":20,"status":"archived"}'; do
  n=$(echo "$r" | python3 -c "import json,sys;print(json.load(sys.stdin)['name'])")
  aj -X POST "$B/v1/store/dog/products?id=$n" -d "$r" >/dev/null
done
chk "put x4"                "$(a "$B/v1/store/dog/products" | python3 -c 'import json,sys;print(json.load(sys.stdin)["total"])')" "4"
chk "get by id"             "$(a "$B/v1/store/dog/products/widget" | python3 -c 'import json,sys;print(json.load(sys.stdin)["record"]["price"])')" "30"
chk "filter eq"             "$(a "$B/v1/store/dog/products?status=live" | python3 -c 'import json,sys;print(json.load(sys.stdin)["total"])')" "2"
chk "filter gt"             "$(a "$B/v1/store/dog/products?price=gt:20" | python3 -c 'import json,sys;print(json.load(sys.stdin)["total"])')" "2"
chk "filter lte"            "$(a "$B/v1/store/dog/products?price=lte:20" | python3 -c 'import json,sys;print(json.load(sys.stdin)["total"])')" "2"
chk "filter ne"             "$(a "$B/v1/store/dog/products?status=ne:draft" | python3 -c 'import json,sys;print(json.load(sys.stdin)["total"])')" "3"
chk "filter in"             "$(a "$B/v1/store/dog/products?status=in:live,archived" | python3 -c 'import json,sys;print(json.load(sys.stdin)["total"])')" "3"
chk "filter by id"          "$(a "$B/v1/store/dog/products?id=in:widget,gizmo" | python3 -c 'import json,sys;print(json.load(sys.stdin)["total"])')" "2"
chk "order_by asc"          "$(a "$B/v1/store/dog/products?order_by=price&order=asc" | python3 -c 'import json,sys;print(json.load(sys.stdin)["records"][0]["name"])')" "gizmo"
chk "order_by desc"         "$(a "$B/v1/store/dog/products?order_by=price" | python3 -c 'import json,sys;print(json.load(sys.stdin)["records"][0]["name"])')" "doohickey"
chk "missing field sorts last" "$(a "$B/v1/store/dog/products?order_by=stock" | python3 -c 'import json,sys;print(json.load(sys.stdin)["records"][-1]["name"])')" "thing"
chk "total vs page"         "$(a "$B/v1/store/dog/products?limit=2" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(str(d["count"])+"/"+str(d["total"]))')" "2/4"
chk "patch merges"          "$(aj -X PATCH "$B/v1/store/dog/products/widget" -d '{"stock":9}' | python3 -c 'import json,sys;r=json.load(sys.stdin)["record"];print(str(r["stock"])+","+r["name"])')" "9,widget"

# Atomic operators and preconditions: the write is computed from the current
# value under compare-and-set, so concurrent callers cannot lose each other.
py() { python3 -c "import json,sys;r=json.load(sys.stdin).get('record',{});print($1)"; }
aj -X PATCH "$B/v1/store/dog/products/widget" -d '{"stock":{"$inc":3}}'      >/dev/null
chk "\$inc accumulates"     "$(aj -X PATCH "$B/v1/store/dog/products/widget" -d '{"stock":{"$inc":3}}' | py "r['stock']")" "15"
chk "\$append concatenates" "$(aj -X PATCH "$B/v1/store/dog/products/widget" -d '{"note":{"$append":"ab"}}' >/dev/null; aj -X PATCH "$B/v1/store/dog/products/widget" -d '{"note":{"$append":"cd"}}' | py "r['note']")" "abcd"
chk "\$push appends"        "$(aj -X PATCH "$B/v1/store/dog/products/widget" -d '{"tags":{"$push":"x"}}' >/dev/null; aj -X PATCH "$B/v1/store/dog/products/widget" -d '{"tags":{"$push":"y"}}' | py "','.join(r['tags'])")" "x,y"
chk "\$setIfEmpty sticks"   "$(aj -X PATCH "$B/v1/store/dog/products/widget" -d '{"owner":{"$setIfEmpty":"first"}}' >/dev/null; aj -X PATCH "$B/v1/store/dog/products/widget" -d '{"owner":{"$setIfEmpty":"second"}}' | py "r['owner']")" "first"
chk "plain object is a value" "$(aj -X PATCH "$B/v1/store/dog/products/widget" -d '{"meta":{"a":"b"}}' | py "r['meta']['a']")" "b"
chk "unknown operator -> 400" "$(code -X PATCH "$B/v1/store/dog/products/widget" -d '{"stock":{"$nope":1}}' -H 'Content-Type: application/json')" "400"
chk "bad operand -> 400"      "$(code -X PATCH "$B/v1/store/dog/products/widget" -d '{"name":{"$inc":1}}' -H 'Content-Type: application/json')" "400"
chk "precondition holds"      "$(aj -X PATCH "$B/v1/store/dog/products/widget?if=status=live" -d '{"status":"sold"}' | py "r['status']")" "sold"
chk "stale precondition -> 409" "$(code -X PATCH "$B/v1/store/dog/products/widget?if=status=live" -d '{"status":"back"}' -H 'Content-Type: application/json')" "409"
chk "precondition wrote nothing" "$(a "$B/v1/store/dog/products/widget" | py "r['status']")" "sold"
chk "if-absent claims once"   "$(aj -X PATCH "$B/v1/store/dog/products/gizmo?if-absent=holder" -d '{"holder":"a"}' | py "r['holder']")" "a"
chk "second claim -> 409"     "$(code -X PATCH "$B/v1/store/dog/products/gizmo?if-absent=holder" -d '{"holder":"b"}' -H 'Content-Type: application/json')" "409"

chk "delete"                "$(code -X DELETE "$B/v1/store/dog/products/thing")" "200"
chk "delete missing -> 404" "$(code -X DELETE "$B/v1/store/dog/products/thing")" "404"
chk "after delete"          "$(a "$B/v1/store/dog/products" | python3 -c 'import json,sys;print(json.load(sys.stdin)["total"])')" "3"
echo "   [$PASS passed, $FAIL failed]"
