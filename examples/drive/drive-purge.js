// drive-purge: empty the bin of anything binned longer than the grace period.
//
// Run by bkn's own scheduler, which arrives as caller kind "system":
//
//   bkn script create drive-purge --file drive-purge.js
//   bkn cron create drive-purge --schedule '@daily' --script drive-purge
//
// It is a separate script from drive.js on purpose. The drive is a
// user-facing API where every op is authorised against the caller; this runs
// with no user at all and deletes other people's files. Keeping that in its
// own script means no request path can ever reach it, whatever it is sent.

const ENTRIES = "drive/entries";
const USAGE = "drive/usage";
const BLOBS = "drive-blobs";

const BIN_DAYS = 30;
const BATCH = 200;

function main(input) {
  const days = Number(input && input.days) > 0 ? Number(input.days) : BIN_DAYS;
  const cutoff = Date.now() - days * 86400000;
  const dryRun = !!(input && input.dry_run);

  // Every binned entry across every drive. The bin is small by construction --
  // anything older than the grace period leaves on the next run -- so one
  // page is the normal case and the cursor is for the first run after a
  // change of policy.
  const rows = bkn.store.list(ENTRIES, {
    where: { state: "binned" },
    order_by: "deleted",
    limit: BATCH
  });

  const purged = [];
  let bytes = 0;
  let skipped = 0;

  for (let i = 0; i < rows.length; i++) {
    const e = rows[i];
    const when = Date.parse(e.deleted);
    if (!e.deleted || isNaN(when)) {
      // A binned entry with no usable timestamp would otherwise sit there
      // forever. Report it rather than guessing at its age.
      skipped++;
      bkn.log("binned entry", e.id, "has no readable deleted timestamp");
      continue;
    }
    if (when > cutoff) continue; // still inside the grace period

    if (!dryRun) {
      if (e.kind === "file" && e.blob) {
        try { bkn.files.delete(BLOBS, e.blob); } catch (err) { bkn.log("blob delete failed:", err); }
      }
      if (e.kind === "file") {
        const size = Number(e.size) || 0;
        bkn.store.patch(USAGE, e.drive, {
          used_bytes: { $inc: -size }, files: { $inc: -1 }, binned_bytes: { $inc: -size }
        });
      }
      bkn.store.delete(ENTRIES, e.id);
    }
    bytes += Number(e.size) || 0;
    purged.push({ drive: e.drive, name: e.name, deleted: e.deleted });
  }

  return {
    ok: true,
    dry_run: dryRun,
    grace_days: days,
    examined: rows.length,
    purged: purged.length,
    bytes_freed: bytes,
    skipped_without_timestamp: skipped,
    more: rows.length === BATCH,
    entries: purged.slice(0, 25)
  };
}
