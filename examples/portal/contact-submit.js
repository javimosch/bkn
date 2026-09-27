// POST {name,email,message} — the contact form. Guests may write, nobody reads without a
// session (the book itself is only exposed through admin-contacts). Idempotent on
// email+message so retries don't duplicate rows.
function main(d) {
  let p;
  try { p = JSON.parse(d.body || "{}"); } catch (e) { return { status: 400, body: { error: "json required" } }; }
  const f = {
    name: String(p.name || "").slice(0, 120),
    email: String(p.email || "").trim().toLowerCase().slice(0, 200),
    message: String(p.message || "").slice(0, 5000)
  };
  if (!f.name || !f.email) return { status: 400, body: { error: "name and email required" } };
  const id = f.email + "-" + bkn.crypto.hash(String(f.message)).slice(0, 16);
  const created = bkn.store.putIfAbsent("portal/contacts", {
    name: f.name, email: f.email, message: f.message,
    submitted_at: bkn.now(), ip: d.headers["x-forwarded-for"] || null
  }, id);
  if (!created) return { status: 200, body: { ok: true, duplicate: true } };
  bkn.events.emit("portal", "contact.received", { subject: f.email });
  return { status: 200, body: { ok: true, id: created.id } };
}
