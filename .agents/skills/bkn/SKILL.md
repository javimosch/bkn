---
name: bkn
description: Compose real backends (auth, store, files, hooks, cron) from the bkn single-binary backend — primitives in core, domains in scripts. Use when a project needs a backend, login/sessions, private files, webhooks, or scheduled jobs without standing up a framework.
---

# bkn — backend primitives, domains in scripts

One static binary, embedded SQLite. CLI first, HTTP mirror second. The data
file is `~/.bkn/bkn.db` unless `BKN_DATA` overrides. `bkn guide` prints the
full embedded manual (offline, version-exact) — read it before assuming a
capability is missing.

## Primitives (the whole surface)

| verb | gives you |
|---|---|
| `store` | namespaced document collections, typed filters, `putIfAbsent` for idempotency |
| `kv` | typed settings, `--type encrypted` with a keyring |
| `auth` | users, orgs, memberships, bcrypt passwords, JWT access + single-use refresh tokens |
| `files` | namespaced blobs (local/S3), public or private namespaces, HMAC signed URLs |
| `events` | append-only audit log |
| `cron` | scheduled scripts inside `bkn serve` |
| `script` | sandboxed JS over all of the above: `bkn.store`, `bkn.kv`, `bkn.auth`, `bkn.files`, `bkn.events`, `bkn.lock`, `bkn.crypto`, `bkn.http.fetch`, `bkn.now()`, `bkn.id()` |
| `hooks` | public HTTP endpoints bound to scripts — the script is the auth boundary |
| `access` | declarative read/write policies on collections |

## The rule that shapes everything

**Core holds primitives; domains are scripts.** Before concluding bkn "can't
do X", check the three layers: a direct `/v1/*` route, a script API
(`bkn.*`), and `examples/` recipes. Password login, for example, is
`POST /v1/auth/login` — and `bkn.auth.login(email, password, org)` inside a
script composes it with your own policy (whitelist, org check, audit).

## Gotchas that will bite you

- **Hook delivery `body` is a STRING.** `JSON.parse(d.body)` before reading
  fields. `d.body.email` on the raw string is `undefined` — fails silently as
  bad credentials, not an error. `body_base64` is the exact bytes; never
  re-serialize before verifying a signature.
- **Hooks are public by design.** No bearer check happens before your script
  runs — verify inside with `bkn.auth.verify(token)` and scope with
  `bkn.caller` / a role collection. Add `--rate-limit` to anything
  browser-reachable.
- **Browser calls need `--allow-origin <page origin>`** on the hook, or the
  preflight is refused and the request never leaves the browser.
  `Authorization` and `Content-Type` are the allowed headers.
- **`bkn.auth.issue` mints a session for ANY user** — powerful (SSO
  callbacks, guest whitelist) and dangerous. Scripts are operator code;
  review them like core.
- **`files.sign` returns a RELATIVE path** (`/v1/files/ns/name?sig=…`) —
  prepend the bkn origin. `ttl` takes a duration string (`"5m"`).
- **A private file answers 404 to the unauthenticated** — same as missing.
  Authenticated access is the admin bearer or a signed URL; there is no
  per-session file auth — gate through a hook like `examples/portal/file-link.js`.
- **Passwords via `--password-stdin`, never `--password`** (process table).
- **Refresh tokens are single-use** — keep the newest. Access tokens are NOT
  revoked by logout; `auth me` checks the live user record.
- **`--run-access` on scripts:** `admin` (default), `user`, `org`, `public`.
  Opening a script hands callers everything the script can do — the sandbox
  still holds the full store, so scope inside.

## Recipes

`examples/` — runnable, one directory per domain:

- `portal/` — per-account auth (password + guest whitelist), session-gated
  private files via signed URLs, contact book, runtime grant/revoke, full
  audit trail. The "frontend on another origin needs real logins" pattern.
- `drive/` — upload/link/purge file flows.
- `forms/` — contact form + waitlist export.
- `stripe-webhook/` — signature-verified provider webhook (`crypto.hmac` +
  `crypto.equal`, never `===`).
- `headless/`, `blog-automation/`, `configs/`, `flags/`, `i18n/` —
  CMS/config/flag patterns.

## Deploy shape

`bkn serve` binds `127.0.0.1`; put a reverse proxy in front for public
hostnames. Per-app instances (separate binary path + `BKN_DATA` + systemd
unit) are the isolation pattern — one bkn per tenant, not one bkn with
namespaces.
