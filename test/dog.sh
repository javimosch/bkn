#!/bin/bash
# live dogfood harness — every call goes over the public internet to dk1
# The target is parameterizable so the same assertions can be run against a
# different implementation. Only the ADDRESS moves; no assertion changes.
B=${BKN_TEST_URL:-https://bkn.intrane.fr}
ADMIN=$(cat "$SP/admin.tok")
PASS=0; FAIL=0
a()  { curl -s -H "Authorization: Bearer $ADMIN" "$@"; }
aj() { curl -s -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' "$@"; }
code(){ curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $ADMIN" "$@"; }
chk() { # chk "<label>" "<got>" "<want>"
  if [ "$2" = "$3" ]; then PASS=$((PASS+1)); printf "   \033[32mok\033[0m   %-46s %s\n" "$1" "$2"
  else FAIL=$((FAIL+1)); printf "   \033[31mFAIL\033[0m %-46s got %s want %s\n" "$1" "$2" "$3"; fi
}
chkc(){ if echo "$2" | grep -q "$3"; then PASS=$((PASS+1)); printf "   \033[32mok\033[0m   %-46s\n" "$1"
  else FAIL=$((FAIL+1)); printf "   \033[31mFAIL\033[0m %-46s got: %s\n" "$1" "$(echo "$2"|head -c 90)"; fi
}
