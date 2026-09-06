# Live dogfood suite

These run against a **deployed** bkn over the public internet — not a test
server, not a mock. That is the point: they exercise the same TLS termination,
reverse proxy, systemd unit and SQLite file that real traffic hits.

```sh
export SP=$PWD/test
echo "$BKN_ADMIN_TOKEN" > "$SP/admin.tok"
for t in store kv auth access files runtime stripe forms cms headless; do
  bash "$SP/t-$t.sh"
done
```

`BKN_TEST_URL` points the suite at a different deployment. That is the only
knob: it moves the address, never an assertion — which is what lets the same
checks serve as the acceptance gate for a reimplementation.

There are **164** of them. The clean-room reimplementation (`machin-bkn`)
passes the 113 that existed when it was gated; the 51 added since — `access`
and part of `cms`/`headless` — are contract it has not been held to yet, which
is a statement about what has been ported, not about what passes.

`dog.sh` is the shared harness (auth helper, assertion helpers). Each suite
prints `[N passed, M failed]`.

## Coverage

| Suite | What it exercises |
|---|---|
| `store` | six verbs, all six filter operators, ordering, totals, id filters |
| `kv` | string/json/encrypted, public vs private visibility, on-disk payload |
| `auth` | login → me → refresh rotation → switch-org → logout, and the gates |
| `access` | collection policies, the five audiences, cross-tenant refusals, self-service |
| `files` | namespaces, dedup, inline-vs-attachment, ETag, type allow-list |
| `runtime` | events, cron (including the scheduler firing), locks, the sandbox surface |
| `stripe` | signature verification, replay window, idempotent retries |
| `forms` | validation, honeypot, dedupe, CSV export with RFC 4180 quoting |
| `cms` | i18n, redirects, feature flags, JSON configs |
| `headless` | schema validation, uniqueness, refs, tokens, the full CRUD surface |

## Suites must be re-runnable

`stripe`, `headless` and `access` scope their ids with `RUN=$(date +%s)`. Without that
they pass once and then fail against their own leftovers — which is what
happened the first time, and it masked a real defect underneath the noise.
