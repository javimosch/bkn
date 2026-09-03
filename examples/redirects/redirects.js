// Page redirects.
//
// Replaces pageRedirects.service + adminPageRedirects.routes + the
// PageRedirect model from the Node backend.
//
// bkn does not serve pages, so this resolves a path and answers with a real
// 301/302 that an edge proxy can follow, or with JSON for a caller that wants
// to decide for itself:
//
//   GET /v1/hooks/redirects?path=/old-page          -> 301 Location: /new-page
//   GET /v1/hooks/redirects?path=/old-page&as=json  -> {"match":true,"to":"/new-page"}
//
// Install:
//   bkn script create redirects --file redirects.js
//   bkn hooks create redirects --script redirects --rate-limit 600

const EXACT = "redirects/rules";
const PREFIX = "redirects/prefixes";

// The rule id is a hash of the normalised path, which turns the common case -
// "is there a redirect for this exact path" - into one indexed lookup instead
// of a scan over every rule.
function idFor(path) {
  return bkn.crypto.hash(normalize(path)).slice(0, 32);
}

function normalize(path) {
  let p = String(path || "/").trim().toLowerCase();
  const q = p.indexOf("?");
  if (q >= 0) p = p.slice(0, q);
  p = p.replace(/\/+$/, "");
  return p === "" ? "/" : p;
}

function queryOf(path) {
  const q = String(path || "").indexOf("?");
  return q >= 0 ? String(path).slice(q + 1) : "";
}

// Prefix rules are scanned, but there are only ever a handful and the longest
// match must win: /docs/v1 has to beat /docs.
function matchPrefix(path) {
  const rules = bkn.store.list(PREFIX, { limit: 200 });
  let best = null;
  for (const rule of rules) {
    if (rule.enabled === false) continue;
    const from = normalize(rule.from);
    if (path === from || path.indexOf(from + "/") === 0) {
      if (!best || normalize(best.from).length < from.length) best = rule;
    }
  }
  if (!best) return null;
  const rest = path.slice(normalize(best.from).length);
  return {
    to: best.keep_path === false ? best.to : best.to.replace(/\/$/, "") + rest,
    type: best.type || 301,
    rule: best.id,
  };
}

function resolve(rawPath) {
  const path = normalize(rawPath);

  const exact = bkn.store.get(EXACT, idFor(path));
  if (exact && exact.enabled !== false) {
    return { to: exact.to, type: exact.type || 301, rule: exact.id };
  }
  return matchPrefix(path);
}

function main(d) {
  if (d.method !== "GET") {
    return { status: 405, body: { error: "resolve a redirect with GET" } };
  }
  const requested = d.query.path;
  if (!requested) return { status: 400, body: { error: "pass ?path=/some/page" } };

  const hit = resolve(requested);
  if (!hit) {
    return {
      status: 404,
      body: { match: false, path: normalize(requested) },
      // A miss is cacheable too, but briefly: a rule may be added at any time.
      headers: { "Cache-Control": "public, max-age=60" },
    };
  }

  // The original query string is carried across unless the target has one of
  // its own; dropping ?utm_source silently loses attribution.
  let target = hit.to;
  const query = queryOf(requested);
  if (query && target.indexOf("?") === -1) target += "?" + query;

  bkn.events.emit("redirects", "hit", {
    subject: normalize(requested), data: { to: target, type: hit.type },
  });

  if (d.query.as === "json") {
    return { status: 200, body: { match: true, to: target, type: hit.type } };
  }
  return {
    status: hit.type === 302 ? 302 : 301,
    body: "",
    headers: {
      "Location": target,
      // A permanent redirect is worth caching; a temporary one is not.
      "Cache-Control": hit.type === 302 ? "no-cache" : "public, max-age=3600",
    },
  };
}
