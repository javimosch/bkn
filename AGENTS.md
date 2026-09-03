# Working on bkn

## What this is

A single static binary providing seven backend primitives — `store` (namespaced
document collections), `kv` (typed, optionally encrypted settings), `auth`
(users, organizations, memberships, tokens), `files` (namespaced blobs, local
or S3), `events` (append-only log), `cron` (scheduled scripts), and `script`
(sandboxed JavaScript over all of them) — on embedded SQLite. The CLI is the
primary interface; HTTP mirrors it.

## The rules that shape the code

1. **Small on purpose.** This replaces an 85k-line Node backend whose feature
   count grew because the core had no escape hatch. Target for the full core
   (store, kv, auth, files, events, cron, script) was ~8k lines; it landed
   near that, and the number is a point of attention rather than a gate. A new feature
   is presumed to belong in userland until proven otherwise — and since
   `script` exists, "userland" is now a real answer rather than a deferral.
   Before adding a Go feature, write it as a script first; if that works, it
   ships as an example, not as core.
2. **Six verbs, no more.** The store surface was derived from an audit of real
   consumers, not from what a database can do. The `list` predicate was later
   widened - six comparison operators, one sort field, a total - because a
   content API genuinely needs ordering and ranges and doing them in a script
   means loading the collection into the VM. That is the whole allowance: no
   `$or`, no regex, no nesting, no joins, no multi-field sort, no aggregation.
   Anything past it belongs in a script.
3. **Single writer.** `bkn` owns its SQLite file. Nothing else opens it. The
   previous system's consumers bypassed the API and opened Mongo directly,
   which froze the data model permanently.
4. **Invariants live in one place.** Field normalization is declared on the
   collection and applied by the store on writes *and* on filter values. Never
   push an invariant out to callers.
5. **Fail loudly, never downgrade.** An encrypted write with no key configured
   is an error, not a plaintext write.
6. **Identity is not billing, and not anything else that happens to hang off a
   user.** `auth` grew a subscription field in the previous system and that is
   precisely what made it unchangeable. Anything that is not needed to answer
   "who is this and what may they do" belongs in a store collection.
7. **The sandbox boundary must stay readable in one sitting.** Everything a
   script can reach is in `internal/script/host.go`. Keep it that way; a
   capability added anywhere else is a capability nobody will audit.

## Spec conformance is not optional

Every command follows <https://cli-specs.intrane.fr>: cli-output-spec,
cli-guide-spec, cli-daemon-spec. Concretely, when adding a command:

- data to stdout via `out.Data`, context to stderr via `out.Log`, never mixed;
- errors via `out.Fail` with a semantic code (80–119), a stable snake_case
  `type`, and `suggestions` holding the exact command that fixes it;
- never retry internally — report `recoverable` and let the caller decide;
- add the command to `helpJSON()` in `cmd_meta.go` **and** to the embedded
  guide in `internal/guide/guide.json`; both have tests that check coverage.

## Layout

```
main.go              dispatch
flags.go             permuting flag parser (stdlib flag stops at positionals)
cmd_store.go         store subcommands
cmd_kv.go            kv subcommands
cmd_script.go        script subcommands
cmd_auth.go          auth subcommands
cmd_files.go         files subcommands
cmd_events.go        events subcommands
cmd_cron.go          cron subcommands
cmd_hooks.go         hooks subcommands
cmd_lock.go          lock subcommands
cmd_lifecycle.go     update, install, uninstall, feedback, telemetry
wiring.go            shared construction of a fully-equipped script runner
cmd_server.go        serve + daemon subcommands
cmd_meta.go          guide, help-json, help
internal/out/        output contract: envelopes, exit codes, typed errors
internal/db/         SQLite open + schema; the one place a path is resolved
internal/store/      collection primitive (six verbs) + ULID ids
internal/kv/         settings primitive + AES-256-GCM keyring
internal/guide/      embedded guide.json (go:embed) + markdown renderer
internal/server/     HTTP routes, /_health, /_shutdown, /guide, /llms.txt
internal/daemon/     start/stop/status over health probing
```

## Gotchas found the hard way

- **The stdlib `flag` package stops parsing at the first positional argument.**
  `store put myapp/users --data '{}'` silently dropped `--data`. Every
  subcommand must use `parseFlags(fs, args)` from `flags.go`, never
  `fs.Parse`. There is a test for this.
- **ULIDs only sort across milliseconds** unless the random component is
  incremented within one. `store list` breaks `updated_at` ties by id, so
  without monotonic ids two records written in the same millisecond could swap
  places between pages.
- **Check access on metadata, not on the decrypted value.** `kv.Meta` exists
  because decrypting before the access check let a decrypt error reveal that a
  private key existed.
- **`modernc.org/sqlite`, not `mattn/go-sqlite3`.** The latter needs cgo, which
  costs the static single binary. `go.mod` pins a toolchain newer than the
  system Go; `GOTOOLCHAIN=auto` (the default) fetches it.
