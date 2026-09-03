source "$SP/dog.sh"
echo "=== files: namespaces, dedup, and the XSS posture ==="
PNG=$(printf '\x89PNG\r\n\x1a\n\x00\x01\x02\xff\xfe')
printf '\x89PNG\r\n\x1a\n\x00\x01\x02\xff\xfe' > /tmp/dog.png
curl -s -H "Authorization: Bearer $ADMIN" -H 'Content-Type: image/png' -X POST "$B/v1/files/dogpub/a.png" --data-binary @/tmp/dog.png >/dev/null
curl -s -H "Authorization: Bearer $ADMIN" -H 'Content-Type: image/png' -X POST "$B/v1/files/dogpub/b.png" --data-binary @/tmp/dog.png >/dev/null
chk "public serves without auth"   "$(curl -s -o /dev/null -w '%{http_code}' "$B/v1/files/dogpub/a.png")" "200"
chk "binary survives the round trip" "$(curl -s "$B/v1/files/dogpub/a.png" | sha256sum | cut -c1-12)" "$(sha256sum /tmp/dog.png | cut -c1-12)"
chk "dedup: same sha for both"     "$(a "$B/v1/files/dogpub" | python3 -c 'import json,sys
f=json.load(sys.stdin)["files"];print("same" if f[0]["sha256"]==f[1]["sha256"] else "differ")')" "same"
chk "image is inline"              "$(curl -s -D- -o /dev/null "$B/v1/files/dogpub/a.png" | grep -i content-disposition | grep -o 'inline\|attachment')" "inline"
chk "nosniff present"              "$(curl -s -D- -o /dev/null "$B/v1/files/dogpub/a.png" | grep -ci 'x-content-type-options')" "1"
chk "CSP present"                  "$(curl -s -D- -o /dev/null "$B/v1/files/dogpub/a.png" | grep -ci 'content-security-policy')" "1"
ET=$(curl -s -D- -o /dev/null "$B/v1/files/dogpub/a.png" | grep -i '^etag' | tr -d '\r' | cut -d' ' -f2)
chk "ETag gives a 304"             "$(curl -s -o /dev/null -w '%{http_code}' -H "If-None-Match: $ET" "$B/v1/files/dogpub/a.png")" "304"
chk "type allow-list enforced"     "$(curl -s -H "Authorization: Bearer $ADMIN" -H 'Content-Type: text/plain' -X POST "$B/v1/files/dogpub/x.txt" -d 'hi' | python3 -c 'import json,sys;print(json.load(sys.stdin)["error"]["type"])')" "type_refused"
curl -s -H "Authorization: Bearer $ADMIN" -H 'Content-Type: text/html' -X POST "$B/v1/files/dogpriv/x.html" -d '<script>alert(1)</script>' >/dev/null
chk "private ns is 404 unauthed"   "$(curl -s -o /dev/null -w '%{http_code}' "$B/v1/files/dogpriv/x.html")" "404"
chk "html is an attachment"        "$(curl -s -D- -o /dev/null -H "Authorization: Bearer $ADMIN" "$B/v1/files/dogpriv/x.html" | grep -i content-disposition | grep -o 'inline\|attachment')" "attachment"
chk "delete releases one name"     "$(code -X DELETE "$B/v1/files/dogpub/a.png")" "200"
chk "the shared blob survives"     "$(curl -s "$B/v1/files/dogpub/b.png" | sha256sum | cut -c1-12)" "$(sha256sum /tmp/dog.png | cut -c1-12)"
echo "   [$PASS passed, $FAIL failed]"
