// Headless CMS: schema-driven CRUD over user-defined content models.
//
// Replaces headlessModels.service, headlessCrud.controller,
// headlessApiTokenAuth and two Mongoose models (~780 lines).
//
//   GET    /v1/hooks/cms?model=articles&status=live&order_by=published_at
//   GET    /v1/hooks/cms?model=articles&id=<id>
//   POST   /v1/hooks/cms?model=articles      { "title": "...", ... }
//   PUT    /v1/hooks/cms?model=articles&id=<id>
//   DELETE /v1/hooks/cms?model=articles&id=<id>
//
// Every request carries X-Api-Token.
//
// Install:
//   bkn script run headless-model --input @article-model.json
//   bkn script create headless --file headless.js
//   bkn hooks create cms --script headless --rate-limit 600

const MODELS = "headless/models";
const TOKENS = "headless/tokens";

function fail(status, error, field) {
  // The key is omitted rather than set to undefined: goja exports undefined as
  // null, and a null "field" reads like a field named null.
  const body = { error: error };
  if (field) body.field = field;
  return { status: status, body: body };
}

// --- tokens ---------------------------------------------------------------

// The token id IS its hash, so authenticating is one indexed lookup and the
// plaintext is never stored. There is nothing to compare, so nothing to leak
// through comparison timing either.
function authenticate(d) {
  const supplied = d.headers["x-api-token"] || d.headers["x-api-key"] || "";
  if (!supplied) return null;
  const token = bkn.store.get(TOKENS, bkn.crypto.hash(supplied).slice(0, 32));
  if (!token || token.enabled === false) return null;
  return token;
}

function permits(token, modelCode, operation) {
  const grants = (token.permissions || {})[modelCode] || (token.permissions || {})["*"] || [];
  return grants.indexOf(operation) >= 0 || grants.indexOf("*") >= 0;
}

// --- validation -----------------------------------------------------------

const TYPES = {
  string: (v) => typeof v === "string",
  number: (v) => typeof v === "number" && isFinite(v),
  boolean: (v) => typeof v === "boolean",
  date: (v) => typeof v === "string" && !isNaN(Date.parse(v)),
  object: (v) => v !== null && typeof v === "object" && !Array.isArray(v),
  array: (v) => Array.isArray(v),
  ref: (v) => typeof v === "string",
  "ref[]": (v) => Array.isArray(v) && v.every((x) => typeof x === "string"),
};

function checkField(field, value) {
  const check = TYPES[field.type];
  if (!check) return "unknown field type " + field.type;
  if (!check(value)) return "must be a " + field.type;

  const rules = field.validation || {};
  if (rules.enum && rules.enum.indexOf(value) === -1) {
    return "must be one of: " + rules.enum.join(", ");
  }
  if (field.type === "number") {
    if (rules.min !== undefined && value < rules.min) return "must be at least " + rules.min;
    if (rules.max !== undefined && value > rules.max) return "must be at most " + rules.max;
  }
  if (field.type === "string") {
    if (rules.minLength !== undefined && value.length < rules.minLength) {
      return "must be at least " + rules.minLength + " characters";
    }
    if (rules.maxLength !== undefined && value.length > rules.maxLength) {
      return "must be at most " + rules.maxLength + " characters";
    }
    if (rules.match && !new RegExp(rules.match).test(value)) return "does not match " + rules.match;
  }
  return null;
}

// Validation happens here rather than in the store, because the store is
// deliberately schemaless: a model change needs no migration precisely
// because nothing below this line knows the schema exists.
function validate(model, submitted, partial) {
  const clean = {};
  for (const field of model.fields) {
    let value = submitted[field.name];

    if (value === undefined) {
      if (partial) continue;
      if (field.default !== undefined) value = field.default;
      else if (field.required) return { error: fail(400, "this field is required", field.name) };
      else continue;
    }
    if (value === null) {
      if (field.required) return { error: fail(400, "this field is required", field.name) };
      clean[field.name] = null;
      continue;
    }
    const problem = checkField(field, value);
    if (problem) return { error: fail(400, problem, field.name) };
    clean[field.name] = value;
  }

  // Unknown keys are dropped rather than rejected, so adding a field to a
  // client before adding it to the model does not start failing writes.
  return { fields: clean };
}

// --- uniqueness -----------------------------------------------------------

// The store gives uniqueness on the id and nowhere else, so a second unique
// field needs a companion collection whose id is the value. putIfAbsent
// decides the race; a read-then-write would not.
function uniqueRef(modelCode, fieldName) {
  return "headless/u-" + modelCode + "-" + fieldName;
}

function claimUnique(model, id, fields, previous) {
  const claimed = [];
  for (const field of model.fields) {
    if (!field.unique || fields[field.name] === undefined) continue;
    const key = bkn.crypto.hash(String(fields[field.name])).slice(0, 32);
    const ref = uniqueRef(model.id, field.name);

    const before = previous ? previous[field.name] : undefined;
    if (before !== undefined && String(before) === String(fields[field.name])) continue;

    const won = bkn.store.putIfAbsent(ref, { value: fields[field.name], record: id }, key);
    if (won === null) {
      const holder = bkn.store.get(ref, key);
      if (!holder || holder.record !== id) {
        for (const undo of claimed) bkn.store.delete(undo.ref, undo.key);
        return { error: fail(409, "must be unique", field.name) };
      }
    } else {
      claimed.push({ ref: ref, key: key });
    }
    if (before !== undefined) {
      bkn.store.delete(ref, bkn.crypto.hash(String(before)).slice(0, 32));
    }
  }
  return {};
}

