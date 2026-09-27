// GET with bearer — returns the caller's identity + portal role (the role lives in
// portal/access, not in the JWT, so the page asks us rather than guessing).
function caller(d) {
  const h = d.headers["authorization"] || d.headers["Authorization"] || "";
  const claims = bkn.auth.verify(h.slice(7));
  if (!claims) return null;
  const acc = bkn.store.find("portal/access", { email: claims.email });
  if (!acc) return null;
  return { email: claims.email, role: acc.role, org_role: claims.org_role || "" };
}
function main(d) {
  const c = caller(d);
  if (!c) return { status: 401, body: { error: "auth required" } };
  return { status: 200, body: c };
}
