// Public JSON configuration documents.
//
// Replaces jsonConfigs.service + its controller and the JsonConfig model from
// the Node backend (~660 lines, half of which is a hand-rolled cache).
//
//   GET /v1/hooks/configs?slug=pricing-table
//   GET /v1/hooks/configs?alias=pricing        (a stable name for a rotating slug)
//   GET /v1/hooks/configs?slug=pricing-table&raw=1   -> the document alone
//
// Install:
//   bkn script create configs --file configs.js
//   bkn hooks create configs --script configs \
//     --allow-origin https://your-site.example --rate-limit 300

const DOCS = "configs/documents";
const ALIASES = "configs/aliases";

function resolveSlug(query) {
  if (query.slug) return String(query.slug).toLowerCase();
  if (query.alias) {
    const pointer = bkn.store.get(ALIASES, String(query.alias).toLowerCase());
    return pointer ? pointer.slug : null;
  }
  return null;
}

function main(d) {
  if (d.method !== "GET") {
    return { status: 405, body: { error: "read a config with GET" } };
  }
  const slug = resolveSlug(d.query);
  if (!slug) return { status: 400, body: { error: "pass ?slug=<slug> or ?alias=<alias>" } };

  const doc = bkn.store.get(DOCS, slug);
  // A private config and a missing one look identical from outside: whether a
  // slug exists is itself information about unreleased work.
  if (!doc || doc.public !== true) {
    return { status: 404, body: { error: "no such config" } };
  }

  // The stored hash is the ETag. The Node version kept an in-process TTL cache
  // per slug; HTTP already has caching, and its cache is shared by every
  // client rather than living in one server's memory.
  const etag = '"' + doc.hash + '"';
  const maxAge = Math.max(0, Number(doc.cache_ttl_seconds) || 0);
  const headers = {
    "ETag": etag,
    "Cache-Control": maxAge > 0 ? "public, max-age=" + maxAge : "no-cache",
  };
  if (d.headers["if-none-match"] === etag) {
    return { status: 304, body: "", headers: headers };
  }

  bkn.events.emit("configs", "read", { subject: slug, data: { alias: d.query.alias || null } });

  if (d.query.raw === "1") {
    return { status: 200, body: doc.value, headers: headers };
  }
  return {
    status: 200,
    headers: headers,
    body: {
      slug: doc.id, title: doc.title, alias: doc.alias || null,
      hash: doc.hash, updated_at: doc.updated_at, value: doc.value,
    },
  };
}
