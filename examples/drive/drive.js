// Shared drive: user, group and org drives over bkn's store and file storage.
//
// Replaces superbackend's fileManager module (FileEntry + Asset models,
// fileManager.service, fileManagerStoragePolicy.service and their
// controllers) with one script plus a companion upload hook.
//
//   bkn script run drive --input '{"op":"ls","drive":"user:me","path":"/"}'
//
// Quota policy (policy-set, policy-get) needs the ADMIN token, because bkn
// reports kind "admin" only for that token -- see isAdmin below.
//
// Every op is authorised against bkn.caller, so the same script serves an
// admin and an ordinary user without either seeing the other's drive.
//
// Install:
//   bkn script create drive --file drive.js --run-access user
//   bkn files namespace drive-blobs --private --sign
//   bkn script create drive-upload --file drive-upload.js
//   bkn hooks create drive-upload --script drive-upload --max-bytes 26214400

const ENTRIES = "drive/entries";
const PATHS = "drive/u-path"; // uniqueness companion, id = the path key
const USAGE = "drive/usage";
const POLICY = "drive/policy";
const GROUPS = "drive/groups";
const MEMBERS = "drive/group_members";
const SHARES = "drive/shares";
const BLOBS = "drive-blobs";

// How long a binned file waits before the nightly purge takes it.
const BIN_DAYS = 30;

// Entry state. bkn store filters are equality matches, so "not deleted" has to
// be a value rather than the absence of one.
const LIVE = "live";
const BINNED = "binned";

// Defaults matching superbackend's, so a migrated deployment behaves the same
// until a policy says otherwise.
const DEFAULT_MAX_UPLOAD = 1073741824; // 1 GiB
const DEFAULT_MAX_STORAGE = 104857600; // 100 MiB

function fail(error, field) {
  const body = { error: error };
  if (field) body.field = field;
  throw new Error(JSON.stringify(body));
}

// --- identity -------------------------------------------------------------

function caller() {
  const c = bkn.caller || {};
  // The admin token has no subject: it is an operator, not a person. Ops that
  // need a person say so when they resolve the drive.
  if (!isAdmin(c) && !c.sub) fail("this operation needs a signed-in caller");
  return c;
}

// bkn only reports kind "admin" for the static admin token (or a loopback
// call); a user whose ROLE is admin still arrives as kind "user", and the role
// is not exposed to scripts at all. So quota policy is deliberately an
// operator concern, reachable with the admin token rather than by any
// admin-role user. Org and group administration, which people really do need
// to perform, goes through bkn.auth.can instead.
function isAdmin(c) {
  // "system" is bkn's own scheduler running a cron. It is the server acting on
  // its own behalf, which for this domain is the same authority as the admin
  // token -- that is how the nightly purge gets to delete other people's
  // binned files.
  return c.kind === "admin" || c.kind === "system";
}

// Callers name people by email; bkn.caller identifies them by id. Storing the
// email would make every membership and share lookup miss, silently and only
// for the person it was granted to -- so resolve to the id at the boundary and
// keep exactly one identifier inside the domain.
function resolveUser(idOrEmail, field) {
  const raw = String(idOrEmail || "");
  if (!raw) fail((field || "user") + " is required", field || "user");
  const u = bkn.auth.findUser(raw);
  if (!u) fail("no such user: " + raw, field || "user");
  return u.id;
}

// --- drive addressing -----------------------------------------------------

// A drive is "<type>:<id>" -- user:<user-id>, group:<group-id>, org:<slug>.
// "user:me" resolves to the caller, which is what a client without its own
// user id in hand actually wants to say.
function parseDrive(spec, c) {
  const raw = String(spec || "").trim();
  if (!raw) fail("drive is required", "drive");
  const at = raw.indexOf(":");
  if (at < 1) fail('drive must look like "user:<id>", "group:<id>" or "org:<slug>"', "drive");

  const type = raw.slice(0, at);
  let id = raw.slice(at + 1);
  if (type !== "user" && type !== "group" && type !== "org") {
    fail("drive type must be user, group or org", "drive");
  }
  if (type === "user" && (id === "me" || id === "")) id = c.sub;
  if (!id) {
    if (type === "user") fail('"user:me" needs a signed-in user; name the user id explicitly', "drive");
    fail("drive id is required", "drive");
  }
  return { type: type, id: id, key: type + ":" + id };
}

