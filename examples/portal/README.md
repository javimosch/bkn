# Portal — per-account auth + private files + contact book

A complete private-portal backend composed entirely of scripts and hooks —
no core features. This is the pattern for "a frontend on another origin
(hart artifact, static site, SPA) that needs real logins, role-gated
content, private file delivery and a contact form."

The core idea: **hooks are public routes; the script IS the authorization
boundary.** Every script here verifies the caller's bearer token itself with
`bkn.auth.verify` and resolves the caller's role from a store collection —
the hook registry grants nobody anything.

## Roles

`portal/access` is the role book: one document per email, `role` = `admin`
or `viewer`. It is policy — kept in the store so an admin can grant and
revoke at runtime (`admin-access.js`) without touching the auth tables.
Revoking also disables the auth user so issued sessions die at the next
verify.

## Scripts

| script | hook | does |
|---|---|---|
| `login-password` | POST | `bkn.auth.login` — password sign-in, tokens or `bad_credentials` |
| `guest-login` | POST | passwordless entry for whitelisted `viewer` emails; provisions the auth user lazily, audits every attempt |
| `whoami` | GET | verify bearer → identity + portal role |
| `contact-submit` | POST | validated contact write, idempotent on email+message hash |
| `admin-contacts` | GET | the address book, `admin` role only |
| `admin-access` | POST/DELETE | grant `viewer` / revoke (deletes access entry + disables the user) |
| `file-link` | GET | session check → 5-minute signed URL into the private namespace; the blob never transits the script |

## Setup

```sh
# org + first admin (role book is seeded through the CLI, not a hook)
bkn auth org create portal --name "Portal"
echo -n 'long-and-random' | bkn auth user create admin@example.com --password-stdin --name Admin
bkn auth member add portal admin@example.com --role owner
bkn store put portal/access --id admin@example.com \
  --data '{"email":"admin@example.com","role":"admin"}'

# scripts + public hooks — allow-origin is your page's origin, not the bkn host
for s in login-password guest-login whoami contact-submit admin-contacts admin-access file-link; do
  bkn script create $s --file examples/portal/$s.js
  bkn hooks create $s --script $s --rate-limit 60 --allow-origin https://your-frontend.example
done

# private files: signing key enables the signed URLs file-link mints
bkn files ns create portal-priv --signing-key auto
bkn files put portal-priv ./demo.glb --name demo.glb
```

## Frontend contract

```
POST /v1/hooks/login-password   {"email","password"}  -> {tokens:{access_token,...}}
POST /v1/hooks/guest-login      {"email"}             -> {tokens} or 403
GET  /v1/hooks/whoami           Authorization: Bearer -> {email, role}
POST /v1/hooks/contact-submit   {name,email,message}  -> {ok,id}
GET  /v1/hooks/file-link?name=x Authorization: Bearer -> {url:"/v1/files/..."} (prepend the bkn origin)
```

The page's CSP needs `connect-src <bkn origin>`. Signed file URLs are
relative paths — prepend the bkn origin before handing them to a renderer.

## What this deliberately does not do

- **Email verification.** `guest-login` trusts the whitelist; wiring a
  verify-by-email round trip is a script + `bkn.http.fetch` to your mailer,
  not a core feature.
- **CSRF tokens.** `allow_origin` + per-IP `--rate-limit` is the bound;
  bearer tokens are not ambient credentials, so CSRF does not apply the way
  it does to cookie sessions.
- **Lockout.** Repeated failures are audited in `events` (`login.denied`)
  and IP-limited by the hook; there is no per-email counter yet.
