// Feature flag evaluation.
//
// Replaces featureFlags.service + its controller from the Node backend. The
// flags themselves need no storage code at all: they are kv entries, so the
// admin interface is `bkn kv set`.
//
//   bkn kv set flag.new-checkout --type json --data ... --public
//
//   GET /v1/hooks/flags                      -> public flags for an anonymous visitor
//   GET /v1/hooks/flags?anon=<id>            -> stable bucketing for that visitor
//   GET /v1/hooks/flags  + Bearer <token>    -> evaluated for the signed-in user and org
//
// Install:
//   bkn script create feature-flags --file feature-flags.js
//   bkn hooks create flags --script feature-flags \
//     --allow-origin https://your-site.example --rate-limit 300

const PREFIX = "flag.";

function normalizeList(value) {
  if (!Array.isArray(value)) return [];
  return value.map(String);
}

function definition(key, raw) {
  let rollout = Number(raw.rollout_percentage);
  if (!isFinite(rollout)) rollout = 0;
  rollout = Math.max(0, Math.min(100, rollout));
  return {
    key: key,
    description: String(raw.description || ""),
    enabled: Boolean(raw.enabled),
    public: Boolean(raw.public),
    rollout_percentage: rollout,
    allow_users: normalizeList(raw.allow_users),
    allow_orgs: normalizeList(raw.allow_orgs),
    deny_users: normalizeList(raw.deny_users),
    deny_orgs: normalizeList(raw.deny_orgs),
    payload: raw.payload === undefined ? null : raw.payload,
  };
}

// The bucket must be stable for a subject across requests and across restarts,
// and independent per flag - otherwise everyone in the first 10% of one
// rollout is in the first 10% of every rollout.
function bucket(key, subject) {
  const digest = bkn.crypto.hash(key + ":" + subject);
  return parseInt(digest.slice(0, 8), 16) % 100;
}

function evaluate(def, subject) {
  // Deny wins over everything, including an explicit allow: a kill switch that
  // an allow-list can override is not a kill switch.
  if ((subject.user && def.deny_users.indexOf(subject.user) >= 0) ||
      (subject.org && def.deny_orgs.indexOf(subject.org) >= 0)) {
    return { key: def.key, enabled: false, reason: "denied" };
  }
  if ((subject.user && def.allow_users.indexOf(subject.user) >= 0) ||
      (subject.org && def.allow_orgs.indexOf(subject.org) >= 0)) {
    return { key: def.key, enabled: true, payload: def.payload, reason: "allowed" };
  }
  if (def.enabled) {
    return { key: def.key, enabled: true, payload: def.payload, reason: "on" };
  }
  if (def.rollout_percentage > 0) {
    // Bucketing by org before user keeps a whole team on the same side of a
    // rollout, which is what makes a partial release demoable.
    const id = subject.org || subject.user || subject.anon;
    if (!id) return { key: def.key, enabled: false, reason: "no subject" };
    const slot = bucket(def.key, id);
    return slot < def.rollout_percentage
      ? { key: def.key, enabled: true, payload: def.payload, reason: "rollout" }
      : { key: def.key, enabled: false, reason: "rollout" };
  }
  return { key: def.key, enabled: false, reason: "off" };
}

function loadDefinitions() {
  const out = [];
  for (const entry of bkn.kv.list(PREFIX)) {
    if (entry.type !== "json") continue;
    try {
      out.push(definition(entry.key.slice(PREFIX.length), JSON.parse(entry.value)));
    } catch (e) {
      bkn.events.emit("flags", "definition.invalid", {
        level: "error", subject: entry.key, data: { error: String(e && e.message || e) },
      });
    }
  }
  return out;
}

function subjectFrom(d) {
  const header = d.headers["authorization"] || "";
  const subject = { user: null, org: null, anon: d.query.anon || null };
  if (header.indexOf("Bearer ") === 0) {
    // An invalid or expired token is an anonymous visitor, not an error: a
    // flag check should never be the thing that breaks a page.
    const claims = bkn.auth.verify(header.slice(7));
    if (claims) {
      subject.user = claims.sub;
      subject.org = claims.org || null;
    }
  }
  return subject;
}

function main(d) {
  if (d.method !== "GET") {
    return { status: 405, body: { error: "evaluate flags with GET" } };
  }
  const subject = subjectFrom(d);
  const anonymous = !subject.user && !subject.org;

  const flags = {};
  for (const def of loadDefinitions()) {
    // An anonymous caller sees only the flags marked public. A private flag
    // leaks the shape of unreleased work.
    if (anonymous && !def.public) continue;
    const result = evaluate(def, subject);
    flags[def.key] = d.query.explain === "1"
      ? result
      : (result.payload === null || result.payload === undefined
          ? result.enabled
          : { enabled: result.enabled, payload: result.payload });
  }

  return {
    status: 200,
    body: {
      flags: flags,
      subject: { user: subject.user, org: subject.org, anonymous: anonymous },
    },
    // Per-subject and short-lived: a rollout change should take effect in
    // seconds, and one visitor's answer must never be served to another.
    headers: { "Cache-Control": "private, max-age=30" },
  };
}