// Read and write are separate questions: an org member may read the org drive
// while only an admin writes to it, and a group reader is not a group editor.
function access(drive, c) {
  if (isAdmin(c)) return "write";

  if (drive.type === "user") return drive.id === c.sub ? "write" : "none";

  if (drive.type === "group") {
    const m = bkn.store.get(MEMBERS, drive.id + ":" + c.sub);
    if (!m) return "none";
    return m.role === "reader" ? "read" : "write";
  }

  // org drive: membership is bkn's own, so there is one source of truth for
  // who is in an organisation rather than a second copy inside this domain.
  if (!c.org || c.org !== drive.id) {
    if (!bkn.auth.can(c.sub, drive.id, "member")) return "none";
  }
  return bkn.auth.can(c.sub, drive.id, "admin") ? "write" : "read";
}

function requireAccess(drive, c, need) {
  const have = access(drive, c);
  if (have === "none") fail("no access to drive " + drive.key);
  if (need === "write" && have !== "write") fail("read-only access to drive " + drive.key);
  return have;
}

// --- paths ----------------------------------------------------------------

function normalizePath(p) {
  let s = String(p === undefined || p === null ? "/" : p).trim();
  if (s === "") s = "/";
  if (s.charAt(0) !== "/") s = "/" + s;
  // collapse duplicate slashes and resolve nothing: "..' is rejected outright
  // rather than resolved, because a drive path is a key, not a filesystem walk.
  const parts = [];
  const raw = s.split("/");
  for (let i = 0; i < raw.length; i++) {
    const seg = raw[i];
    if (seg === "" || seg === ".") continue;
    if (seg === "..") fail("path segments may not be ..", "path");
    parts.push(seg);
  }
  return "/" + parts.join("/");
}

function checkName(name) {
  const n = String(name || "").trim();
  if (!n) fail("name is required", "name");
  if (n.indexOf("/") >= 0) fail("name may not contain /", "name");
  if (n === "." || n === "..") fail("name may not be . or ..", "name");
  if (n.length > 255) fail("name is longer than 255 characters", "name");
  return n;
}

function joinPath(parent, name) {
  return parent === "/" ? "/" + name : parent + "/" + name;
}

// The uniqueness key for (drive, parent, name). Hashed because a store id has
// a limited character set and a file name has none.
function pathKey(drive, parent, name) {
  return bkn.crypto.hash(drive.key + "|" + parent + "|" + name).slice(0, 32);
}

// --- quota ----------------------------------------------------------------

function policyFor(id) {
  const p = bkn.store.get(POLICY, id);
  return p || {};
}

function positive(v) {
  const n = Number(v);
  if (!isFinite(n) || n <= 0) return null;
  return Math.floor(n);
}

