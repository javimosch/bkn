source "$SP/dog.sh"
RUN=$(date +%s)
echo "=== domain: Stripe webhook ==="
sign() { python3 "$SP/sign.py" "$@"; }
post() { curl -s -X POST "$B/v1/hooks/stripe" -H "Stripe-Signature: $2" -H 'Content-Type: application/json' -d "$1"; }
CO='{"id":"dog_evt_'"$RUN"'_1","type":"checkout.session.completed","data":{"object":{"id":"cs_1","customer":"cus_dog","customer_details":{"email":"buyer@dog.io"},"metadata":{"plan":"pro"}}}}'
chk "valid signature accepted" "$(post "$CO" "$(sign "$CO")" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("type","?"))')" "checkout.session.completed"
chk "retry is idempotent"      "$(post "$CO" "$(sign "$CO")" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("duplicate"))')" "True"
chk "forged signature"         "$(post "$CO" "t=$(date +%s),v1=deadbeef" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("error","?"))')" "signature mismatch"
chk "replayed timestamp"       "$(post "$CO" "$(sign "$CO" 1700000000)" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("error","?"))')" "timestamp outside the tolerance window"
SUB='{"id":"dog_evt_'"$RUN"'_2","type":"customer.subscription.updated","data":{"object":{"id":"sub_1","customer":"cus_dog","status":"past_due","cancel_at_period_end":true}}}'
chk "subscription.updated"     "$(post "$SUB" "$(sign "$SUB")" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("ok"))')" "True"
UNK='{"id":"dog_evt_'"$RUN"'_3","type":"radar.warning","data":{"object":{}}}'
chk "unhandled type ignored"   "$(post "$UNK" "$(sign "$UNK")" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("ignored","?"))')" "radar.warning"
chk "billing NOT on the user"  "$(a "$B/v1/store/billing/subjects/cus_dog" | python3 -c 'import json,sys;r=json.load(sys.stdin)["record"];print(r["plan"]+"/"+r["status"])')" "pro/past_due"
chk "user linked by email"     "$(a "$B/v1/store/billing/subjects/cus_dog" | python3 -c 'import json,sys;print(json.load(sys.stdin)["record"]["email"])')" "buyer@dog.io"
chk "user record has no plan"  "$(a "$B/v1/auth/users" | python3 -c 'import json,sys;print("plan" in json.dumps(json.load(sys.stdin)["users"]))')" "False"
chk "event ledger grew"        "$(a "$B/v1/store/stripe/events" | python3 -c 'import json,sys;print(json.load(sys.stdin)["total"]>=4)')" "True"
echo "   [$PASS passed, $FAIL failed]"
