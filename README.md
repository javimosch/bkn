# bkn

A single-binary backend core: namespaced document collections and typed
settings over embedded SQLite, driven from the CLI.

No database server, no runtime, no admin UI, no container. One static binary
that is the server, the client, and the migration runner.

```sh
go build -o bin/bkn .

bkn guide                 # the mental model, embedded in the binary
bkn help-json             # machine-readable command catalog

bkn store create myapp/users --normalize email=trim_lower
bkn store put myapp/users --data '{"email":"  Ada@Example.IO ","name":"Ada"}'
bkn store find myapp/users --where email=ADA@EXAMPLE.IO

export BKN_ENCRYPTION_KEY=$(openssl rand -hex 32)
bkn kv set myapp.stripe sk_live_xxx --type encrypted
bkn kv get myapp.stripe

bkn script test --file digest.js --input '{"limit":10}'
bkn script create waitlist-digest --file digest.js
bkn script run waitlist-digest

bkn daemon start          # the same primitives over HTTP
curl -s localhost:7799/_health
```

## Why so few primitives

`bkn` replaces a 85k-line Node backend that had grown ~40 admin domains. An
audit of every real consumer of that backend found that:

- only six store operations were ever used — get, put, patch, delete, find by
  field, list — with **no** aggregations, joins or server-side sorts;
- consumers routinely bypassed the HTTP API and opened the database directly,
  because the API did not expose what they needed;
- one consumer imported the backend's internal service files, and its Mongoose
  instance, by absolute filesystem path.

The lesson was not "port the 40 domains". It was that features multiply when
the core lacks an escape hatch. So the core is small on purpose — **store**,
**kv**, and **script** — with everything else built on top rather than beside
them.

## Scripts are the escape hatch

Most of those 40 domains were a scheduled HTTP call, a transform over stored
records, or a webhook handler. Given a sandboxed runtime with access to the
other primitives, they are scripts:

```js
function main(input) {
  const subs = bkn.store.list("marketing/waitlist", { limit: input.limit || 100 });
  bkn.kv.set("marketing.last_digest", bkn.now());
  return { total: subs.length };
}
```

A script must define `main(input)`; its return value is the run's result. It
gets `bkn.store`, `bkn.kv`, `bkn.http.fetch`, `console.log`, `bkn.id()` and
`bkn.now()` — and nothing else. No filesystem, no processes, no timers, no
`require`, and no network beyond its own `allow_net` list. Every run is bounded
by `timeout_ms` and recorded with its status, logs, result and duration.

Outbound requests that resolve to a loopback, link-local or private address are
refused even when the hostname is allow-listed, which is what stops an
allow-listed name from reaching the cloud metadata endpoint.
`BKN_SCRIPT_ALLOW_PRIVATE_NET=1` lifts that deliberately.

There is no event loop: `bkn.http.fetch` blocks and returns its response, and
there are no promises or `async`/`await`.

## Conformance

Built to the agent-first CLI spec family — <https://cli-specs.intrane.fr>.

| Spec | Status |
|---|---|
| cli-output-spec | stdout = data, stderr = context, exit codes 0/80–119, typed errors with `recoverable` + `suggestions`, `help-json`, no internal retries |
| cli-guide-spec | `bkn guide` (embedded, `--human` markdown), `GET /guide`, `GET /llms.txt` |
| cli-daemon-spec | `bkn serve --host --port` (loopback default), `GET /_health`, `POST /_shutdown` (token-gated off-loopback), `bkn daemon start\|stop\|status` (idempotent, health-probed) |
| cli-update-spec | not yet |
| cli-feedback-spec | not yet |
| cli-telemetry-spec | not yet |

## Environment

| Variable | Purpose |
|---|---|
| `BKN_DATA` | datastore path (default `~/.bkn/bkn.db`) |
| `BKN_HOST` / `BKN_PORT` | serve bind defaults (`127.0.0.1` / `7799`); flags win |
| `BKN_ADMIN_TOKEN` | bearer token gating every non-public HTTP route; **required** to bind off-loopback |
| `BKN_SCRIPT_ALLOW_PRIVATE_NET` | `1` lets scripts reach loopback and private addresses |
| `BKN_ENCRYPTION_KEY` | single key, hex-64 / base64-32 / 32 literal chars; stamped as keyId `v1` |
| `BKN_ENCRYPTION_KEYS` | `v1:<material>,v2:<material>` — every key that can decrypt |
| `BKN_ENCRYPTION_KEY_ID` | which key encrypts new values |
| `SUPERBACKEND_ENCRYPTION_KEY`, `SAASBACKEND_ENCRYPTION_KEY` | read as fallbacks so values written by the Node backend decrypt unchanged |

## Encrypted values

The payload format is byte-compatible with the Node implementation it replaces
and is verified in both directions:

```json
{"alg":"aes-256-gcm","keyId":"v1","iv":"<b64>","tag":"<b64>","ciphertext":"<b64>"}
```

`keyId` was present in the original format but nothing ever read it, so
rotation was impossible in practice. `bkn kv rekey` implements it: add a key,
point `BKN_ENCRYPTION_KEY_ID` at it, rotate. Entries sealed by a key that is no
longer configured are reported rather than skipped, and do not block rotating
the rest.

## Status

Scaffold. `store`, `kv` and `script` are complete and tested; `auth`, `files`,
`events` and `cron` are the remaining core primitives.

~3,700 lines of implementation, ~800 of tests, one 19 MB static binary.