// Effective limits cascade most-specific first, exactly as superbackend's
// storage policy did: user beats group beats org beats global beats default.
// The source is reported so an operator can see WHICH rule bound them, which
// is the question actually asked when an upload is refused.
function effectiveLimits(drive, c) {
  const chain = [];
  if (drive.type === "user") {
    chain.push({ id: "user:" + drive.id, source: "user" });
    const groups = groupsOf(drive.id);
    for (let i = 0; i < groups.length; i++) chain.push({ id: "group:" + groups[i], source: "group" });
    if (c.org) chain.push({ id: "org:" + c.org, source: "org" });
  } else if (drive.type === "group") {
    chain.push({ id: "group:" + drive.id, source: "group" });
    const g = bkn.store.get(GROUPS, drive.id);
    if (g && g.org) chain.push({ id: "org:" + g.org, source: "org" });
  } else {
    chain.push({ id: "org:" + drive.id, source: "org" });
  }
  chain.push({ id: "global", source: "global" });

  let upload = null, storage = null;
  const source = { max_upload: "default", max_storage: "default" };
  for (let i = 0; i < chain.length; i++) {
    const p = policyFor(chain[i].id);
    if (upload === null) {
      const v = positive(p.max_upload_bytes);
      if (v !== null) { upload = v; source.max_upload = chain[i].source; }
    }
    if (storage === null) {
      const v = positive(p.max_storage_bytes);
      if (v !== null) { storage = v; source.max_storage = chain[i].source; }
    }
  }
  return {
    max_upload_bytes: upload === null ? DEFAULT_MAX_UPLOAD : upload,
    max_storage_bytes: storage === null ? DEFAULT_MAX_STORAGE : storage,
    source: source
  };
}

function usageOf(drive) {
  const u = bkn.store.get(USAGE, drive.key);
  return u || { used_bytes: 0, files: 0 };
}

// Reserve first, write second. Checking then writing would let two concurrent
// uploads both pass the check and jointly exceed the quota; $inc is atomic, so
// the loser of the race sees the over-limit total and gives its bytes back.
function reserve(drive, size, limits) {
  bkn.store.putIfAbsent(USAGE, { used_bytes: 0, files: 0, drive: drive.key }, drive.key);
  const after = bkn.store.patch(USAGE, drive.key, {
    used_bytes: { $inc: size },
    files: { $inc: 1 }
  });
  if (Number(after.used_bytes) > limits.max_storage_bytes) {
    release(drive, size);
    fail("drive quota exceeded: " + limits.max_storage_bytes + " bytes, " +
         (Number(after.used_bytes) - size) + " already used, " + size + " more requested");
  }
  return after;
}

function release(drive, size) {
  bkn.store.patch(USAGE, drive.key, { used_bytes: { $inc: -size }, files: { $inc: -1 } });
}

// --- groups ---------------------------------------------------------------

function groupsOf(user) {
  const rows = bkn.store.list(MEMBERS, { where: { user: user }, limit: 200 });
  const out = [];
  for (let i = 0; i < rows.length; i++) out.push(rows[i].group);
  return out;
}

// --- entries --------------------------------------------------------------

function claimPath(drive, parent, name, entryId) {
  const key = pathKey(drive, parent, name);
  const won = bkn.store.putIfAbsent(PATHS, {
    drive: drive.key, parent: parent, name: name, entry: entryId
  }, key);
  if (!won) fail(joinPath(parent, name) + " already exists in " + drive.key);
  return key;
}

function releasePath(key) {
  bkn.store.delete(PATHS, key);
}

function entryAt(drive, parent, name) {
  const claim = bkn.store.get(PATHS, pathKey(drive, parent, name));
  if (!claim) return null;
  return bkn.store.get(ENTRIES, claim.entry);
}

// --- operations -----------------------------------------------------------

function opLs(input, c) {
  const drive = parseDrive(input.drive, c);
  requireAccess(drive, c, "read");
  const path = normalizePath(input.path);
  const rows = bkn.store.list(ENTRIES, {
    where: { drive: drive.key, parent_path: path, deleted: "" },
    order_by: "name",
    limit: Number(input.limit) > 0 ? Number(input.limit) : 200
  });
  const out = [];
  for (let i = 0; i < rows.length; i++) {
    const r = rows[i];
    out.push({
      id: r.id, name: r.name, kind: r.kind, size: r.size || 0,
      content_type: r.content_type || "", path: joinPath(path, r.name),
      owner: r.owner, visibility: r.visibility, updated_at: r.updated_at
    });
  }
  return { drive: drive.key, path: path, count: out.length, entries: out };
}

