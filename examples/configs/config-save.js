// Create or update a JSON config.
//
// Run from the CLI, never bound to a hook: writing configuration is operator
// work and a hook is public.
//
//   bkn script run config-save --input '{"title":"Pricing table","value":{...},"public":true,"alias":"pricing"}'
//   bkn script run config-save --input '{"slug":"pricing-table-a1b2","value":{...}}'

const DOCS = "configs/documents";
const ALIASES = "configs/aliases";

function slugBase(title) {
  return String(title).toLowerCase()
    .replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 60) || "config";
}

// A slug is the document id, so uniqueness comes from putIfAbsent rather than
// from a check that a concurrent save could slip past.
function claimSlug(title) {
  const base = slugBase(title);
  for (let attempt = 0; attempt < 12; attempt++) {
    const slug = attempt === 0 ? base : base + "-" + bkn.crypto.randomHex(2);
    if (bkn.store.get(DOCS, slug) === null) return slug;
  }
  throw new Error("could not find a free slug for " + base);
}

function main(input) {
  if (input.value === undefined) throw new Error("value is required");
  const isNew = !input.slug;
  const slug = input.slug ? String(input.slug).toLowerCase() : claimSlug(input.title || "config");

  const existing = bkn.store.get(DOCS, slug);
  if (!isNew && !existing) throw new Error("no config with slug " + slug);

  const serialized = JSON.stringify(input.value);
  const record = {
    title: input.title || (existing && existing.title) || slug,
    value: input.value,
    hash: bkn.crypto.hash(serialized).slice(0, 32),
    public: input.public !== undefined
      ? Boolean(input.public)
      : Boolean(existing && existing.public),
    cache_ttl_seconds: input.cache_ttl_seconds !== undefined
      ? Number(input.cache_ttl_seconds)
      : (existing ? existing.cache_ttl_seconds : 0),
    alias: input.alias !== undefined
      ? (input.alias ? String(input.alias).toLowerCase() : null)
      : (existing ? existing.alias : null),
    size_bytes: serialized.length,
    updated_at: bkn.now(),
  };

  // An alias is a stable public name for a slug that may be regenerated, so it
  // must never point at two documents at once.
  if (record.alias) {
    const claimed = bkn.store.putIfAbsent(ALIASES, { slug: slug }, record.alias);
    if (claimed === null) {
      const owner = bkn.store.get(ALIASES, record.alias);
      if (owner.slug !== slug) {
        throw new Error("alias " + record.alias + " already points at " + owner.slug);
      }
    }
  }
  if (existing && existing.alias && existing.alias !== record.alias) {
    bkn.store.delete(ALIASES, existing.alias);
  }

  const saved = bkn.store.put(DOCS, record, slug);
  bkn.events.emit("configs", isNew ? "created" : "updated", {
    subject: slug, data: { hash: record.hash, bytes: record.size_bytes },
  });
  return {
    slug: saved.id, alias: record.alias, hash: record.hash,
    public: record.public, bytes: record.size_bytes,
  };
}
