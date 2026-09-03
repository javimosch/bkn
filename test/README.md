# Live dogfood suite

These run against a **deployed** bkn over the public internet — not a test
server, not a mock. That is the point: they exercise the same TLS termination,
reverse proxy, systemd unit and SQLite file that real traffic hits.

```sh
export SP=$PWD/test
echo "$BKN_ADMIN_TOKEN" > "$SP/admin.tok"
for t in store kv auth files runtime stripe forms cms headless; do
  bash "$SP/t-$t.sh"
done
```

`dog.sh` is the shared harness (auth helper, assertion helpers). Each suite
prints `[N passed, M failed]`.

## Coverage

| Suite | What it exercises |
|---|---|
| `store` | six verbs, all six filter operators, ordering, totals, id filters |
| `kv` | string/json/encrypted, public vs private visibility, on-disk payload |
| `auth` | login → me → refresh rotation → switch-org → logout, and the gates |
| `files` | namespaces, dedup, inline-vs-attachment, ETag, type allow-list |
| `runtime` | events, cron (including the scheduler firing), locks, the sandbox surface |
| `stripe` | signature verification, replay window, idempotent retries |
| `forms` | validation, honeypot, dedupe, CSV export with RFC 4180 quoting |
| `cms` | i18n, redirects, feature flags, JSON configs |
| `headless` | schema validation, uniqueness, refs, tokens, the full CRUD surface |

## Suites must be re-runnable

`stripe` and `headless` scope their ids with `RUN=$(date +%s)`. Without that
they pass once and then fail against their own leftovers — which is what
happened the first time, and it masked a real defect underneath the noise.
