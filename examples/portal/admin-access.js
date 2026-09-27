// POST {email} grants viewer access; DELETE {email} revokes it. Admin only.
// Revoke deletes the access entry AND disables the auth user so issued sessions die
// at the next verify — matching the Flask behavior "retirer un accès désactive le compte".
function caller(d) {
  const h = d.headers["authorization"] || d.headers["Authorization"] || "";
  const claims = bkn.auth.verify(h.slice(7));
  if (!claims) return null;
  const acc = bkn.store.find("portal/access", { email: claims.email });
  return acc && acc.role === "admin" ? claims : null;
}
function main(d) {
  const admin = caller(d);
  if (!admin) return { status: 403, body: { error: "admin only" } };
  let p = {};
  try { p = JSON.parse(d.body || "{}"); } catch (e) {}
  const email = String(p.email || "").trim().toLowerCase();
  if (!email) return { status: 400, body: { error: "email required" } };
  if (d.method === "POST") {
    const id = email;
    const created = bkn.store.putIfAbsent("portal/access", {
      email: email, role: "viewer", added_by: admin.email, added_at: bkn.now()
    }, id);
    bkn.events.emit("portal", "access.granted", { subject: email, data: { by: admin.email } });
    return { status: 200, body: { ok: true, created: !!created } };
  }
  if (d.method === "DELETE") {
    const acc = bkn.store.find("portal/access", { email: email });
    if (!acc) return { status: 404, body: { error: "not on the list" } };
    if (acc.role === "admin") return { status: 400, body: { error: "cannot revoke an admin" } };
    bkn.store.delete("portal/access", acc.id);
    const u = bkn.auth.findUser(email);
    if (u) bkn.auth.updateUser(email, { disabled: true });
    bkn.events.emit("portal", "access.revoked", { subject: email, data: { by: admin.email } });
    return { status: 200, body: { ok: true, revoked_sessions: !!u } };
  }
  return { status: 405, body: { error: "method not allowed" } };
}
