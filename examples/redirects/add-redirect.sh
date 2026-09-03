#!/bin/sh
# Adding a rule is a store write; the id must be the hash of the normalised
# path so the resolver can find it in one lookup.
#   ./add-redirect.sh /old-page /new-page 301
set -eu
from=$(printf '%s' "$1" | tr 'A-Z' 'a-z' | sed 's:/*$::')
[ -z "$from" ] && from=/
id=$(printf '%s' "$from" | sha256sum | cut -c1-32)
bkn store put redirects/rules --id "$id" \
  --data "{\"from\":\"$from\",\"to\":\"$2\",\"type\":${3:-301},\"enabled\":true}"