- **Do not route a forced state change through a validating update.** The
  scheduler disabled a job with an unparseable schedule by calling
  `Registry.Update`, which re-parses the schedule and refuses — so the disable
  silently failed and the job was retried on every tick forever. There is a
  dedicated `disable` for exactly this, and a test that catches the
  regression.
- **`events stats --by` interpolates a column name into SQL**, because a
  GROUP BY column cannot be a bound parameter. It is selected from a fixed map
  and there is a test that feeds it injection strings. Never widen that map to
  arbitrary input.
- **New columns need an explicit `ALTER`.** `CREATE TABLE IF NOT EXISTS` does
  nothing to a database that already has the table, so a column added later
  goes in `addedColumns` in `internal/db`, which tolerates "duplicate column
  name" and fails on anything else.
- **`/v1/hooks/{name}` is the only public write route, and that is deliberate.**
  Anything added to `hooksRoutes` is reachable by the internet with no
  credential. The bound script is the authorization boundary.
- **Signature comparison must be constant time.** `bkn.crypto.equal` wraps
  `subtle.ConstantTimeCompare`; a script using `===` leaks the correct prefix.
- **A JavaScript string is UTF-8.** Handing goja arbitrary bytes replaces every
  invalid sequence with U+FFFD, silently. `http.fetch` needs
  `responseEncoding: "base64"` for anything non-text, and hook deliveries carry
  `body_base64` alongside `body` for the same reason.
- **Second-resolution timestamps make short-interval tests flaky.** A prune
  test slept 1.1s for a 1s cutoff; whenever the events landed late in their
  second the margin rounded to zero. The sleep must exceed the age by a full
  second.
- **The id is a column, not a document field.** `splitID` strips it before
  writing and `decode` merges it back on read, so a filter on `id` has to
  target the column; going through `json_extract` returns NULL and silently
  matches nothing. That bug made every attempt to batch-resolve references
  return raw ids.
- **You cannot truncate-write a running binary** — Linux returns ETXTBSY.
  Both `update` and `install` stage beside the target and `rename`, which
  swaps the directory entry without disturbing the running inode.
- **A hash check is not enough before a swap.** A truncated publish is
  self-consistent: the advertised hash was computed over the truncated bytes.
  The downloaded binary is executed with `version` before it replaces
  anything.
- **Telemetry is opt-in here and should stay that way.** bkn holds encrypted
  kv values, bcrypt hashes and the token signing secret. Adding a field to the
  payload is a spec-level decision, not a convenience: the allow-list in
  `internal/telemetry` is the whole contract and a test enumerates it.
- **Never derive a storage path from a user-supplied name.** Blobs are stored
  under their content hash, which makes traversal structurally impossible
  instead of a validation problem. The name check in `ValidateName` is a
  second line, not the first.
- **Serving uploads inline from the API origin is stored XSS.** `inlineTypes`
  in `internal/server/files_routes.go` is an allow-list, and SVG is excluded
  from it deliberately despite being an image. Adding a type to that map is a
  security decision.
- **Lazy-resolve credentials.** `auth.New` used to generate and store the token
  signing secret eagerly, so `bkn script test` provisioned a credential it
  never used and printed a warning about it. Anything that creates a secret
  belongs behind first use.
- **A silent no-op edit left `help-json` incomplete for a whole release.** A
  scripted edit anchored on a line whose spacing `gofmt` had since changed
  matched nothing, and the spot-check test only asserted five command names,
  so an entire namespace was missing from the catalog while every test passed.
  `TestGuideAndHelpJSONAgree` now enforces a bijection between the embedded
  guide and the catalog in both directions. Always verify a scripted edit
  actually applied.
- **The JWT verifier pins HS256 rather than reading `alg` from the token.**
  That is deliberate and tested — `alg: none` is the classic way to turn a
  signed token into an unsigned one.
- **A goja `Runtime` is not safe for concurrent use.** Every run builds a fresh
  one, which also stops one script leaving state behind for the next.
- **goja appends a Go symbol chain to host errors.** `cleanError` strips the
  internal frames and keeps the JS ones; without it a script author debugging
  their own code reads `(*Runner).newHost.(*Runner).fetchFunc.func19`.
- **A hostname allowlist alone does not stop SSRF.** An allow-listed name can
  resolve to `169.254.169.254`. The dial-time IP check in `guardedTransport`
  is what actually closes it, including against a DNS answer that changes
  between the check and the connection.
- **The encrypted payload format is a contract**, shared with the Node backend
  and with external processes. Changing a field name or the encoding orphans
  every stored secret. Bidirectional compatibility is verified in the tests.

## Build and test

The S3 signer is verified against a live server, not just a stub. Run it when
touching `internal/files/s3.go`:

```sh
docker run -d --rm -p 9123:9000 -e MINIO_ROOT_USER=minioadmin \
  -e MINIO_ROOT_PASSWORD=minioadmin quay.io/minio/minio server /data
BKN_S3_TEST_ENDPOINT=http://127.0.0.1:9123 go test ./internal/files/
```

```sh
go test ./...
CGO_ENABLED=0 go build -ldflags "-s -w -X main.Version=$(git describe --tags --always)" -o bin/bkn .
```
