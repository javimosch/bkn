# Examples — domains ported from the Node backend

These are the proof that the core is the right size: two features that were
whole admin domains in the 85k-line Node backend, running as scripts with no
Go code added for either.

| Domain | Node | bkn |
|---|---|---|
| Blog automation | ~2,500 lines across services, controllers, routes and 3 Mongoose models | [`blog-automation.js`](blog-automation/blog-automation.js), 234 lines |
| Stripe webhook | ~1,500 lines across services, controllers, routes and 2 models | [`stripe-webhook.js`](stripe-webhook/stripe-webhook.js), 159 lines |
| Forms | forms.service + controller + FormSubmission model | [`forms.js`](forms/forms.js), 154 lines |
| Waiting-list exports | ~900 lines across two services plus controllers and routes | [`waitlist-export.js`](forms/waitlist-export.js), 120 lines |
| i18n | i18n.service + i18nInferredKeys.service + 2 controllers + 2 models | [`i18n.js`](i18n/i18n.js) 158 + [`i18n-import.js`](i18n/i18n-import.js) 82 lines |
| Page redirects | pageRedirects.service + admin routes + model | [`redirects.js`](redirects/redirects.js), 110 lines |
| Feature flags | featureFlags.service + controller | [`feature-flags.js`](flags/feature-flags.js), 138 lines |
| JSON configs | jsonConfigs.service + controller + model | [`configs.js`](configs/configs.js) 67 + [`config-save.js`](configs/config-save.js) 77 lines |
| Headless CMS | headlessModels.service + 2 controllers + token middleware + 2 models, ~780 lines | [`headless.js`](headless/headless.js) 311 + [`headless-model.js`](headless/headless-model.js) 111 lines |

## blog-automation

Picks a weighted topic, asks a model for an angle and a research query,
researches it, drafts the article, generates a cover image, and publishes —
on a schedule.

```sh
bkn kv set blog.llm_key sk-... --type encrypted
bkn store put blog/configs --id default --data @blog-automation/config.json
bkn files ns create blog-images --allow-type 'image/*' --public
bkn script create blog-automation --file blog-automation/blog-automation.js \
  --allow-net api.openai.com --timeout 180000
bkn cron create blog-daily --schedule '0 7 * * *' --script blog-automation \
  --input '{"trigger":"scheduled"}'
```

Uses `store` (config, posts, run records), `files` (cover images), `events`
(audit and the runs-per-day limit), `lock` (manual and scheduled runs cannot
overlap), `http.fetch` (the model API) and `cron`.

Worth noting in the port:

- The image model is asked for a **data URL** rather than a hosted link, so the
  bytes arrive already base64-encoded and go straight to `bkn.files.put`.
- A failed image does not lose the article: the cover step is caught, a
  `blog/image.failed` warning is emitted, and the post ships without one.
- The slug **is** the record id, so `store.get` before writing is how slug
  collisions are resolved.
- Every exit path releases the lock, including the failure path.

## stripe-webhook

Verifies the signature, deduplicates retries, and updates billing state.

```sh
bkn kv set stripe.webhook_secret whsec_... --type encrypted
bkn script create stripe-webhook --file stripe-webhook/stripe-webhook.js
bkn hooks create stripe --script stripe-webhook
# point Stripe at https://your-host/v1/hooks/stripe
```

Worth noting in the port:

- The signature covers `"<timestamp>.<raw body>"`, so the body must arrive
  byte-for-byte — which is why hook deliveries are never parsed before the
  script sees them.
- Timestamps outside a 5 minute window are rejected, so a captured request is
  not replayable forever.
- `bkn.crypto.equal` is used, never `===`: a plain comparison leaks the correct
  prefix through timing.
- Retries are handled by `store.putIfAbsent` on the Stripe event id. A
  get-then-put has a window a retry will eventually find.
- A handler that throws answers 500 so Stripe retries, rather than swallowing
  the delivery.
- **Billing state lives in `billing/subjects`, not on the user record.** Putting
  it on the user is exactly what drove a consumer of the old system to bypass
  the API and write MongoDB directly.

## i18n

Translation bundles, with a missing-key queue.