function releaseUnique(model, record) {
  for (const field of model.fields) {
    if (!field.unique || record[field.name] === undefined) continue;
    bkn.store.delete(uniqueRef(model.id, field.name),
      bkn.crypto.hash(String(record[field.name])).slice(0, 32));
  }
}

// --- references -----------------------------------------------------------

// One extra read per referenced model rather than per record: without the
// grouping this is an N+1 that gets slower with every row returned.
function populate(model, records, requested) {
  const wanted = String(requested || "").split(",").map((s) => s.trim()).filter(Boolean);
  if (wanted.length === 0) return records;

  for (const field of model.fields) {
    if (wanted.indexOf(field.name) === -1) continue;
    if (field.type !== "ref" && field.type !== "ref[]") continue;
    const target = "headless/" + field.ref_model;

    const ids = [];
    for (const record of records) {
      const value = record[field.name];
      if (Array.isArray(value)) ids.push.apply(ids, value);
      else if (value) ids.push(value);
    }
    if (ids.length === 0) continue;

    const resolved = {};
    for (const item of bkn.store.list(target, { where: { id: { in: ids } }, limit: 500 })) {
      resolved[item.id] = item;
    }
    for (const record of records) {
      const value = record[field.name];
      if (Array.isArray(value)) record[field.name] = value.map((id) => resolved[id] || id);
      else if (value) record[field.name] = resolved[value] || value;
    }
  }
  return records;
}

// --- request handling -----------------------------------------------------

const RESERVED = ["model", "id", "limit", "offset", "order_by", "order", "populate"];

function filtersFrom(query, model) {
  const where = {};
  const known = {};
  for (const field of model.fields) known[field.name] = field;
  for (const key of Object.keys(query)) {
    if (RESERVED.indexOf(key) >= 0) continue;
    if (!known[key] && key !== "id") continue; // never filter on an unmodelled key
    const raw = query[key];
    const [maybeOp, rest] = [raw.slice(0, raw.indexOf(":")), raw.slice(raw.indexOf(":") + 1)];
    if (raw.indexOf(":") > 0 && ["gt", "gte", "lt", "lte", "ne", "in"].indexOf(maybeOp) >= 0) {
      where[key] = {};
      where[key][maybeOp] = maybeOp === "in" ? rest.split(",") : coerce(known[key], rest);
    } else {
      where[key] = coerce(known[key], raw);
    }
  }
  return where;
}

function coerce(field, raw) {
  if (field && field.type === "number") return Number(raw);
  if (field && field.type === "boolean") return raw === "true";
  return raw;
}

function body(d) {
  try {
    const parsed = JSON.parse(d.body || "{}");
    // The Node version accepted the fields at the top level or nested; keep
    // both so existing clients do not have to change.
    return parsed.data || parsed.fields || parsed;
  } catch (e) {
    return null;
  }
}

function main(d) {
  const token = authenticate(d);
  if (!token) return fail(401, "invalid or missing X-Api-Token");

  const code = d.query.model;
  if (!code) return fail(400, "pass ?model=<code>");
  const model = bkn.store.get(MODELS, code);
  if (!model || model.enabled === false) return fail(404, "no such model");

  const collection = "headless/" + code;
  const id = d.query.id;

  if (d.method === "GET") {
    if (!permits(token, code, "read")) return fail(403, "token cannot read " + code);

    if (id) {
      const record = bkn.store.get(collection, id);
      if (!record) return fail(404, "no such item");
      return { status: 200, body: { item: populate(model, [record], d.query.populate)[0] } };
    }
    const where = filtersFrom(d.query, model);
    const limit = Math.min(Number(d.query.limit || 50) || 50, 200);
    const items = bkn.store.list(collection, {
      where: where, limit: limit, offset: Number(d.query.offset || 0) || 0,
      order_by: d.query.order_by || "", order: d.query.order === "asc" ? "asc" : "desc",
    });
    return {
      status: 200,
      body: {
        items: populate(model, items, d.query.populate),
        total: bkn.store.count(collection, where),
        limit: limit, offset: Number(d.query.offset || 0) || 0,
      },
    };
  }

  if (d.method === "DELETE") {
    if (!permits(token, code, "write")) return fail(403, "token cannot write " + code);
    if (!id) return fail(400, "pass ?id=<id>");
    const existing = bkn.store.get(collection, id);
    if (!existing) return fail(404, "no such item");
    releaseUnique(model, existing);
    bkn.store.delete(collection, id);
    bkn.events.emit("headless", "item.deleted", { subject: code, data: { id: id } });
    return { status: 200, body: { deleted: id } };
  }

  if (d.method === "POST" || d.method === "PUT" || d.method === "PATCH") {
    if (!permits(token, code, "write")) return fail(403, "token cannot write " + code);
    const submitted = body(d);
    if (!submitted) return fail(400, "body must be a JSON object");

    const updating = Boolean(id);
    const previous = updating ? bkn.store.get(collection, id) : null;
    if (updating && !previous) return fail(404, "no such item");

    const checked = validate(model, submitted, d.method === "PATCH");
    if (checked.error) return checked.error;

    const recordId = updating ? id : bkn.id();
    const claim = claimUnique(model, recordId, checked.fields, previous);
    if (claim.error) return claim.error;

    const now = bkn.now();
    let saved;
    if (updating) {
      saved = bkn.store.patch(collection, id,
        Object.assign({}, checked.fields, { updated_at: now }));
    } else {
      saved = bkn.store.put(collection,
        Object.assign({}, checked.fields, { created_at: now, updated_at: now }), recordId);
    }
    bkn.events.emit("headless", updating ? "item.updated" : "item.created", {
      subject: code, data: { id: saved.id },
    });
    return { status: updating ? 200 : 201, body: { item: saved } };
  }

  return fail(405, "unsupported method " + d.method);
}
