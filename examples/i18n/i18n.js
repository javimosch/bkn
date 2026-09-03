// Translation bundles.
//
// Replaces i18n.service + i18nInferredKeys.service + their controllers and the
// I18nEntry / I18nLocale models from the Node backend.
//
//   GET /v1/hooks/i18n                       -> bundle for the best locale
//   GET /v1/hooks/i18n?locale=fr             -> bundle for fr, filled from the default
//   GET /v1/hooks/i18n?locale=fr&key=nav.home -> one string, recording a miss
//
// Install:
//   bkn store put i18n/locales --id en --data '{"name":"English","default":true}'
//   bkn script create i18n --file i18n.js
//   bkn hooks create i18n --script i18n --allow-origin https://your-site.example --rate-limit 300

const LOCALES = "i18n/locales";
const ENTRIES = "i18n/entries";
const MISSING = "i18n/missing";

function entryId(locale, key) {
  return locale + "--" + bkn.crypto.hash(key).slice(0, 32);
}

function defaultLocale() {
  const marked = bkn.store.find(LOCALES, { default: true });
  if (marked) return marked.id;
  const any = bkn.store.list(LOCALES, { limit: 1 });
  return any.length ? any[0].id : "en";
}

function knownLocales() {
  return bkn.store.list(LOCALES, { limit: 100 })
    .filter((l) => l.enabled !== false)
    .map((l) => l.id);
}

// Accept-Language is a weighted list; honouring the weights is the difference
// between "fr-CA,fr;q=0.9,en;q=0.8" getting French and getting whatever came
// first in the header.
function negotiate(header, available) {
  const wanted = String(header || "")
    .split(",")
    .map((part) => {
      const [tag, ...params] = part.trim().split(";");
      const q = params.map((p) => p.trim()).find((p) => p.indexOf("q=") === 0);
      return { tag: tag.trim().toLowerCase(), q: q ? parseFloat(q.slice(2)) : 1 };
    })
    .filter((c) => c.tag)
    .sort((a, b) => b.q - a.q);

  for (const candidate of wanted) {
    if (available.indexOf(candidate.tag) >= 0) return candidate.tag;
    // fr-CA falls back to fr before moving on to the next preference.
    const base = candidate.tag.split("-")[0];
    if (available.indexOf(base) >= 0) return base;
  }
  return null;
}

function entriesFor(locale) {
  const map = {};
  let offset = 0;
  for (;;) {
    const page = bkn.store.list(ENTRIES, { where: { locale: locale }, limit: 500, offset: offset });
    if (page.length === 0) break;
    for (const e of page) map[e.key] = e.value;
    offset += page.length;
    if (page.length < 500) break;
  }
  return map;
}

// A key missing in one locale but present in the default should render the
// default rather than the raw key: a half-translated site beats a broken one.
function bundleFor(locale, fallback) {
  const base = locale === fallback ? {} : entriesFor(fallback);
  const target = entriesFor(locale);
  const merged = {};
  for (const k of Object.keys(base)) merged[k] = base[k];
  for (const k of Object.keys(target)) merged[k] = target[k];
  return merged;
}

// Recording what the site asked for and did not find turns translation from a
// guessing game into a work queue. putIfAbsent keeps it to one row per key.
function recordMiss(locale, key) {
  const created = bkn.store.putIfAbsent(MISSING, {
    locale: locale, key: key, first_seen: bkn.now(),
  }, entryId(locale, key));
  if (created) {
    bkn.events.emit("i18n", "key.missing", { subject: locale, data: { key: key } });
  }
}

function interpolate(template, vars) {
  if (!vars) return template;
  return String(template).replace(/\{\{\s*([a-zA-Z0-9_.]+)\s*\}\}/g, (whole, name) =>
    Object.prototype.hasOwnProperty.call(vars, name) ? String(vars[name]) : whole);
}

function main(d) {
  if (d.method !== "GET") {
    return { status: 405, body: { error: "read a bundle with GET" } };
  }

  const fallback = defaultLocale();
  const available = knownLocales();
  const locale =
    (d.query.locale && available.indexOf(d.query.locale.toLowerCase()) >= 0
      ? d.query.locale.toLowerCase()
      : null) ||
    negotiate(d.headers["accept-language"], available) ||
    fallback;

  // A single key: the cheap path a server-rendered page uses per string.
  if (d.query.key) {
    const own = bkn.store.get(ENTRIES, entryId(locale, d.query.key));
    const inherited = own || (locale === fallback
      ? null
      : bkn.store.get(ENTRIES, entryId(fallback, d.query.key)));
    if (!inherited) {
      recordMiss(locale, d.query.key);
      return {
        status: 200,
        body: { locale: locale, key: d.query.key, value: d.query.key, missing: true },
      };
    }
    let vars = null;
    if (d.query.vars) {
      try { vars = JSON.parse(d.query.vars); } catch (e) { vars = null; }
    }
    return {
      status: 200,
      body: {
        locale: locale, key: d.query.key,
        value: interpolate(inherited.value, vars),
        format: inherited.format || "text",
        inherited: !own,
      },
    };
  }

  const entries = bundleFor(locale, fallback);
  const body = {
    locale: locale, default_locale: fallback, available: available,
    count: Object.keys(entries).length, entries: entries,
  };

  // The bundle is large and changes rarely, so the content hash is worth
  // returning as an ETag: a client that already has it re-downloads nothing.
  const etag = '"' + bkn.crypto.hash(JSON.stringify(body)).slice(0, 32) + '"';
  if (d.headers["if-none-match"] === etag) {
    return { status: 304, body: "", headers: { "ETag": etag } };
  }
  return {
    status: 200, body: body,
    headers: { "ETag": etag, "Cache-Control": "public, max-age=60" },
  };
}
