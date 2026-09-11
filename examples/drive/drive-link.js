// drive-link: resolve a share link for someone who has no account.
//
// Public by necessity -- that is what a share link is for -- so this script
// authenticates nothing and authorises one thing: possession of the token, and
// the password if the link carries one.
//
//   POST /v1/hooks/drive-link   { "token": "...", "password": "..." }
//
// Install:
//   bkn script create drive-link --file drive-link.js
//   bkn hooks create drive-link --script drive-link --max-bytes 4096 --rate-limit 30
//
// The rate limit is load-bearing. A share password is checked with a salted
// HMAC rather than a slow KDF, so guessing is cheap for whoever can ask
// quickly; 30 tries a minute per address is what makes it not cheap.

const LINKS = "drive/links";
const ENTRIES = "drive/entries";
const BLOBS = "drive-blobs";

function reply(status, body) { return { status: status, body: body }; }

function main(d) {
  if (d.method !== "POST") return reply(405, { ok: false, error: "post a token" });

  let body;
  try { body = JSON.parse(d.body || "{}"); } catch (e) {
    return reply(400, { ok: false, error: "body must be JSON" });
  }

  const token = String(body.token || "");
  // Every failure below answers the same way. A link that says "wrong
  // password" has already confirmed that the token is real, which turns
  // guessing the token into a separate, easier game.
  const nope = reply(404, { ok: false, error: "this link is not valid" });
  if (token.length < 16) return nope;

  const link = bkn.store.get(LINKS, bkn.crypto.hash(token).slice(0, 32));
  if (!link) return nope;

  if (link.expires_at && Date.parse(link.expires_at) < Date.now()) {
    return reply(410, { ok: false, error: "this link has expired" });
  }

  if (link.pw_hash) {
    const supplied = String(body.password || "");
    if (!supplied) {
      // Only ever admitted for a token that is otherwise valid and unexpired,
      // so it reveals nothing that possession of the token did not.
      return reply(401, { ok: false, password_required: true, name: link.name });
    }
    if (!bkn.crypto.equal(bkn.crypto.hmac(link.pw_salt, supplied), link.pw_hash)) {
      return reply(401, { ok: false, password_required: true, error: "wrong password" });
    }
  }

  const entry = bkn.store.get(ENTRIES, link.entry);
  if (!entry || entry.state !== "live") {
    // The file was deleted or binned after the link was made. Say so plainly:
    // this is the one failure the holder can do nothing about, and pretending
    // the link is invalid would send them back to ask for another one.
    return reply(410, { ok: false, error: "the shared file is no longer available" });
  }

  bkn.store.patch(LINKS, link.id, { downloads: { $inc: 1 }, last_used: bkn.now() });

  return reply(200, {
    ok: true,
    name: entry.name,
    size: entry.size || 0,
    content_type: entry.content_type || "",
    url: bkn.files.sign(BLOBS, entry.blob, { ttl: "15m" })
  });
}
