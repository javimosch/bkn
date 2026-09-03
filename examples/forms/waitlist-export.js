// Password-protected public exports of form submissions, as CSV or JSON.
//
// Replaces waitingListPublicExports.service + waitingListJson.service from the
// Node backend (~900 lines plus their controllers and routes).
//
// Install:
//   bkn kv set exports.waitlist_password "a-long-shared-secret" --type encrypted
//   bkn store put forms/exports --id waitlist --data @export-config.json
//   bkn script create waitlist-export --file waitlist-export.js
//   bkn hooks create exports --script waitlist-export --rate-limit 30
//
//   curl "https://host/v1/hooks/exports?name=waitlist&password=..." -o waitlist.csv

const PAGE = 500;

// RFC 4180: quote a field when it contains a delimiter, a quote or a newline,
// and escape embedded quotes by doubling them. Getting this wrong is how an
// address with a comma in it silently shifts every later column.
function csvCell(value) {
  if (value === null || value === undefined) return "";
  const text = typeof value === "object" ? JSON.stringify(value) : String(value);
  if (/[",\r\n]/.test(text)) return '"' + text.replace(/"/g, '""') + '"';
  return text;
}

function toCSV(rows, columns) {
  const out = [columns.join(",")];
  for (const row of rows) {
    out.push(columns.map((c) => csvCell(row[c])).join(","));
  }
  return out.join("\r\n") + "\r\n";
}

function flatten(record) {
  const flat = { id: record.id, submitted_at: record.submitted_at, form: record.form };
  const fields = record.fields || {};
  for (const key of Object.keys(fields)) flat[key] = fields[key];
  return flat;
}

// The shared secret lives in an encrypted kv entry, not as a hash on the
// config: it is a shared password to compare, not a user credential to verify,
// and keeping it out of the database entirely is stronger than hashing it in.
function authorize(cfg, d) {
  if (!cfg.password_setting) return null;
  const expected = bkn.kv.get(cfg.password_setting);
  if (!expected) return { status: 500, body: { error: "export password is not configured" } };

  const header = d.headers["authorization"] || "";
  const supplied = header.indexOf("Bearer ") === 0
    ? header.slice(7)
    : (d.query.password || "");

  if (!bkn.crypto.equal(expected, supplied)) {
    bkn.events.emit("exports", "access.denied", {
      level: "warn", subject: cfg.name || "unknown",
      data: { ip: d.headers["x-forwarded-for"] || null },
    });
    return { status: 401, body: { error: "invalid or missing password" } };
  }
  return null;
}

function collect(formKey, limit) {
  const rows = [];
  let offset = 0;
  while (rows.length < limit) {
    const page = bkn.store.list("forms/submissions", {
      where: { form: formKey }, limit: Math.min(PAGE, limit - rows.length), offset: offset,
    });
    if (page.length === 0) break;
    for (const record of page) rows.push(flatten(record));
    offset += page.length;
    if (page.length < PAGE) break;
  }
  return rows;
}

function main(d) {
  if (d.method !== "GET") {
    return { status: 405, body: { error: "exports are read with GET" } };
  }
  const name = d.query.name;
  if (!name) return { status: 400, body: { error: "pass ?name=<export>" } };

  const cfg = bkn.store.get("forms/exports", name);
  if (!cfg || cfg.enabled === false) {
    return { status: 404, body: { error: "no such export" } };
  }

  const refused = authorize(Object.assign({ name: name }, cfg), d);
  if (refused) return refused;

  const rows = collect(cfg.form, cfg.limit || 5000);
  const columns = cfg.fields && cfg.fields.length
    ? cfg.fields
    : Object.keys(rows[0] || { id: 1, submitted_at: 1 });

  // Access is recorded rather than counted in place: the event log already
  // answers "who pulled this and when" without another collection.
  bkn.events.emit("exports", "access.granted", {
    subject: name, data: { rows: rows.length, format: cfg.format || "csv" },
  });

  const filename = name + "-" + bkn.now().slice(0, 10);
  if ((cfg.format || "csv") === "json") {
    return {
      status: 200,
      body: { export: name, count: rows.length, generated_at: bkn.now(), rows: rows },
    };
  }
  return {
    status: 200,
    body: toCSV(rows, columns),
    headers: {
      "Content-Type": "text/csv; charset=utf-8",
      "Content-Disposition": 'attachment; filename="' + filename + '.csv"',
    },
  };
}