function opMkdir(input, c) {
  const drive = parseDrive(input.drive, c);
  requireAccess(drive, c, "write");
  const parent = normalizePath(input.path);
  const name = checkName(input.name);
  if (parent !== "/" && !entryAt(drive, parentOf(parent), baseOf(parent))) {
    fail("parent folder " + parent + " does not exist", "path");
  }
  const id = bkn.id();
  const key = claimPath(drive, parent, name, id);
  try {
    const rec = bkn.store.put(ENTRIES, {
      drive: drive.key, drive_type: drive.type, drive_id: drive.id,
      parent_path: parent, name: name, kind: "folder",
      owner: c.sub, visibility: "private", state: LIVE, deleted: "",
      path_key: key, created_at: bkn.now(), updated_at: bkn.now()
    }, id);
    return { created: true, entry: { id: rec.id, name: name, kind: "folder", path: joinPath(parent, name) } };
  } catch (e) {
    releasePath(key);
    throw e;
  }
}

function parentOf(path) {
  if (path === "/") return "/";
  const at = path.lastIndexOf("/");
  return at <= 0 ? "/" : path.slice(0, at);
}

function baseOf(path) {
  const at = path.lastIndexOf("/");
  return path.slice(at + 1);
}

function opStat(input, c) {
  const drive = parseDrive(input.drive, c);
  requireAccess(drive, c, "read");
  const path = normalizePath(input.path);
  if (path === "/") return { entry: { name: "/", kind: "folder", path: "/" } };
  const entry = entryAt(drive, parentOf(path), baseOf(path));
  if (!entry || entry.state === BINNED) fail(path + " does not exist");
  return { entry: entry };
}

function opRm(input, c) {
  const drive = parseDrive(input.drive, c);
  requireAccess(drive, c, "write");
  const path = normalizePath(input.path);
  if (path === "/") fail("the drive root cannot be removed", "path");
  const parent = parentOf(path), name = baseOf(path);
  const entry = entryAt(drive, parent, name);
  if (!entry || entry.state === BINNED) fail(path + " does not exist");

  if (entry.kind === "folder") {
    const kids = bkn.store.list(ENTRIES, {
      where: { drive: drive.key, parent_path: path, state: LIVE }, limit: 1
    });
    if (kids.length > 0) fail(path + " is not empty");
  }

  // Into the bin, not gone. The path claim IS released, so the name can be
  // used again immediately -- a bin that blocks the name it holds would make
  // "delete and re-upload" fail for thirty days.
  releasePath(entry.path_key);
  bkn.store.patch(ENTRIES, entry.id, {
    state: BINNED, deleted: bkn.now(), deleted_from: parent,
    path_key: "", updated_at: bkn.now()
  });

  // Quota is NOT released. The bytes are still on the disk, and a bin that
  // gave the space back would let a drive hold twice its quota for a month.
  if (entry.kind === "file") {
    bkn.store.patch(USAGE, drive.key, { binned_bytes: { $inc: Number(entry.size) || 0 } });
  }
  return { binned: path, id: entry.id, purges_after_days: BIN_DAYS };
}

// --- the bin ---------------------------------------------------------------

function binnedOf(drive, limit) {
  return bkn.store.list(ENTRIES, {
    where: { drive: drive.key, state: BINNED },
    order_by: "deleted", order: "desc",
    limit: limit || 200
  });
}

function opBin(input, c) {
  const drive = parseDrive(input.drive, c);
  requireAccess(drive, c, "read");
  const rows = binnedOf(drive, Number(input.limit) > 0 ? Number(input.limit) : 200);
  const out = [];
  for (let i = 0; i < rows.length; i++) {
    const r = rows[i];
    out.push({
      id: r.id, name: r.name, kind: r.kind, size: r.size || 0,
      content_type: r.content_type || "", deleted: r.deleted,
      original_path: joinPath(r.deleted_from || "/", r.name),
      owner: r.owner
    });
  }
  return { drive: drive.key, count: out.length, purges_after_days: BIN_DAYS, entries: out };
}

