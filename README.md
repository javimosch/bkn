# bkn

A single-binary backend core: namespaced document collections and typed
settings over embedded SQLite, driven from the CLI.

No database server, no runtime, no admin UI, no container. One static binary
that is the server, the client, and the migration runner.

**Ported from** [superbackend](https://github.com/javimosch/superbackend) — an
85k-line Node/Express/MongoDB backend with ~40 admin domains, whose features
bkn reimplements as nine scripts sitting on seven primitives.

**If you want a UI, use [PocketBase](https://pocketbase.io).** It is the
closest thing to this and it is very good: one Go binary, embedded SQLite,
realtime subscriptions, and a genuinely nice admin dashboard. bkn makes the
opposite trade deliberately — no dashboard, a CLI that *is* the interface, and
a sandboxed script runtime as the extension point instead of Go hooks. If a
human is going to click through your data, PocketBase is the better tool.

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

## Why there is no admin UI

This is the decision people push back on, so it deserves an argument rather
than a slogan.

**The UI was 30% of the system it replaced and the least testable part of it.**
superbackend's admin was 83 EJS templates, ~36,000 lines — comparable to its
entire service layer. None of it was covered by a test, none of it could be
driven by anything but a person, and every feature had to be built twice: once
as an endpoint and again as a screen.

**A UI is a client, not an interface.** Every one of the nine ported domains
turned out to be reachable as CLI verbs over a JSON contract; the admin screens
were a second, unversioned client of that contract. bkn keeps the contract and
drops the second client. The HTTP API is still there — if you want a dashboard,
build one against it. bkn just declines to make it the product.

**A CLI can be introspected; a UI cannot.** `bkn help-json` returns every
command, flag, exit code and environment variable as JSON. `bkn guide` returns
the mental model, the loop, the concepts and the gotchas — embedded in the
binary, offline. An agent that has never seen this tool learns to drive it in
two calls. There is no equivalent for a screen: the only way to find out what a
dashboard does is to look at it.

**Composability is the whole point.**

```sh
bkn store list shop/orders --where 'total>100' --order-by total:desc   | jq -r '.records[] | [.id, .total] | @tsv'
```

That is a sentence. The dashboard equivalent is a screenshot.

**The honest counterargument.** A UI is genuinely better for browsing data you
cannot yet describe, and it lets non-technical operators work without a
terminal. bkn loses both. That is a real cost, and if it matters to you,
PocketBase already solved it well. This is a scope decision about what belongs
in the core — not a claim that UIs are bad.

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

Small is not the same as limited. The bar for any application is not "could
this run on bkn" but "would this be *less code* on bkn" — an application bkn
cannot serve is a gap to close, not a boundary to defend. What keeps that from
rebuilding superbackend is a single admission rule, in
[VISION.md](VISION.md#what-gets-in): admit a primitive that removes a class of
application code from every embedder; refuse a query feature that only moves
application code into bkn.

Fit-checked against two real Go codebases, counting distinct SQL statements and
how many exceed what `store` offers:

| codebase | statements | beyond the allowance | genuinely relational |
|---|---|---|---|
| superlandings-go — 8k LOC, 8 tables | 48 | 3 | 1 |
| automaintainer-saas-panel — 131k LOC, 20 tables | 202 | 33 (16%) | ~10 (5%) |

The large one is the informative one: of its 33 outliers, only the 8 joins and
2 subqueries are relational. The rest is atomicity and summarisation.

## Writes that do not lose each other

`patch` used to read a document, merge in Go, and write the whole thing back.
Two concurrent patches of different fields kept only one of them — 16 concurrent
increments landed as `1`, and every contender for a job "won" it.

A patch field may now be an operator, computed from the field's current value:

```sh
bkn store patch app/runs r1 --data '{
  "tries":  {"$inc": 1},
  "log":    {"$append": "started\n"},
  "stages": {"$push": "build"},
  "worker": {"$setIfEmpty": "w1"}
}'
```

The write is a compare-and-set against the exact document the merge was computed
from, retried on a lost race — so concurrent patches accumulate instead of
erasing each other, across processes and not merely within one. A missing field
is the operator's identity: incrementing an absent counter yields the operand.
A plain object is still a plain value; only a single `$`-prefixed key is an
operator.

Preconditions guard a write, and are how one worker claims a job:

```sh
bkn store patch app/runs r1 --data '{"status":"done"}'  --if status=running
bkn store patch app/runs r1 --data '{"worker":"w1"}'    --if-absent worker
```

Conditions are ANDed and checked against the document being written; a failed
one writes nothing and exits `95` (`precondition_failed`). There is deliberately
no `OR`, matching the query surface — an either/or guard belongs in your control
flow, not in a predicate language growing inside bkn. The same options are query
parameters over HTTP (`?if=status=running`, `?if-absent=worker`, `409` on
failure) and an optional fourth argument in scripts.

These are primitives, not query features, by the test in
[VISION.md](VISION.md#what-gets-in): they remove read-modify-write races from
every caller, and userland cannot implement them safely — a script that reads,
adds one and writes back *is* the race. The core needed them first: cron's
`claim()` compare-and-sets a job's next run in one `UPDATE` for exactly this
reason, and applications had no way to say the same thing.

## How many, not which

A control plane's first dashboard question is a rollup:

```sql
SELECT status, COUNT(*) FROM explore_candidates
 WHERE repo_id=? AND user_id=? GROUP BY status
```

```sh
bkn store count app/runs --where repo_id=7 --by status
```

```json
{"by":"status","total":41,"groups":3,"truncated":false,
 "buckets":[{"key":"ok","count":30},{"key":"failed","count":8},{"key":"stale","count":3}]}
```

Without `--by` it is a plain total. With it, one bucket per distinct value,
largest first — the same shape and ordering `events stats --by` already uses,
because a store is no less entitled to a rollup than a log is.

`total` counts matching documents and `groups` counts distinct values *before*
any limit, so truncation is visible rather than silent: a rollup on a
high-cardinality field returns capped buckets and still tells you how many
groups there really were. A document with no value for the field gets a `null`
key, which is deliberately different from an empty string.

The alternative was listing the collection and counting in the caller — which
is precisely what rule 2 rejected when it admitted ordering and ranges, because
doing them in a script means loading the collection into the VM. A rollup is
admitted on the same ground, and stays a summarisation rather than a query
language: one field, one aggregate, no expressions, no `HAVING`, no joins, and
it never returns documents.

Over HTTP the same question is `?by=status`, alongside the filters — `by` and
`by_limit` join `limit` and `order_by` as reserved parameter names. In a script
it is `bkn.store.countBy(ref, where, field)`.

## Collections that bound themselves

An unbounded log table is a disk-space incident waiting to happen, so every
codebase grows a trim query and a job to run it. One 131k-line control plane
ran this twice:

```sql
DELETE FROM repo_memories WHERE tag=? AND repo_id=? AND user_id=?
  AND id NOT IN (SELECT id FROM repo_memories WHERE tag=? AND repo_id=?
                 AND user_id=? ORDER BY created_at DESC LIMIT ?)
```

Declare the bound on the collection instead:

```sh
bkn store create app/runs     --retain-last 500
bkn store create app/memories --retain-last 20 --retain-per tag,repo_id,user_id
```

`--retain-last N` keeps the newest N documents. `--retain-per` gives each
distinct value of those fields its own N, which is what the query above was
doing by hand. The store enforces the bound on every write and again the moment
the policy is declared, so a bound set on a collection that already holds a
million rows applies immediately rather than at the next write.

That deletes two things from every caller: the query, and the schedule that ran
it. No bound means unbounded, exactly as before. `--retain-per` without
`--retain-last` is rejected rather than accepted-and-ignored — a policy that
reads like a policy and enforces nothing is worse than no policy.

Ordering is `created_at`, then insertion order. The tiebreak matters because
timestamps have second resolution: without it, documents written in the same
second would be evicted arbitrarily.

Retention is declared on the collection, like `--normalize`, and a collection
is declared either from the CLI or with `PUT /v1/store/<ns>/<coll>` — which is
how a deployment behind a reverse proxy, with nobody logged into the box, gets
one at all.

## One place for an invariant

A collection declares how its fields are normalized, and the store applies the
rule on every write **and** to every filter value for the same field, so a
lookup finds the row a normalized write created:

```sh
bkn store create app/users   --normalize email=trim_lower
bkn store create app/reports --normalize declarant.email=trim_lower
```

A field may name a nested key with dots. The filter side already addressed
documents that way (`--where declarant.email=...`), and until both halves
agreed, a nested rule was accepted and then silently skipped on write — which
left the document unfindable by the very field its collection declared as
normalized, because the lookup value *was* normalized and the stored value was
not.

A path the store cannot address — array indices, quoting, wildcards — is
**refused when declared** rather than accepted and ignored. Rule 5: fail
loudly, never downgrade.

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

## Collections that know who is asking

Every data route used to require the admin token, so a browser or a phone
could not talk to bkn at all. The application in between had to re-implement
the same three lines — verify the token, read the subject, add the tenant
filter — at every endpoint, and the day one of them was forgotten, that
endpoint returned everybody's rows.

A collection declares who may do what, and bkn enforces it:

```sh
bkn store create app/notes --owner-field user_id \
  --access read=owner,create=owner,update=owner,delete=owner

bkn store create app/docs  --org-field org_id --access read=org,create=org
bkn store create site/pages --access read=public
```

Four verbs — `read` (get, list and count), `create`, `update`, `delete` — each
mapped to one of five audiences:

| audience | who | scoped to |
|---|---|---|
| `admin` | the admin token | everything |
| `user` | any valid access token | everything |
| `owner` | a token holder | documents whose `--owner-field` is them |
| `org` | a token holder | documents whose `--org-field` matches their token's org |
| `public` | anybody | everything |

**`admin` is the default, so nothing changed for a collection nobody has
spoken about.** A policy grants and never revokes: `--access read=public`
leaves writes admin-only, because an undeclared verb is admin.

The part that removes bugs rather than catching them is that a scoped create
takes its tenancy **from the token, not from the body**:

```sh
curl -H "Authorization: Bearer $ADA" -d '{"title":"hi","user_id":"SOMEBODY-ELSE"}' \
     $HOST/v1/store/app/notes
# -> {"record":{"id":"...","title":"hi","user_id":"<ada's id>"}}
```

A document cannot be written into the wrong tenant, because the tenant is not
something the client gets to say. The read side is the same idea: the scope is
part of the query, not a check beside it, so `?user_id=someone-else` narrows an
already-scoped page to nothing rather than widening it — filters are ANDed and
there is no `OR` to escape through. A rollup (`?by=`) is scoped too, or it
would leak the count of what you cannot read.

Three refusals are worth stating, because each one is a way this normally goes
wrong:

- **A scoped read, patch or delete of another tenant's document answers 404**,
  not 403. A 403 confirms the id is real to somebody with no right to know it.
- **A caller-supplied id cannot take a document.** `POST ...?id=<theirs>` is an
  upsert, and a create policy stamps your id onto it — so without a guard it
  would replace their document and relabel it as yours. The insert and the
  ownership test are one statement (`ON CONFLICT ... WHERE`), not a check
  followed by a write.
- **An `org` audience refuses a token carrying no organization.** Reading it as
  "unscoped" would hand every tenant's documents to somebody who merely forgot
  to pick one — the exact failure the mechanism exists to prevent.

Scoped updates reuse machinery that was already there: the scope becomes a
precondition on the compare-and-set that `patch` already performs, so a
document that changes hands mid-flight fails the write instead of passing a
stale test.

This is admitted by the rule in [VISION.md](VISION.md#what-gets-in) — it
removes a class of application code from *every* embedder, and userland cannot
implement it safely, because a filter the caller has to remember to add is a
filter somebody eventually forgets. What it is not is a rule language: an
audience is one word from a closed set and a scope is a field name, never an
expression, for the same reason the query surface refuses `OR`.

### What an application needs besides the store

Scoping the data was necessary and not sufficient. Three things were operator-
only because the only client had ever been an operator:

```sh
POST /v1/auth/register          # self-service signup, off unless BKN_OPEN_SIGNUP=1
POST /v1/auth/password          # change your own, verifying the current one
POST /v1/auth/orgs              # create a workspace; the creator becomes owner
POST /v1/auth/orgs/{org}/members    # invite, gated by org role, not by the admin token
GET  /v1/auth/orgs/{org}/members
DELETE /v1/auth/orgs/{org}/members/{user}
PUT  /v1/store/{ns}/{coll}      # declare a collection: normalizers, retention, access
```

Registration will not let a caller name their own role, or it would be an
admin-account vending machine, and it shares the login throttle, since it is
the other way to learn whether an address has an account. Any signed-in user
may create an organization and becomes its owner — that is the "create a
workspace" move a product needs, and the deployment-level control over it is
`BKN_OPEN_SIGNUP`: with signup closed, the only accounts that exist are ones an
operator made. A password change
verifies the current password rather than trusting the access token, because a
stolen token is exactly the situation where it must not be enough.

Browsers need one more thing: `BKN_CORS_ORIGIN` is an explicit allow-list,
unset by default. It is not a reflected origin — bkn authenticates with a
bearer header, and a permissive default would let any page on the internet
spend a token it managed to obtain.

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

A name is claimed and written in one statement. `files put` refuses an
existing name with `already_exists` unless `--overwrite`, and that refusal used
to be a lookup followed, some way down, by an unconditional upsert — so two
uploads of the same free name both saw it free, both wrote, and the second
silently replaced the first. Nothing looked wrong afterwards: the name still
resolved, it just pointed at the other file. A test runs twelve writers at one
name and asserts exactly one wins; against the previous code **all twelve
reported success.**

A namespace may also decide a file's type from its bytes rather than from what
the uploader said:

```sh
bkn files ns create avatars --allow-type 'image/*' --verify-type
```

Without it the allow-list is checked against a string the caller sent —
declare `Content-Type: image/png` over any bytes at all and an image-only
namespace accepts them and records them as an image. That is tolerable while
the only writer holds the admin token, and stops being tolerable the moment a
tenant can upload.

The cost is worth stating rather than hiding, because it decides whether you
want the flag: **a sniffer sees bytes, not intentions.** A namespace that
verifies types allow-lists what files *look like*, not what they are called. A
`.docx` is a zip and sniffs as `application/zip`; a `.css` full of words sniffs
as `text/plain`. A declared type is kept when it is a more specific truth about
bytes the sniffer can only call a container — `docx` over zip is accepted,
`image/png` over zip is not. And bytes resembling no known format sniff as
`application/octet-stream`, which is the sniffer declining to answer rather
than evidence of a lie, so arbitrary binary can still be declared as anything
the allow-list permits. What the flag removes is the whole class the sniffer
*can* name: HTML, JavaScript, SVG, and every text format a browser would act
on. It is off by default, so existing namespaces are unchanged.

The `s3` backend speaks the S3 REST API directly with hand-rolled SigV4 — no
SDK, because object storage needs three verbs and one signing algorithm, and
the official client pulls in a dependency tree larger than this program. It is
verified against a live MinIO in the test suite:

```sh
docker run -d --rm -p 9123:9000 -e MINIO_ROOT_USER=minioadmin \
  -e MINIO_ROOT_PASSWORD=minioadmin quay.io/minio/minio server /data
BKN_S3_TEST_ENDPOINT=http://127.0.0.1:9123 go test ./internal/files/
```

## Events

One append-only log for what used to be three admin domains. Errors, audit
trails and counters differ only in which field you care about, so an event has
a stream, a type, a level, a source, a subject and a JSON body.

```sh
bkn events emit errors http.500 --level error --source api --subject /v1/orders
bkn events stats errors --by subject --since 24h
bkn events list errors --level error --limit 20
bkn events prune --older-than 30d
```

There is no automatic retention: the log grows until something prunes it.
That is deliberate — deletion should be something someone asked for — and
`prune` on a cron job is the usual way to ask.

## Cron

A schedule bound to a script. The scheduler is thin because `script` already
runs code safely with a timeout and a run history; it only has to answer "what
is due".

```sh
bkn cron create nightly --schedule '@daily' --script retention \
  --input '{"stream":"errors","older_than":"30d"}'
bkn cron list
bkn cron run nightly     # immediately, without consuming the scheduled slot
```

Standard 5-field expressions plus `@hourly`/`@daily`/`@weekly`/`@monthly`/
`@yearly` and `@every 5m`. Jobs fire while `bkn serve` runs; `bkn cron tick`
performs exactly the same pass once, for anyone who would rather drive the
timing from systemd or a real crontab. Overlapping runs are skipped rather than
queued, and every run is recorded in the `cron` event stream.

## Hooks — the inbound counterpart

`bkn.http.fetch` lets a script call out. `hooks` lets the world call in.

```sh
bkn hooks create stripe --script stripe-webhook
# -> /v1/hooks/stripe   PUBLIC and unauthenticated
```

This route is deliberately public, because providers authenticate with a
signature header rather than a bearer token. Without it, no signed integration
could ever be a script and every one of them would have to be Go code — which
is the thing the sandbox exists to avoid.

The script receives the body **byte-for-byte**, the lower-cased headers and the
query string, and returns `{status, body, headers}` to shape the reply. It is
the only thing between the internet and your data, so it must verify the
signature — `bkn.crypto` exists for that:

```js
const expected = bkn.crypto.hmac(secret, parts.t + "." + d.body);
if (!bkn.crypto.equal(expected, parts.v1)) return { status: 400, body: { error: "bad signature" } };
```

`bkn.crypto.equal` is constant-time; `===` leaks the correct prefix through
timing. Digests are verified against Node's `crypto` in the test suite, since
that is what the providers document against.

A script that throws answers 500 so the provider retries, rather than silently
swallowing a delivery.

## Locks

An expiring lease for work that must not overlap across processes — the
scheduler's in-process guard says nothing about a CLI `cron tick` racing the
daemon, or two deployments sharing a database.

```js
const lock = bkn.lock.acquire("blog-automation", 900);
if (!lock) return { skipped: "already running" };
try { /* … */ } finally { bkn.lock.release(lock.key, lock.owner); }
```

Acquisition is a single conditional statement, so a read-then-write race cannot
produce two holders; there is a test that runs 24 goroutines at one key and
asserts exactly one wins.

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
gets `bkn.store`, `bkn.kv`, `bkn.auth`, `bkn.files`, `bkn.events`, `bkn.lock`,
`bkn.crypto`, `bkn.http.fetch`, `console.log`, `bkn.id()` and `bkn.now()` —
and nothing else. No filesystem, no processes, no timers, no
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
| cli-update-spec | `bkn update [--check] [--force]` — content-hash versions, verify + smoke-test before an atomic swap, `.bak` kept; `bkn install`/`uninstall` (§6); throttled passive nudge; `GET /version` + `/dl/bkn` when serving |
| cli-feedback-spec | `bkn feedback` — dual-write to the app endpoint and the relay under one id, never fails the caller; `POST /v1/feedback` open, capped, rate-limited, idempotent |
| cli-telemetry-spec | `bkn telemetry [--on\|--off]` — **opt-in**, allow-listed payload, disclosed before the first send, honours `DO_NOT_TRACK` and CI |

## Environment

| Variable | Purpose |
|---|---|
| `BKN_DATA` | datastore path (default `~/.bkn/bkn.db`) |
| `BKN_HOST` / `BKN_PORT` | serve bind defaults (`127.0.0.1` / `7799`); flags win |
| `BKN_ADMIN_TOKEN` | bearer token gating every admin HTTP route; **required** to bind off-loopback |
| `BKN_CORS_ORIGIN` | comma-separated origin allow-list for browser callers (`*` permitted, and echoed rather than returned literally); unset means no cross-origin access |
| `BKN_OPEN_SIGNUP` | `1` enables `POST /v1/auth/register` |
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

## Ported domains

Two features that were whole admin domains in the Node backend now run as
scripts, with no Go code added for either — see [`examples/`](examples/):

| Domain | Node | bkn |
|---|---|---|
| Blog automation | ~2,500 lines, 3 models | 234 lines of JavaScript |
| Stripe webhook | ~1,500 lines, 2 models | 159 lines of JavaScript |
| Forms | forms service, controller, model | 154 lines of JavaScript |
| Waiting-list exports | ~900 lines, two services | 120 lines of JavaScript |
| i18n | 2 services, 2 controllers, 2 models | 240 lines of JavaScript |
| Page redirects | service, admin routes, model | 110 lines of JavaScript |
| Feature flags | service + controller | 138 lines of JavaScript |
| JSON configs | service + controller + model | 144 lines of JavaScript |
| Headless CMS | ~780 lines, 2 models | 422 lines of JavaScript |

That is the test the core was built for. Porting them surfaced gaps — no
crypto in the sandbox, no public inbound route, no atomic conditional write, a
binary-unsafe `fetch`, then GET hooks, non-JSON responses, CORS and rate
limiting — every one of which became a general primitive rather than a
one-off patch. Four consecutive domains needed no core change at all; the
headless CMS finally did, and got a bounded query surface rather than a query
language.

## Keeping itself current

The version is `sha256[:12]` of the binary, so nothing needs bumping — identical
bytes are the same version.

```sh
./bkn install                 # copies the binary you have onto ~/.local/bin
bkn update --check            # exit 5 means an update exists
bkn update                    # verify hash -> run it -> swap -> keep .bak
```

The download is verified **and executed** before it replaces anything: a
truncated publish can still match its own hash, and running it is the only
check that catches that. The previous binary stays at `<path>.bak`, so a
rollback is one `mv`.

A running `bkn serve` publishes updates for other machines from
`BKN_RELEASE_DIR`, via `GET /version` and `/dl/bkn`.

## Feedback and telemetry

`bkn feedback "..."` dual-writes to this instance and to a central relay under
one id, so a retry is stored once. It **never fails the caller** — losing a bug
report because reporting it failed is the worst outcome — and
`FEEDBACK_RELAY=off` keeps submissions local.

Telemetry is **opt-in**, not opt-out. The spec permits either and asks for
opt-in from tools handling credentials; bkn holds encrypted secrets, password
hashes and token signing keys, so nothing is sent until `bkn telemetry --on`.
`bkn telemetry` prints the exact payload it would send, and
`BKN_TELEMETRY_URL` redirects it — so what it sends can be observed without
letting it send.

```
$ bkn telemetry
enabled: False | reason: not enabled; bkn is opt-in (bkn telemetry --on)
payload: {tool, version, event, verb, os, arch, exit_class, ts}
```

`install_id` is omitted entirely, which the spec calls the safer default.

**No collector is running yet.** The endpoint follows the convention the rest
of these tools use (`feedback.intrane.fr/v1/telemetry`), but the relay
currently serves only `/v1/feedback`, so an enabled sender posts into a void —
silently, with no retry and no effect on the exit code, which is exactly what
the spec asks for from an unreachable collector.

## Deployment

The reference deployment is `bkn.intrane.fr` — systemd on loopback, Traefik for
TLS, Cloudflare for DNS. See [`docs/deploy.md`](docs/deploy.md).

One hazard is worth repeating here. `serve` binds loopback and, with no
`BKN_ADMIN_TOKEN`, treats that as "only a co-resident process can reach me" —
true on a laptop, **false behind a public reverse proxy**, which is the normal
way to deploy this. bkn refuses the loopback exemption for any request carrying
a forwarding header and warns at startup, but set `BKN_ADMIN_TOKEN` regardless:
it is the thing that actually gates the admin routes.

```
$ curl -s -o /dev/null -w '%{http_code}\n' https://bkn.intrane.fr/v1/kv
403
```

## Status

The core primitives — `store`, `kv`, `auth`, `files`, `events`, `cron`,
`hooks`, `lock` and `script` — are complete and tested, and nine real domains
have been ported onto them.

With collection access policies, a browser or mobile client can talk to bkn
directly: identity, tenancy, self-service signup and workspace membership are
all reachable over HTTP. Three things are honestly not:

- **Files are not scoped.** A namespace is `--public` or admin-only; there is
  no per-user upload, so avatars and user attachments still go through a hook
  script that does its own checking. The two things that had to be true before
  that could change are now true — a name cannot be taken by a racing writer,
  and a namespace can refuse bytes that contradict their declared type — but
  the policy layer itself is not built. Note that scoping would not remove a
  hook like the signalement one: its real work is relational (does this record
  exist, does it already have five) and no namespace policy expresses that.
- **There is no realtime.** No websockets, no server-sent events. Anything
  built on presence, live cursors or a chat feed is not a bkn application.
- **There is no text search.** Filters are equality, ranges and `in`; a search
  box over content has nothing to call.

The first is a gap. The other two are scope decisions, and closing either one
would be a different program.
