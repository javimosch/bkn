// GET with bearer — the address book, admin only. The bearer is verified inside the script:
// hooks are unauthenticated routes by design, so the script IS the authorization boundary.
function caller(d) {
  const h = d.headers["authorization"] || d.headers["Authorization"] || "";
  const claims = bkn.auth.verify(h.slice(7));
  if (!claims) return null;
  const acc = bkn.store.find("portal/access", { email: claims.email });
  return acc ? { email: claims.email, role: acc.role } : null;
}
function main(d) {
  const c = caller(d);
  if (!c) return { status: 401, body: { error: "auth required" } };
  if (c.role !== "admin") return { status: 403, body: { error: "admin only" } };
  return { status: 200, body: { contacts: bkn.store.list("portal/contacts", { limit: 100 }) } };
}
