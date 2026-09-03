// Public form submissions.
//
// Replaces forms.service + forms.controller + the FormSubmission model from
// the Node backend with one script behind a browser-reachable hook.
//
// Install:
//   bkn store put forms/definitions --id contact --data @contact-form.json
//   bkn script create forms --file forms.js
//   bkn hooks create forms --script forms \
//     --allow-origin https://your-site.example --rate-limit 10

const SUBMISSIONS = "forms/submissions";

function bad(message, field) {
  return { status: 400, body: { ok: false, error: message, field: field } };
}

function validEmail(value) {
  return /^[^@\s]+@[^@\s.]+\.[^@\s]+$/.test(String(value || "").trim());
}

// The definition is data, so a new form needs no deploy - only a store write.
function publicDefinition(def, key) {
  return {
    form: key,
    title: def.title || key,
    fields: (def.fields || []).map((f) => ({
      name: f.name, label: f.label || f.name, type: f.type || "text",
      required: Boolean(f.required), max: f.max || null,
      options: f.options || null,
    })),
  };
}

function validate(def, submitted) {
  const clean = {};
  for (const field of def.fields || []) {
    const raw = submitted[field.name];
    const value = typeof raw === "string" ? raw.trim() : raw;

    if (field.required && (value === undefined || value === null || value === "")) {
      return { error: bad("this field is required", field.name) };
    }
    if (value === undefined || value === null || value === "") continue;

    if (field.type === "email" && !validEmail(value)) {
      return { error: bad("not a valid email address", field.name) };
    }
    if (field.max && String(value).length > field.max) {
      return { error: bad("must be at most " + field.max + " characters", field.name) };
    }
    if (field.options && field.options.indexOf(value) === -1) {
      return { error: bad("not one of the allowed values", field.name) };
    }
    clean[field.name] = field.type === "email" ? String(value).toLowerCase() : value;
  }
  return { fields: clean };
}

function main(d) {
  // GET returns the definition so a page can render the form from data.
  if (d.method === "GET") {
    const key = d.query.form;
    if (!key) return bad("pass ?form=<key>");
    const def = bkn.store.get("forms/definitions", key);
    if (!def || def.enabled === false) return { status: 404, body: { error: "no such form" } };
    return { status: 200, body: publicDefinition(def, key) };
  }

  let payload;
  try {
    payload = JSON.parse(d.body || "{}");
  } catch (e) {
    return bad("body must be JSON");
  }

  const key = payload.form || d.query.form;
  if (!key) return bad("form is required");
  const def = bkn.store.get("forms/definitions", key);
  if (!def || def.enabled === false) return { status: 404, body: { error: "no such form" } };

  const submitted = payload.fields || payload;

  // A honeypot field is invisible to a person and irresistible to a bot.
  // Answer 200 so the bot believes it succeeded and does not retry or adapt.
  if (def.honeypot && String(submitted[def.honeypot] || "").trim() !== "") {
    bkn.events.emit("forms", "submission.spam", {
      level: "warn", subject: key, data: { reason: "honeypot" },
    });
    return { status: 200, body: { ok: true } };
  }

  const checked = validate(def, submitted);
  if (checked.error) {
    bkn.events.emit("forms", "submission.rejected", {
      subject: key, data: { field: checked.error.body.field },
    });
    return checked.error;
  }

  const record = {
    form: key,
    fields: checked.fields,
    ip: d.headers["x-forwarded-for"] || null,
    user_agent: d.headers["user-agent"] || null,
    referer: d.headers["referer"] || null,
    submitted_at: bkn.now(),
  };

  // A dedupe field turns "sign me up" into an idempotent operation: the same
  // address submitted twice is one entry, decided atomically rather than by a
  // read that a second submission can race.
  if (def.dedupe_field && checked.fields[def.dedupe_field]) {
    const id = key + "-" + bkn.crypto.hash(String(checked.fields[def.dedupe_field])).slice(0, 24);
    const created = bkn.store.putIfAbsent(SUBMISSIONS, record, id);
    if (created === null) {
      bkn.events.emit("forms", "submission.duplicate", { subject: key });
      return { status: 200, body: { ok: true, duplicate: true } };
    }
    notify(def, key, checked.fields);
    bkn.events.emit("forms", "submission.received", { subject: key, data: { id: id } });
    return { status: 200, body: { ok: true, id: id } };
  }

  const stored = bkn.store.put(SUBMISSIONS, record);
  notify(def, key, checked.fields);
  bkn.events.emit("forms", "submission.received", { subject: key, data: { id: stored.id } });
  return { status: 200, body: { ok: true, id: stored.id } };
}

// Notification is best-effort: a form submission is already safely stored, and
// losing it because an email provider is down would be the worse failure.
function notify(def, key, fields) {
  if (!def.notify || !def.notify.enabled) return;
  try {
    const apiKey = bkn.kv.get(def.notify.api_key_setting || "forms.email_key");
    if (!apiKey) throw new Error("no email key configured");
    const lines = Object.keys(fields).map((k) => k + ": " + fields[k]).join("\n");
    const res = bkn.http.fetch("https://api.resend.com/emails", {
      method: "POST",
      headers: { "Authorization": "Bearer " + apiKey, "Content-Type": "application/json" },
      body: {
        from: def.notify.from, to: [def.notify.to],
        subject: "New " + key + " submission",
        text: lines,
      },
    });
    if (!res.ok) throw new Error("HTTP " + res.status);
  } catch (err) {
    bkn.events.emit("forms", "notify.failed", {
      level: "warn", subject: key, data: { error: String(err && err.message || err) },
    });
  }
}
