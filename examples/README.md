# Examples — domains ported from the Node backend

These are the proof that the core is the right size: two features that were
whole admin domains in the 85k-line Node backend, running as scripts with no
Go code added for either.

| Domain | Node | bkn |
|---|---|---|
| Blog automation | ~2,500 lines across services, controllers, routes and 3 Mongoose models | [`blog-automation.js`](blog-automation/blog-automation.js), 234 lines |
| Stripe webhook | ~1,500 lines across services, controllers, routes and 2 models | [`stripe-webhook.js`](stripe-webhook/stripe-webhook.js), 159 lines |

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

## Testing them without the real services

Both were developed against local mocks. `bkn hooks test` replays a delivery
with arbitrary headers, so a signature check can be built without a public URL
or a provider's retry schedule:

```sh
bkn hooks test stripe --body @sample.json --header "stripe-signature=t=...,v1=..."
```
