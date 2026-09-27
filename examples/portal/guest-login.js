// POST {email} — guest entry: whitelisted viewer emails get a real bkn session without a
// password (Michael's explicit guest-mode choice in the Flask app). Lazily provisions the
// auth user on first login; every entry is audited.
function main(d) {
  let p = {};
  try { p = JSON.parse(d.body || "{}"); } catch (e) {}
  const email = String(p.email || "").trim().toLowerCase();
  const acc = bkn.store.find("portal/access", { email: email });
  if (!acc || acc.role !== "viewer") {
    bkn.events.emit("portal", "login.denied", { subject: email });
    return { status: 403, body: { error: "not_authorized" } };
  }
  if (!bkn.auth.findUser(email)) {
    // password is a throwaway — guests never log in by password; this keeps the
    // auth record consistent with the createUser contract.
    bkn.auth.createUser(email, "guest-" + bkn.id(), { name: acc.name || email });
    bkn.auth.addMember("portal", email, "member");
  }
  const tokens = bkn.auth.issue(email, "portal");
  bkn.events.emit("portal", "login.guest", { subject: email });
  return { status: 200, body: { tokens: tokens, role: "viewer" } };
}
