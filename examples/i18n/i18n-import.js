// Import and export translation bundles.
//
// Run from the CLI, never bound to a hook: writing translations is operator
// work, and a hook is public.
//
//   bkn script run i18n-import --input '{"locale":"fr","entries":@fr.json}'
//   bkn script run i18n-import --input '{"locale":"fr","export":true}'

const LOCALES = "i18n/locales";
const ENTRIES = "i18n/entries";

function entryId(locale, key) {
  return locale + "--" + bkn.crypto.hash(key).slice(0, 32);
}

// Translators hand over nested JSON; the store holds flat keys. Flattening on
// the way in means the bundle format and the storage format can differ without
// the site caring.
function flatten(value, prefix, out) {
  out = out || {};
  for (const key of Object.keys(value || {})) {
    const path = prefix ? prefix + "." + key : key;
    const item = value[key];
    if (item && typeof item === "object" && !Array.isArray(item)) {
      flatten(item, path, out);
    } else {
      out[path] = Array.isArray(item) ? JSON.stringify(item) : String(item);
    }
  }
  return out;
}

function main(input) {
  const locale = String(input.locale || "").toLowerCase();
  if (!locale) throw new Error("locale is required");

  if (input.export) {
    const out = {};
    let offset = 0;
    for (;;) {
      const page = bkn.store.list(ENTRIES, { where: { locale: locale }, limit: 500, offset: offset });
      if (page.length === 0) break;
      for (const e of page) out[e.key] = e.value;
      offset += page.length;
      if (page.length < 500) break;
    }
    return { locale: locale, count: Object.keys(out).length, entries: out };
  }

  if (!input.entries) throw new Error("entries is required unless exporting");

  if (bkn.store.get(LOCALES, locale) === null) {
    bkn.store.put(LOCALES, { name: input.name || locale, enabled: true, default: false }, locale);
  }

  const flat = flatten(input.entries);
  let written = 0, skipped = 0;
  for (const key of Object.keys(flat)) {
    const id = entryId(locale, key);
    const existing = bkn.store.get(ENTRIES, id);
    // An import must not silently undo a correction made in the admin unless
    // it is explicitly told to.
    if (existing && !input.overwrite) { skipped++; continue; }
    bkn.store.put(ENTRIES, {
      locale: locale, key: key, value: flat[key],
      format: /<[a-z][\s\S]*>/i.test(flat[key]) ? "html" : "text",
      updated_at: bkn.now(),
    }, id);
    written++;
  }

  // A key imported into this locale is no longer missing.
  let resolved = 0;
  for (const key of Object.keys(flat)) {
    if (bkn.store.delete("i18n/missing", entryId(locale, key))) resolved++;
  }

  bkn.events.emit("i18n", "import.completed", {
    subject: locale, data: { written: written, skipped: skipped, resolved: resolved },
  });
  return { locale: locale, written: written, skipped: skipped, resolved_missing: resolved };
}
