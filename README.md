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

echo -n 'correct-horse-battery' | bkn auth user create ada@example.io --password-stdin
bkn auth org create acme --name "Acme Inc"
bkn auth member add acme ada@example.io --role owner
echo -n 'correct-horse-battery' | bkn auth login ada@example.io --password-stdin --org acme

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

## Identity, without billing

`auth` holds users, organizations, memberships and tokens. Emails are
normalized in exactly one place. Passwords are bcrypt, so hashes written by the
Node backend's `bcryptjs` keep verifying — checked in both directions.

Access tokens are short-lived HS256 JWTs carrying `sub`, `email`, `role`, `org`
and `org_role`. Refresh tokens are opaque, stored only as a digest, and
single-use: every refresh rotates them, so a stolen one dies the moment the
real holder refreshes. Changing a password or disabling a user revokes that
user's sessions immediately.

Platform roles (`user`, `admin`) govern the deployment. Organization roles
(`owner` > `admin` > `member`) govern a tenant. A platform admin is **not**
implicitly an owner of anybody's organization.

What `auth` deliberately does **not** hold is billing. In the Node backend,
`subscriptionStatus`, `currentPlan` and `stripeCustomerId` lived on the user
record, and the absence of an endpoint to write `stripeCustomerId` is what
drove a consumer to bypass the API and write MongoDB directly. Billing goes in
a store collection:

```sh
bkn store put billing/subjects --id <user-id> \
  --data '{"plan":"pro","status":"active","stripe_customer_id":"cus_x"}'
```

## Files

Namespaced blob storage. A namespace declares its backend (`local` or `s3`),
a per-file size cap, the content types it accepts, and whether its files are
served over HTTP without auth.

```sh
bkn files ns create avatars --allow-type 'image/*' --max-bytes 1048576 --public
bkn files put avatars ./ada.png
curl -s localhost:7799/v1/files/avatars/ada.png -o ada.png   # no auth
```

Bytes are stored under their **SHA-256, never under the caller's filename**.
That is not a flourish: it means no user-supplied name ever reaches the
filesystem, so directory traversal is impossible by construction rather than by
validation. Identical uploads deduplicate, and the hash doubles as the ETag.
Two names sharing one blob are reference-counted — deleting one keeps the other
readable.

Serving user uploads inline from the API's own origin is a stored-XSS delivery
mechanism, so only a short allow-list of types is served inline (common images,
`text/plain`, audio, video). HTML, **SVG**, PDF and JavaScript are served as
attachments with `X-Content-Type-Options: nosniff` and
`Content-Security-Policy: default-src 'none'; sandbox`. A file in a non-public
namespace answers 404 to an unauthenticated caller, exactly like one that does
not exist.

The `s3` backend speaks the S3 REST API directly with hand-rolled SigV4 — no
SDK, because object storage needs three verbs and one signing algorithm, and
the official client pulls in a dependency tree larger than this program. It is
verified against a live MinIO in the test suite:

```sh
docker run -d --rm -p 9123:9000 -e MINIO_ROOT_USER=minioadmin \
  -e MINIO_ROOT_PASSWORD=minioadmin quay.io/minio/minio server /data
BKN_S3_TEST_ENDPOINT=http://127.0.0.1:9123 go test ./internal/files/
```

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
gets `bkn.store`, `bkn.kv`, `bkn.auth`, `bkn.files`, `bkn.http.fetch`,
`console.log`, `bkn.id()` and `bkn.now()` — and nothing else. No filesystem, no processes, no timers, no
`require`, and no network beyond its own `allow_net` list. Every run is bounded
by `timeout_ms` and recorded with its status, logs, result and duration.

Outbound requests that resolve to a loopback, link-local or private address are
refused even when the hostname is allow-listed, which is what stops an
allow-listed name from reaching the cloud metadata endpoint.
`BKN_SCRIPT_ALLOW_PRIVATE_NET=1` lifts that deliberately.

There is no event loop: `bkn.http.fetch` blocks and returns its response, and
there are no promises or `async`/`await`.

The trust model is worth stating plainly: **scripts are operator code, not
tenant code.** A script already reads decrypted secrets through `bkn.kv`, and
`bkn.auth.issue` can mint a session for any user — which is what makes SSO
callbacks and invite acceptance scriptable. Review a script the way you would
review core.

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
| `BKN_FILES_DIR` | where local blobs live (default `~/.bkn/files`) |
| `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `S3_ACCESS_KEY_ID`, `S3_SECRET_ACCESS_KEY` | enable the `s3` backend; `BKN_S3_*` also accepted |
| `BKN_AUTH_SECRET` | token signing secret; generated and stored in `kv` if unset |
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

Scaffold. `store`, `kv`, `script`, `auth` and `files` are complete and tested;
`events` and `cron` are the remaining core primitives, after which the old
system's domains get ported as scripts.
