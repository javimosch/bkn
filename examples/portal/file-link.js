// GET /v1/hooks/file-link?name=<file> with bearer — session check first (any authorized
// account, viewer or admin), then a short-lived signed URL the page can hand straight to a
// renderer (<model-viewer src>, <a>, img). The GLB never transits the script.
function main(d) {
  const h = d.headers["authorization"] || d.headers["Authorization"] || "";
  const claims = bkn.auth.verify(h.slice(7));
  if (!claims) return { status: 401, body: { error: "auth required" } };
  const acc = bkn.store.find("portal/access", { email: claims.email });
  if (!acc) return { status: 403, body: { error: "not_authorized" } };
  const name = String(d.query.name || "");
  if (!name || name.indexOf("..") >= 0 || name.indexOf("/") >= 0) {
    return { status: 400, body: { error: "bad name" } };
  }
  const f = bkn.files.show("portal-priv", name);
  if (!f) return { status: 404, body: { error: "not found" } };
  bkn.events.emit("portal", "file.served", { subject: name, data: { by: claims.email } });
  return { status: 200, body: { url: bkn.files.sign("portal-priv", name, { ttl: "5m" }), name: name, bytes: f.size } };
}
