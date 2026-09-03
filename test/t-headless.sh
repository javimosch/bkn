source "$SP/dog.sh"
RUN=$(date +%s)
echo "=== domain: headless CMS ==="
C="$B/v1/hooks/cms"
RW=$(sed -n '1p' "$SP/cmstok.txt"); RO=$(sed -n '2p' "$SP/cmstok.txt")
w() { curl -s -X "$1" -H "X-Api-Token: $RW" -H 'Content-Type: application/json' "$C?$2" -d "$3"; }
r() { curl -s -H "X-Api-Token: $RO" "$C?$1"; }
S1="engines-$RUN"; S2="second-$RUN"; S3="archive-$RUN"
chk "no token"                "$(curl -s "$C?model=articles" | python3 -c 'import json,sys;print(json.load(sys.stdin)["error"])')" "invalid or missing X-Api-Token"
chk "read token cannot write" "$(curl -s -X POST -H "X-Api-Token: $RO" "$C?model=articles" -d '{"title":"x","slug":"x"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["error"])')" "token cannot write articles"
AU=$(w POST "model=authors" "{\"name\":\"Ada Lovelace\",\"email\":\"ada-$RUN@cms.dog\"}" | python3 -c 'import json,sys;print(json.load(sys.stdin)["item"]["id"])')
w POST "model=articles" "{\"title\":\"On Engines\",\"slug\":\"$S1\",\"status\":\"live\",\"read_minutes\":7,\"published_at\":\"2026-01-10T00:00:00Z\",\"author\":\"$AU\"}" >/dev/null
w POST "model=articles" "{\"title\":\"Second Piece\",\"slug\":\"$S2\",\"read_minutes\":2,\"published_at\":\"2026-03-01T00:00:00Z\"}" >/dev/null
w POST "model=articles" "{\"title\":\"Old Archive\",\"slug\":\"$S3\",\"status\":\"archived\",\"read_minutes\":30,\"published_at\":\"2025-06-01T00:00:00Z\"}" >/dev/null
chk "default applied"        "$(r "model=articles&slug=$S2" | python3 -c 'import json,sys;print(json.load(sys.stdin)["items"][0]["status"])')" "draft"
chk "minLength enforced"     "$(w POST "model=articles" '{"title":"ab","slug":"ab"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["field"])')" "title"
chk "enum enforced"          "$(w POST "model=articles" "{\"title\":\"Fine title\",\"slug\":\"fine-$RUN\",\"status\":\"pending\"}" | python3 -c 'import json,sys;print(json.load(sys.stdin)["field"])')" "status"
chk "regex enforced"         "$(w POST "model=articles" '{"title":"Fine title","slug":"Not A Slug"}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["field"])')" "slug"
chk "unique enforced"        "$(w POST "model=articles" "{\"title\":\"Copy cat\",\"slug\":\"$S1\"}" | python3 -c 'import json,sys;print(json.load(sys.stdin)["error"])')" "must be unique"
chk "filter + sort"          "$(r "model=articles&read_minutes=gt:5&order_by=read_minutes&order=asc&limit=200" | python3 -c "
import json,sys;print(','.join(i['slug'] for i in json.load(sys.stdin)['items'] if i['slug'].endswith('$RUN')))")" "$S1,$S3"
chk "in filter finds this run" "$(r "model=articles&status=in:live,archived&limit=200" | python3 -c "
import json,sys;print(len([i for i in json.load(sys.stdin)['items'] if i['slug'].endswith('$RUN')]))")" "2"
chk "pagination total"       "$(r "model=articles&limit=1" | python3 -c 'import json,sys;d=json.load(sys.stdin);print(str(len(d["items"]))+"/"+str(d["total"]>=3))')" "1/True"
chk "populate resolves ref"  "$(r "model=articles&slug=$S1&populate=author" | python3 -c 'import json,sys;a=json.load(sys.stdin)["items"][0]["author"];print(a["name"] if isinstance(a,dict) else "unresolved")')" "Ada Lovelace"
ID=$(r "model=articles&slug=$S2" | python3 -c 'import json,sys;print(json.load(sys.stdin)["items"][0]["id"])')
chk "PATCH is partial"       "$(w PATCH "model=articles&id=$ID" '{"status":"live"}' | python3 -c 'import json,sys;i=json.load(sys.stdin)["item"];print(i["status"]+"/"+i["title"])')" "live/Second Piece"
chk "DELETE"                 "$(w DELETE "model=articles&id=$ID" | python3 -c 'import json,sys;print("deleted" in json.load(sys.stdin))')" "True"
chk "unique freed on delete" "$(w POST "model=articles" "{\"title\":\"Reuse slug\",\"slug\":\"$S2\"}" | python3 -c 'import json,sys;print("item" in json.load(sys.stdin))')" "True"
echo "   [$PASS passed, $FAIL failed]"