// purgeEntry is the only place that actually destroys anything. Blob first,
// then the record: a record without its blob is a broken row in a listing,
// while a blob without its record is invisible and merely wastes space, and of
// the two the second is the one you can clean up later.
function purgeEntry(drive, entry) {
  if (entry.kind === "file") {
    if (entry.blob) {
      try { bkn.files.delete(BLOBS, entry.blob); } catch (e) { bkn.log("blob delete failed:", e); }
    }
    const size = Number(entry.size) || 0;
    bkn.store.patch(USAGE, drive.key, {
      used_bytes: { $inc: -size }, files: { $inc: -1 }, binned_bytes: { $inc: -size }
    });
  }
  bkn.store.delete(ENTRIES, entry.id);
}

function opPurge(input, c) {
  const drive = parseDrive(input.drive, c);
  requireAccess(drive, c, "write");
  const id = String(input.id || "");
  if (!id) fail("id is required; take it from the bin listing", "id");
  const entry = bkn.store.get(ENTRIES, id);
  if (!entry || entry.drive !== drive.key) fail("no such entry in " + drive.key, "id");
  if (entry.state !== BINNED) fail("that entry is not in the bin; remove it first", "id");
  purgeEntry(drive, entry);
  return { purged: entry.name, id: id };
}

function opEmptyBin(input, c) {
  const drive = parseDrive(input.drive, c);
  requireAccess(drive, c, "write");
  const rows = binnedOf(drive, 500);
  for (let i = 0; i < rows.length; i++) purgeEntry(drive, rows[i]);
  return { drive: drive.key, purged: rows.length, more: rows.length === 500 };
}

function opRestore(input, c) {
  const drive = parseDrive(input.drive, c);
  requireAccess(drive, c, "write");
  const id = String(input.id || "");
  if (!id) fail("id is required; take it from the bin listing", "id");
  const entry = bkn.store.get(ENTRIES, id);
  if (!entry || entry.drive !== drive.key) fail("no such entry in " + drive.key, "id");
  if (entry.state !== BINNED) fail("that entry is not in the bin", "id");

  const parent = normalizePath(input.to_path || entry.deleted_from || "/");
  const name = checkName(input.to_name || entry.name);

  // The folder it came from may be gone, and the name may have been reused
  // while it sat in the bin. Both are ordinary, so say which one happened.
  if (parent !== "/" && !entryAt(drive, parentOf(parent), baseOf(parent))) {
    fail("the folder " + parent + " no longer exists; restore somewhere else with to_path", "to_path");
  }
  const key = claimPath(drive, parent, name, entry.id);
  bkn.store.patch(ENTRIES, entry.id, {
    state: LIVE, deleted: "", parent_path: parent, name: name,
    path_key: key, updated_at: bkn.now()
  });
  if (entry.kind === "file") {
    bkn.store.patch(USAGE, drive.key, { binned_bytes: { $inc: -(Number(entry.size) || 0) } });
  }
  return { restored: joinPath(parent, name), id: entry.id };
}

function opMv(input, c) {
  const drive = parseDrive(input.drive, c);
  requireAccess(drive, c, "write");
  const from = normalizePath(input.path);
  const toParent = normalizePath(input.to_path === undefined ? parentOf(from) : input.to_path);
  const toName = checkName(input.to_name || baseOf(from));
  if (from === "/") fail("the drive root cannot be moved", "path");

  const entry = entryAt(drive, parentOf(from), baseOf(from));
  if (!entry || entry.state === BINNED) fail(from + " does not exist");
  if (entry.kind === "folder" && (toParent === from || toParent.indexOf(from + "/") === 0)) {
    fail("a folder cannot be moved inside itself", "to_path");
  }

  const key = claimPath(drive, toParent, toName, entry.id);
  releasePath(entry.path_key);
  bkn.store.patch(ENTRIES, entry.id, {
    parent_path: toParent, name: toName, path_key: key, updated_at: bkn.now()
  });
  return { moved: from, to: joinPath(toParent, toName) };
}

