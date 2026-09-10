// Drive upload: the one drive operation that cannot be a plain script run.
//
// A script run's body is capped at 4MB by the server and base64 inflates by a
// third, so uploads arrive through a hook instead, whose max_bytes is
// configurable per hook. The cost of that choice is that hooks are
// deliberately unauthenticated -- bkn.caller is anonymous here -- so this
// script authenticates the caller itself with bkn.auth.verify and then
// re-implements nothing else: it reuses the same collections, the same path
// claims and the same quota reservation as drive.js.
//
//   POST /v1/hooks/drive-upload
//   Authorization: Bearer <user access token>
//   { "drive": "user:me", "path": "/reports", "name": "q3.pdf",
//     "content_base64": "...", "content_type": "application/pdf" }
//
// Install:
//   bkn script create drive-upload --file drive-upload.js
//   bkn hooks create drive-upload --script drive-upload --max-bytes 26214400 --rate-limit 60

const ENTRIES = "drive/entries";
const PATHS = "drive/u-path";
const USAGE = "drive/usage";
const POLICY = "drive/policy";
const GROUPS = "drive/groups";
const MEMBERS = "drive/group_members";
const BLOBS = "drive-blobs";

const DEFAULT_MAX_UPLOAD = 1073741824;
const DEFAULT_MAX_STORAGE = 104857600;

function reply(status, body) {
  return { status: status, body: body };
}

function bad(status, error, field) {
  const body = { ok: false, error: error };
  if (field) body.field = field;
  return reply(status, body);
}

function authenticate(d) {
  const header = d.headers["authorization"] || d.headers["Authorization"] || "";
  const at = header.indexOf(" ");
  if (at < 0 || header.slice(0, at).toLowerCase() !== "bearer") return null;
  const claims = bkn.auth.verify(header.slice(at + 1));
  if (!claims || !claims.sub) return null;
  return claims;
}

function positive(v) {
  const n = Number(v);
  if (!isFinite(n) || n <= 0) return null;
  return Math.floor(n);
}

function parseDrive(spec, c) {
  const raw = String(spec || "").trim();
  const at = raw.indexOf(":");
  if (at < 1) return null;
  const type = raw.slice(0, at);
  let id = raw.slice(at + 1);
  if (type !== "user" && type !== "group" && type !== "org") return null;
  if (type === "user" && (id === "me" || id === "")) id = c.sub;
  if (!id) return null;
  return { type: type, id: id, key: type + ":" + id };
}

function canWrite(drive, c) {
  if (c.role === "admin") return true;
  if (drive.type === "user") return drive.id === c.sub;
  if (drive.type === "group") {
    const m = bkn.store.get(MEMBERS, drive.id + ":" + c.sub);
    return !!m && m.role !== "reader";
  }
  return bkn.auth.can(c.sub, drive.id, "admin");
}

function groupsOf(user) {
  const rows = bkn.store.list(MEMBERS, { where: { user: user }, limit: 200 });
  const out = [];
  for (let i = 0; i < rows.length; i++) out.push(rows[i].group);
  return out;
}

function effectiveLimits(drive, c) {
  const chain = [];
  if (drive.type === "user") {
    chain.push("user:" + drive.id);
    const gs = groupsOf(drive.id);
    for (let i = 0; i < gs.length; i++) chain.push("group:" + gs[i]);
    if (c.org) chain.push("org:" + c.org);
  } else if (drive.type === "group") {
    chain.push("group:" + drive.id);
    const g = bkn.store.get(GROUPS, drive.id);
    if (g && g.org) chain.push("org:" + g.org);
  } else {
    chain.push("org:" + drive.id);
  }
  chain.push("global");

  let upload = null, storage = null;
  for (let i = 0; i < chain.length; i++) {
    const p = bkn.store.get(POLICY, chain[i]) || {};
    if (upload === null) upload = positive(p.max_upload_bytes);
    if (storage === null) storage = positive(p.max_storage_bytes);
  }
  return {
    max_upload_bytes: upload === null ? DEFAULT_MAX_UPLOAD : upload,
    max_storage_bytes: storage === null ? DEFAULT_MAX_STORAGE : storage
  };
}

function normalizePath(p) {
  let s = String(p === undefined || p === null ? "/" : p).trim();
  if (s === "") s = "/";
  if (s.charAt(0) !== "/") s = "/" + s;
  const parts = [], raw = s.split("/");
  for (let i = 0; i < raw.length; i++) {
    const seg = raw[i];
    if (seg === "" || seg === ".") continue;
    if (seg === "..") return null;
    parts.push(seg);
  }
  return "/" + parts.join("/");
}

function pathKey(drive, parent, name) {
  return bkn.crypto.hash(drive.key + "|" + parent + "|" + name).slice(0, 32);
}

