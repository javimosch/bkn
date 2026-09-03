// Stripe webhook handler.
//
// Replaces src/controllers/billing.controller.js and the StripeWebhookEvent
// model from the Node backend (~1,500 lines across services, controllers,
// routes and models) with one script bound to a public hook.
//
// Install:
//   bkn kv set stripe.webhook_secret whsec_... --type encrypted
//   bkn script create stripe-webhook --file stripe-webhook.js
//   bkn hooks create stripe --script stripe-webhook
//   # point Stripe at https://your-host/v1/hooks/stripe

const SIGNATURE_TOLERANCE_SECONDS = 300;

function parseSignatureHeader(header) {
  const parts = { v1: [] };
  for (const chunk of String(header || "").split(",")) {
    const [key, value] = chunk.split("=");
    if (key === "t") parts.t = value;
    if (key === "v1") parts.v1.push(value);
  }
  return parts;
}

// Stripe signs "<timestamp>.<raw body>" with HMAC-SHA256. The body must be the
// exact bytes that arrived: re-serializing the JSON changes the signature.
function verify(delivery, secret) {
  const parts = parseSignatureHeader(delivery.headers["stripe-signature"]);
  if (!parts.t || parts.v1.length === 0) return "missing or malformed Stripe-Signature";

  // Without a timestamp check a captured request stays replayable forever.
  const age = Math.floor(Date.now() / 1000) - Number(parts.t);
  if (!Number.isFinite(age) || Math.abs(age) > SIGNATURE_TOLERANCE_SECONDS) {
    return "timestamp outside the tolerance window";
  }

  const expected = bkn.crypto.hmac(secret, parts.t + "." + delivery.body);
  // Stripe may send several signatures during a secret rotation; any match is
  // enough. Constant-time compare: === leaks the correct prefix.
  for (const candidate of parts.v1) {
    if (bkn.crypto.equal(expected, candidate)) return null;
  }
  return "signature mismatch";
}

function subjectFor(event) {
  const o = event.data && event.data.object ? event.data.object : {};
  return o.customer || o.id || event.id;
}

// Billing state lives in a store collection keyed by Stripe customer, NOT on
// the user record. Putting it on the user is what forced a consumer of the old
// system to bypass the API and write the database directly.
function upsertBilling(customerId, patch) {
  if (!customerId) return null;
  const existing = bkn.store.get("billing/subjects", customerId);
  if (!existing) {
    return bkn.store.put("billing/subjects", Object.assign(
      { stripe_customer_id: customerId, created_at: bkn.now() }, patch
    ), customerId);
  }
  return bkn.store.patch("billing/subjects", customerId, patch);
}

function linkUser(customerId, email) {
  if (!customerId || !email) return;
  const user = bkn.auth.findUser(email);
  if (user) upsertBilling(customerId, { user_id: user.id, email: user.email });
}

const handlers = {
  "checkout.session.completed": (o) => {
    linkUser(o.customer, o.customer_details && o.customer_details.email);
    return upsertBilling(o.customer, {
      status: "active",
      plan: (o.metadata && o.metadata.plan) || "unknown",
      checkout_session_id: o.id,
      updated_at: bkn.now(),
    });
  },
  "customer.subscription.created": (o) => upsertBilling(o.customer, {
    status: o.status, subscription_id: o.id,
    plan: o.items && o.items.data[0] && o.items.data[0].price ? o.items.data[0].price.id : "unknown",
    current_period_end: o.current_period_end, updated_at: bkn.now(),
  }),
  "customer.subscription.updated": (o) => upsertBilling(o.customer, {
    status: o.status, subscription_id: o.id,
    cancel_at_period_end: Boolean(o.cancel_at_period_end),
    current_period_end: o.current_period_end, updated_at: bkn.now(),
  }),
  "customer.subscription.deleted": (o) => upsertBilling(o.customer, {
    status: "canceled", subscription_id: o.id, canceled_at: bkn.now(), updated_at: bkn.now(),
  }),
  "invoice.payment_succeeded": (o) => upsertBilling(o.customer, {
    last_payment_status: "succeeded", last_invoice_id: o.id, updated_at: bkn.now(),
  }),
  "invoice.payment_failed": (o) => upsertBilling(o.customer, {
    last_payment_status: "failed", last_invoice_id: o.id,
    status: "past_due", updated_at: bkn.now(),
  }),
};

function main(delivery) {
  const secret = bkn.kv.get("stripe.webhook_secret");
  if (!secret) {
    bkn.events.emit("stripe", "webhook.misconfigured", { level: "error" });
    return { status: 500, body: { error: "stripe.webhook_secret is not configured" } };
  }

  const problem = verify(delivery, secret);
  if (problem) {
    bkn.events.emit("stripe", "webhook.rejected", {
      level: "warn", data: { reason: problem },
    });
    return { status: 400, body: { error: problem } };
  }

  const event = JSON.parse(delivery.body);

  // Stripe retries until it sees a 2xx, so the same event id arrives more than
  // once. putIfAbsent decides the race; a get-then-put would let a retry slip
  // through the gap.
  const claimed = bkn.store.putIfAbsent("stripe/events", {
    type: event.type, received_at: bkn.now(), status: "processing",
  }, event.id);
  if (claimed === null) {
    return { status: 200, body: { ok: true, duplicate: true, id: event.id } };
  }

  const handler = handlers[event.type];
  if (!handler) {
    bkn.store.patch("stripe/events", event.id, { status: "ignored" });
    bkn.events.emit("stripe", "webhook.ignored", {
      subject: event.type, data: { id: event.id },
    });
    return { status: 200, body: { ok: true, ignored: event.type } };
  }

  try {
    handler(event.data.object);
    bkn.store.patch("stripe/events", event.id, {
      status: "processed", processed_at: bkn.now(),
    });
    bkn.events.emit("stripe", "webhook.processed", {
      subject: event.type, data: { id: event.id, customer: subjectFor(event) },
    });
    return { status: 200, body: { ok: true, type: event.type } };
  } catch (err) {
    bkn.store.patch("stripe/events", event.id, {
      status: "failed", error: String(err && err.message || err),
    });
    bkn.events.emit("stripe", "webhook.failed", {
      level: "error", subject: event.type,
      data: { id: event.id, error: String(err && err.message || err) },
    });
    // A 500 makes Stripe retry, which is what we want for a transient failure.
    return { status: 500, body: { ok: false, error: "handler failed" } };
  }
}