function opDownload(input, c) {
  const drive = parseDrive(input.drive, c);
  const path = normalizePath(input.path);
  const entry = entryAt(drive, parentOf(path), baseOf(path));
  if (!entry || entry.state === BINNED) fail(path + " does not exist");
  if (entry.kind !== "file") fail(path + " is a folder");

  // A share grants access to one entry without granting the drive, which is
  // the whole point of sharing: no wider grant than the thing being shared.
  if (access(drive, c) === "none") {
    const share = bkn.store.get(SHARES, entry.id + ":" + c.sub);
    if (!share && entry.visibility !== "public") fail("no access to " + path);
  }
  const ttl = String(input.ttl || "1h");
  return { path: path, size: entry.size, content_type: entry.content_type,
           url: bkn.files.sign(BLOBS, entry.blob, { ttl: ttl }), expires_in: ttl };
}

function opQuota(input, c) {
  const drive = parseDrive(input.drive, c);
  requireAccess(drive, c, "read");
  const limits = effectiveLimits(drive, c);
  const usage = usageOf(drive);
  return {
    drive: drive.key,
    limits: limits,
    usage: {
      used_bytes: Number(usage.used_bytes) || 0,
      files: Number(usage.files) || 0,
      // Binned files still occupy their bytes, so the number is reported
      // rather than hidden: "delete things to free space" is misleading advice
      // if the space comes back in thirty days.
      binned_bytes: Number(usage.binned_bytes) || 0,
      free_bytes: Math.max(0, limits.max_storage_bytes - (Number(usage.used_bytes) || 0))
    }
  };
}

function opShare(input, c) {
  const drive = parseDrive(input.drive, c);
  requireAccess(drive, c, "write");
  const path = normalizePath(input.path);
  const entry = entryAt(drive, parentOf(path), baseOf(path));
  if (!entry || entry.state === BINNED) fail(path + " does not exist");
  const withUser = resolveUser(input.user, "user");
  const level = input.access === "write" ? "write" : "read";
  bkn.store.put(SHARES, {
    entry: entry.id, user: withUser, access: level,
    drive: drive.key, granted_by: c.sub, granted_at: bkn.now()
  }, entry.id + ":" + withUser);
  return { shared: path, user: withUser, access: level };
}

function opUnshare(input, c) {
  const drive = parseDrive(input.drive, c);
  requireAccess(drive, c, "write");
  const path = normalizePath(input.path);
  const entry = entryAt(drive, parentOf(path), baseOf(path));
  if (!entry) fail(path + " does not exist");
  const withUser = resolveUser(input.user, "user");
  const gone = bkn.store.delete(SHARES, entry.id + ":" + withUser);
  return { unshared: path, user: withUser, existed: gone };
}

function opShares(input, c) {
  const drive = parseDrive(input.drive, c);
  requireAccess(drive, c, "read");
  const path = normalizePath(input.path);
  const entry = entryAt(drive, parentOf(path), baseOf(path));
  if (!entry) fail(path + " does not exist");
  const rows = bkn.store.list(SHARES, { where: { entry: entry.id }, limit: 200 });
  return { path: path, count: rows.length, shares: rows };
}

// --- group management -----------------------------------------------------

function opGroupCreate(input, c) {
  const name = String(input.name || "").trim();
  if (!name) fail("name is required", "name");
  const org = String(input.org || c.org || "");
  if (!org) fail("org is required for a group", "org");
  if (!isAdmin(c) && !bkn.auth.can(c.sub, org, "admin")) {
    fail("only an org admin may create a group");
  }
  const id = bkn.id();
  bkn.store.put(GROUPS, { name: name, org: org, created_by: c.sub, created_at: bkn.now() }, id);
  bkn.store.put(MEMBERS, { group: id, user: c.sub, role: "owner", added_at: bkn.now() }, id + ":" + c.sub);
  return { group: { id: id, name: name, org: org }, drive: "group:" + id };
}

