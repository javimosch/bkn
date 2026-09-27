// POST {email, password} — password sign-in via bkn.auth.login (bcrypt verify +
// org membership + disabled check in core). The hook stays public; a failed login
// returns 401 with the same message shape as the core route, and every attempt is
// audited so login.denied storms are visible in `bkn events`.
function main(d) {
  let p;
  try { p = JSON.parse(d.body || "{}"); } catch (e) { return { status: 400, body: { error: "json required" } }; }
  const email = String(p.email || "").trim().toLowerCase();
  const tokens = bkn.auth.login(email, String(p.password || ""), "portal");
  if (!tokens) {
    bkn.events.emit("portal", "login.denied", { subject: email });
    return { status: 401, body: { error: "bad_credentials" } };
  }
  bkn.events.emit("portal", "login.password", { subject: email });
  return { status: 200, body: { tokens: tokens } };
}
