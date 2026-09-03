source "$SP/dog.sh"
echo "=== domains: forms + waiting-list exports ==="
F="$B/v1/hooks/forms"; X="$B/v1/hooks/exports"
chk "GET returns the definition"  "$(curl -s "$F?form=dogcontact" | python3 -c 'import json,sys;print(",".join(f["name"] for f in json.load(sys.stdin)["fields"]))')" "email,message,topic"
chk "valid submission"            "$(curl -s -X POST "$F" -d '{"form":"dogcontact","fields":{"email":"Ada@Dog.IO","message":"hello from the live test","topic":"sales"}}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["ok"])')" "True"
chk "required enforced"           "$(curl -s -X POST "$F" -d '{"form":"dogcontact","fields":{"message":"no email here"}}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["field"])')" "email"
chk "enum enforced"               "$(curl -s -X POST "$F" -d '{"form":"dogcontact","fields":{"email":"a@b.io","message":"xx","topic":"nope"}}' | python3 -c 'import json,sys;print(json.load(sys.stdin)["field"])')" "topic"
chk "honeypot: looks like success" "$(curl -s -X POST "$F" -d '{"form":"dogcontact","fields":{"email":"bot@spam.io","message":"buy","website":"http://spam"}}' | python3 -c 'import json,sys;d=json.load(sys.stdin);print(str(d["ok"])+"/"+str("id" in d))')" "True/False"
curl -s -X POST "$F" -d '{"form":"dogwait","fields":{"email":" Ada@Dog.IO ","source":"launch"}}' >/dev/null
chk "dedupe on second submit"     "$(curl -s -X POST "$F" -d '{"form":"dogwait","fields":{"email":"ada@dog.io"}}' | python3 -c 'import json,sys;print(json.load(sys.stdin).get("duplicate"))')" "True"
curl -s -X POST "$F" -d '{"form":"dogwait","fields":{"email":"tricky@dog.io","source":"a,comma \"and\" quote"}}' >/dev/null
chk "export needs the password"   "$(curl -s "$X?name=dogwait" | python3 -c 'import json,sys;print(json.load(sys.stdin)["error"])')" "invalid or missing password"
chk "export content-type"         "$(curl -s -D- -o /dev/null "$X?name=dogwait&password=dog-export-secret" | grep -i '^content-type' | tr -d '\r' | cut -d' ' -f2)" "text/csv;"
chk "export is real CSV"          "$(curl -s "$X?name=dogwait&password=dog-export-secret" -o /tmp/dog.csv; python3 -c '
import csv;rows=list(csv.DictReader(open("/tmp/dog.csv",newline="")));print(len(rows))')" "2"
chk "CSV quoting survives"        "$(python3 -c '
import csv
for r in csv.DictReader(open("/tmp/dog.csv",newline="")):
    if "tricky" in r["email"]: print(r["source"])')" 'a,comma "and" quote'
chk "bearer auth also works"      "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer dog-export-secret" "$X?name=dogwait")" "200"
echo "   [$PASS passed, $FAIL failed]"
