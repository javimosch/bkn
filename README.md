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

bkn daemon start          # the same primitives over HTTP
curl -s localhost:7799/_health
```

## Why only two primitives

`bkn` replaces a 85k-line Node backend that had grown ~40 admin domains. An
audit of every real consumer of that backend found that:

- only six store operations were ever used — get, put, patch, delete, find by
  field, list — with **no** aggregations, joins or server-side sorts;
- consumers routinely bypassed the HTTP API and opened the database directly,
  because the API did not expose what they needed;
- one consumer imported the backend's internal service files, and its Mongoose
  instance, by absolute filesystem path.

The lesson was not "port the 40 domains". It was that features multiply when
the core lacks an escape hatch. So the core is small on purpose: **store** and
**kv**, with everything else built on top rather than beside them.

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

Scaffold. `store` and `kv` are complete and tested; `auth`, `files`, `events`,
`cron` and `scripts` are the remaining core primitives.