```sh
bkn store put i18n/locales --id en --data '{"name":"English","default":true}'
bkn script run i18n-import --input '{"locale":"fr","entries":{"nav":{"home":"Accueil"}}}'
bkn hooks create i18n --script i18n --allow-origin https://your-site.example --rate-limit 300
```

Worth noting in the port:

- `Accept-Language` is negotiated with its **q-weights**, so
  `de;q=1.0,fr;q=0.5` picks French when German is not available, and `fr-CA`
  falls back to `fr` before moving on to the next preference.
- A key present in the default locale but missing in the requested one renders
  the default. A half-translated site beats a broken one.
- A key that resolves nowhere is written to `i18n/missing` with `putIfAbsent`,
  turning translation from a guessing game into a work queue — and a later
  import clears it.
- The bundle carries an **ETag** derived from its content hash, so a client
  that already has it gets a 304.
- Imports flatten nested JSON (`nav.home`) and do not overwrite existing
  values unless told to, so an import cannot silently undo an admin
  correction.

## redirects

bkn does not serve pages, so this resolves a path and answers with a real
`301`/`302` an edge proxy can follow — or with JSON for a caller that would
rather decide itself.

```sh
bkn hooks create redirects --script redirects --rate-limit 600
./add-redirect.sh /old-page /new-page 301

curl -sI "https://host/v1/hooks/redirects?path=/old-page"    # 301, Location: /new-page
curl -s  "https://host/v1/hooks/redirects?path=/old-page&as=json"
```

Worth noting in the port:

- The rule id is the **hash of the normalised path**, so the common case is one
  indexed lookup rather than a scan over every rule.
- Prefix rules are scanned, but the **longest match wins**: `/docs/v1` beats
  `/docs`.
- The incoming query string is carried across, because dropping `?utm_source`
  silently loses attribution.
- A permanent redirect is cached for an hour and a temporary one is not; a miss
  is cached briefly, since a rule can appear at any time.

## feature-flags

Evaluation only — the flags themselves need no storage code, because they are
`kv` entries and the admin interface is `bkn kv set`:

```sh
bkn kv set flag.new-checkout --type json \
  '{"enabled":false,"public":true,"rollout_percentage":30,"payload":{"variant":"b"}}'
bkn hooks create flags --script feature-flags --rate-limit 300
```

Worth noting in the port:

- Precedence is deny → allow → on → rollout. **Deny wins over allow**: a kill
  switch an allow-list can override is not a kill switch.
- The bucket is `sha256(flag + ":" + subject)`, so it is stable for a subject
  across restarts and **independent per flag** — otherwise everyone in the
  first 10% of one rollout is in the first 10% of every rollout. Measured over
  300 subjects, two 30% flags agreed 54.7% of the time (identical bucketing
  would be 100%, independent ≈58%).
- Bucketing prefers the org over the user, which keeps a whole team on the same
  side of a rollout and makes a partial release demoable.
- An anonymous caller sees only flags marked `public`; a private flag leaks the
  shape of unreleased work.
- An invalid or expired token is treated as an anonymous visitor. A flag check
  should never be the thing that breaks a page.

## configs

Public JSON documents addressed by slug or by a stable alias.

```sh
bkn script run config-save --input '{"title":"Pricing Table 2026","value":{...},"public":true,"alias":"pricing"}'
bkn hooks create configs --script configs --rate-limit 300

curl "https://host/v1/hooks/configs?alias=pricing&raw=1"
```

Worth noting in the port:

- Roughly half the Node service was a hand-rolled per-slug TTL cache. That is
  gone: the stored content hash is the **ETag** and the config's own
  `cache_ttl_seconds` becomes `Cache-Control`, so caching is shared by every
  client instead of living in one server's memory.
- The slug **is** the document id, so uniqueness comes from the store rather
  than from a check a concurrent save could slip past.
- An alias is claimed with `putIfAbsent`, so it can never point at two
  documents at once.
- A private config answers 404, exactly like a missing one.

## headless

Schema-driven CRUD over user-defined content models — the domain that finally
pushed back on the core.