// base64 encodes 3 bytes as 4 characters; padding tells us how many of the
// last three are real. Measuring here means the quota is charged the size the
// blob will actually occupy, before anything is written.
function decodedSize(b64) {
  const s = String(b64 || "");
  if (s.length === 0) return 0;
  if (s.length % 4 !== 0) return -1;
  let pad = 0;
  if (s.charAt(s.length - 1) === "=") pad++;
  if (s.charAt(s.length - 2) === "=") pad++;
  return (s.length / 4) * 3 - pad;
}

function main(d) {
  if (d.method !== "POST" && d.method !== "PUT") {
    return bad(405, "upload with POST");
  }
  const c = authenticate(d);
  if (!c) return bad(401, "a valid bearer token is required");

  let body;
  try {
    body = JSON.parse(d.body || "{}");
  } catch (e) {
    return bad(400, "body must be JSON");
  }

  const drive = parseDrive(body.drive, c);
  if (!drive) return bad(400, 'drive must look like "user:me", "group:<id>" or "org:<slug>"', "drive");
  if (!canWrite(drive, c)) return bad(403, "no write access to drive " + drive.key);

  const parent = normalizePath(body.path);
  if (parent === null) return bad(400, "path segments may not be ..", "path");

  const name = String(body.name || "").trim();
  if (!name || name.indexOf("/") >= 0 || name === "." || name === "..") {
    return bad(400, "name is required and may not contain /", "name");
  }
  if (name.length > 255) return bad(400, "name is longer than 255 characters", "name");

  const size = decodedSize(body.content_base64);
  if (size < 0) return bad(400, "content_base64 is not valid base64", "content_base64");

  const limits = effectiveLimits(drive, c);
  if (size > limits.max_upload_bytes) {
    return bad(413, "file is " + size + " bytes, over the " + limits.max_upload_bytes +
                    " byte per-upload limit", "content_base64");
  }

  // The parent folder must exist, so a typo produces an error rather than a
  // file stranded in a folder nobody lists.
  if (parent !== "/") {
    const at = parent.lastIndexOf("/");
    const pp = at <= 0 ? "/" : parent.slice(0, at);
    const pn = parent.slice(at + 1);
    const claim = bkn.store.get(PATHS, pathKey(drive, pp, pn));
    if (!claim) return bad(404, "parent folder " + parent + " does not exist", "path");
  }

  const id = bkn.id();
  const key = pathKey(drive, parent, name);
  const won = bkn.store.putIfAbsent(PATHS, {
    drive: drive.key, parent: parent, name: name, entry: id
  }, key);
  if (!won) {
    return bad(409, (parent === "/" ? "/" + name : parent + "/" + name) +
                    " already exists in " + drive.key);
  }

  // Reserve before writing: $inc is atomic, so two uploads racing for the last
  // free bytes cannot both win, and the loser hands its reservation back.
  bkn.store.putIfAbsent(USAGE, { used_bytes: 0, files: 0, drive: drive.key }, drive.key);
  const after = bkn.store.patch(USAGE, drive.key, {
    used_bytes: { $inc: size }, files: { $inc: 1 }
  });
  if (Number(after.used_bytes) > limits.max_storage_bytes) {
    bkn.store.patch(USAGE, drive.key, { used_bytes: { $inc: -size }, files: { $inc: -1 } });
    bkn.store.delete(PATHS, key);
    return bad(413, "drive quota exceeded: " + limits.max_storage_bytes + " bytes, " +
                    (Number(after.used_bytes) - size) + " already used, " + size + " more requested");
  }

  // A bkn file name is flat: no path separators. The drive and path live in
  // the entry and in the blob's metadata, so the blob name only has to be
  // unique and traceable back to its entry.
  const blob = drive.key.replace(":", "-") + "-" + id;
  try {
    bkn.files.put(BLOBS, blob, body.content_base64, {
      encoding: "base64",
      contentType: String(body.content_type || ""),
      metadata: { drive: drive.key, path: parent, name: name, owner: c.sub }
    });
  } catch (e) {
    // Give everything back, in the reverse order it was taken.
    bkn.store.patch(USAGE, drive.key, { used_bytes: { $inc: -size }, files: { $inc: -1 } });
    bkn.store.delete(PATHS, key);
    return bad(500, "storing the file failed: " + e);
  }

  bkn.store.put(ENTRIES, {
    drive: drive.key, drive_type: drive.type, drive_id: drive.id,
    parent_path: parent, name: name, kind: "file",
    blob: blob, size: size, content_type: String(body.content_type || ""),
    owner: c.sub, visibility: body.visibility === "public" ? "public" : "private",
    deleted: "", path_key: key,
    created_at: bkn.now(), updated_at: bkn.now()
  }, id);

  return reply(201, {
    ok: true,
    entry: {
      id: id, drive: drive.key, path: parent === "/" ? "/" + name : parent + "/" + name,
      name: name, size: size, content_type: String(body.content_type || "")
    },
    usage: { used_bytes: Number(after.used_bytes), max_storage_bytes: limits.max_storage_bytes }
  });
}
