// Define or update a content model, and mint API tokens.
//
// Run from the CLI: defining schemas and issuing credentials is operator work
// and a hook is public.
//
//   bkn script run headless-model --input @article-model.json
//   bkn script run headless-model --input '{"token":{"name":"website","permissions":{"articles":["read"]}}}'

const MODELS = "headless/models";
const TOKENS = "headless/tokens";

const TYPES = ["string", "number", "boolean", "date", "object", "array", "ref", "ref[]"];

function validateFields(fields) {
  if (!Array.isArray(fields) || fields.length === 0) throw new Error("fields must be a non-empty array");
  const seen = {};
  for (const field of fields) {
    if (!field.name || !/^[a-z][a-z0-9_]*$/.test(field.name)) {
      throw new Error("field name must match [a-z][a-z0-9_]*, got " + field.name);
    }
    if (seen[field.name]) throw new Error("duplicate field " + field.name);
    seen[field.name] = true;
    if (TYPES.indexOf(field.type) === -1) {
      throw new Error("field " + field.name + ": type must be one of " + TYPES.join(", "));
    }
    if ((field.type === "ref" || field.type === "ref[]") && !field.ref_model) {
      throw new Error("field " + field.name + " is a reference and needs ref_model");
    }
    // created_at/updated_at are written by the CRUD script; a model field of
    // the same name would be silently overwritten on every save.
    if (field.name === "id" || field.name === "created_at" || field.name === "updated_at") {
      throw new Error("field name " + field.name + " is reserved");
    }
  }
}

function saveModel(input) {
  const code = String(input.code || "").toLowerCase();
  if (!/^[a-z][a-z0-9_-]*$/.test(code)) throw new Error("code must match [a-z][a-z0-9_-]*");
  validateFields(input.fields);

  const existing = bkn.store.get(MODELS, code);

  // Adding a field needs no migration: the store is schemaless, so old records
  // simply lack it and the CRUD script fills in defaults on next write. What
  // does need saying out loud is a field that changed type, because records
  // written under the old type will now fail validation on update.
  const warnings = [];
  if (existing) {
    const before = {};
    for (const f of existing.fields) before[f.name] = f;
    for (const f of input.fields) {
      if (before[f.name] && before[f.name].type !== f.type) {
        warnings.push("field " + f.name + " changed type from " +
          before[f.name].type + " to " + f.type + "; existing records may not validate");
      }
      if (f.unique && (!before[f.name] || !before[f.name].unique)) {
        warnings.push("field " + f.name + " became unique; existing duplicates are not checked");
      }
    }
  }

  const saved = bkn.store.put(MODELS, {
    display_name: input.display_name || code,
    description: input.description || "",
    fields: input.fields,
    enabled: input.enabled !== false,
    version: existing ? (existing.version || 1) + 1 : 1,
    updated_at: bkn.now(),
  }, code);

  bkn.events.emit("headless", existing ? "model.updated" : "model.created", {
    level: warnings.length ? "warn" : "info",
    subject: code, data: { version: saved.version, warnings: warnings },
  });
  return {
    model: code, version: saved.version, fields: input.fields.length,
    warnings: warnings, collection: "headless/" + code,
  };
}

function mintToken(spec) {
  if (!spec.name) throw new Error("token needs a name");
  const secret = "hl_" + bkn.crypto.randomHex(24);
  // Only the hash is stored, so a database read does not yield working
  // credentials. The plaintext is returned exactly once.
  bkn.store.put(TOKENS, {
    name: spec.name,
    permissions: spec.permissions || {},
    enabled: true,
    created_at: bkn.now(),
  }, bkn.crypto.hash(secret).slice(0, 32));

  bkn.events.emit("headless", "token.created", {
    subject: spec.name, data: { permissions: spec.permissions || {} },
  });
  return { token: secret, name: spec.name, note: "store this now; it is not recoverable" };
}

function main(input) {
  if (input.token) return mintToken(input.token);
  if (input.list) {
    return {
      models: bkn.store.list(MODELS, { limit: 100 }).map((m) => ({
        code: m.id, display_name: m.display_name, fields: m.fields.length,
        version: m.version, enabled: m.enabled,
      })),
    };
  }
  return saveModel(input);
}