function requireGroupOwner(groupId, c) {
  if (isAdmin(c)) return;
  const m = bkn.store.get(MEMBERS, groupId + ":" + c.sub);
  if (!m || (m.role !== "owner" && m.role !== "admin")) fail("only a group owner may do that");
}

function opGroupAdd(input, c) {
  const groupId = String(input.group || "");
  const g = bkn.store.get(GROUPS, groupId);
  if (!g) fail("no such group: " + groupId, "group");
  requireGroupOwner(groupId, c);
  const user = resolveUser(input.user, "user");
  const role = input.role === "owner" ? "owner" : (input.role === "reader" ? "reader" : "member");
  bkn.store.put(MEMBERS, { group: groupId, user: user, role: role, added_at: bkn.now() },
                groupId + ":" + user);
  return { group: groupId, user: user, role: role };
}

function opGroupRemove(input, c) {
  const groupId = String(input.group || "");
  if (!bkn.store.get(GROUPS, groupId)) fail("no such group: " + groupId, "group");
  requireGroupOwner(groupId, c);
  const user = resolveUser(input.user, "user");
  return { group: groupId, user: user, removed: bkn.store.delete(MEMBERS, groupId + ":" + user) };
}

function opGroups(input, c) {
  const user = isAdmin(c) && input.user ? resolveUser(input.user, "user") : c.sub;
  const ids = groupsOf(user);
  const out = [];
  for (let i = 0; i < ids.length; i++) {
    const g = bkn.store.get(GROUPS, ids[i]);
    if (g) out.push({ id: ids[i], name: g.name, org: g.org, drive: "group:" + ids[i] });
  }
  return { user: user, count: out.length, groups: out };
}

// --- policy ---------------------------------------------------------------

function opPolicySet(input, c) {
  if (!isAdmin(c)) fail("only an admin may set a quota policy");
  const target = String(input.target || "");
  if (!target) fail('target is required, e.g. "global", "org:acme", "user:<id>"', "target");
  const doc = { updated_at: bkn.now(), updated_by: c.sub };
  if (input.max_upload_bytes !== undefined) doc.max_upload_bytes = positive(input.max_upload_bytes) || 0;
  if (input.max_storage_bytes !== undefined) doc.max_storage_bytes = positive(input.max_storage_bytes) || 0;
  bkn.store.put(POLICY, doc, target);
  return { policy: target, applied: doc };
}

function opPolicyGet(input, c) {
  if (!isAdmin(c)) fail("only an admin may read the quota policy");
  const rows = bkn.store.list(POLICY, { limit: 200 });
  return { count: rows.length, policies: rows };
}

// --- entry point ----------------------------------------------------------

const OPS = {
  ls: opLs, mkdir: opMkdir, stat: opStat, rm: opRm, mv: opMv,
  bin: opBin, restore: opRestore, purge: opPurge, "empty-bin": opEmptyBin,
  download: opDownload, quota: opQuota,
  share: opShare, unshare: opUnshare, shares: opShares,
  "group-create": opGroupCreate, "group-add": opGroupAdd,
  "group-remove": opGroupRemove, groups: opGroups,
  "policy-set": opPolicySet, "policy-get": opPolicyGet
};

function main(input) {
  const d = input || {};
  const op = String(d.op || "");
  const fn = OPS[op];
  if (!fn) {
    const names = [];
    for (const k in OPS) names.push(k);
    names.sort();
    fail('unknown op "' + op + '"; try one of: ' + names.join(", "), "op");
  }
  return fn(d, caller());
}