```sh
bkn script run headless-model --input @article-model.json
bkn script run headless-model --input '{"token":{"name":"website","permissions":{"articles":["read"]}}}'
bkn hooks create cms --script headless --rate-limit 600

curl -H "X-Api-Token: hl_..." \
  "https://host/v1/hooks/cms?model=articles&status=live&order_by=published_at&populate=author"
```

Worth noting in the port:

- **Auto-migration disappears.** Roughly 90 lines of the Node service kept a
  Mongoose schema in step with a changing model definition. The store is
  schemaless, so there is nothing to migrate — a new field is simply absent on
  old records. `headless-model` warns about a changed type or a newly-unique
  field, which is the part that actually needs a human.
- Validation lives in the script — type, `required`, `default`, `min`/`max`,
  `minLength`/`maxLength`, `enum`, regex `match`. Unknown keys are dropped
  rather than rejected, so adding a field to a client before adding it to the
  model does not start failing writes.
- **Uniqueness beyond the id** uses a companion collection whose id is the
  hashed value, claimed with `putIfAbsent`. The claim is released on rename and
  on delete, so a freed value becomes available again.
- API tokens are stored **only as hashes**, and the hash is the record id — so
  authenticating is one indexed lookup, the plaintext never touches the
  database, and there is no comparison to leak timing.
- `populate` resolves references with one query per referenced model
  (`id:in=...`), not one per record.

## Found by dogfooding these live

Running the suite in [`test/`](../test) against the deployed instance found a
real defect in `headless.js` that local testing had not: a `PATCH` with no
`id` fell through to the create path, and because `PATCH` is partial, the
required-field check was skipped — so a mistyped update silently created a
record with neither of its two required fields.

Two such records existed on the live server before it was caught. The fix is
in two parts: `PUT`/`PATCH` without an `id` is now a 400 (an update with
nothing to update is a client error), and partial validation applies only when
a record is actually being updated, since there is nothing to fall back to on
a create.

Worth recording how it surfaced. The suite failed on its *second* run because
the tests were not re-runnable, and the noise from that nearly buried the real
signal. Making the suites idempotent is what separated "my test is stateful"
from "the code accepts invalid writes".

## Testing them without the real services

Both were developed against local mocks. `bkn hooks test` replays a delivery
with arbitrary headers, so a signature check can be built without a public URL
or a provider's retry schedule:

```sh
bkn hooks test stripe --body @sample.json --header "stripe-signature=t=...,v1=..."
```

## forms

Public form submissions, with the definition stored as data so a new form needs
a `store put` rather than a deploy.

```sh
bkn store put forms/definitions --id waitlist --data @waitlist-form.json
bkn script create forms --file forms.js
bkn hooks create forms --script forms \
  --allow-origin https://your-site.example --rate-limit 10
```

`GET /v1/hooks/forms?form=waitlist` returns the field list so a page can render
itself; `POST` validates and stores a submission.

Worth noting in the port:

- A **honeypot** field answers `200` and stores nothing, so a bot believes it
  succeeded and does not adapt.
- `dedupe_field` makes "sign me up" idempotent: the address is normalised,
  hashed into the record id, and written with `putIfAbsent`, so a double
  submission is one entry decided atomically.
- Email notification is best-effort — the submission is already stored, and
  losing it because a mail provider is down would be the worse failure.

## waitlist-export

Password-protected CSV or JSON exports of submissions.

```sh
bkn kv set exports.waitlist_password "a-long-shared-secret" --type encrypted
bkn store put forms/exports --id waitlist --data @export-config.json
bkn hooks create exports --script waitlist-export --rate-limit 30

curl "https://host/v1/hooks/exports?name=waitlist&password=..." -o waitlist.csv
```

Worth noting in the port:

- The shared secret lives in an **encrypted kv entry**, not as a hash on the
  config. It is a password to compare, not a user credential to verify, and
  keeping it out of the database entirely beats hashing it in.
- CSV quoting follows RFC 4180 and is verified against Python's `csv` module in
  the walkthrough — a stray comma in a free-text field silently shifts every
  later column otherwise.
- The export streams in pages of 500 rather than loading everything at once.
- Access, granted and denied, lands in the `exports` event stream; no separate
  access-log collection is needed.
