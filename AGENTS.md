# Working on bkn

## What this is

A single static binary providing three backend primitives — `store` (namespaced
document collections), `kv` (typed, optionally encrypted settings), and
`script` (sandboxed JavaScript over both) — on embedded SQLite. The CLI is the
primary interface; HTTP mirrors it.

## The rules that shape the code

1. **Small on purpose.** This replaces an 85k-line Node backend whose feature
   count grew because the core had no escape hatch. Target for the full core
   (store, kv, script, auth, files, events, cron) is ~8k lines. A new feature
   is presumed to belong in userland until proven otherwise — and since
   `script` exists, "userland" is now a real answer rather than a deferral.
   Before adding a Go feature, write it as a script first; if that works, it
   ships as an example, not as core.
2. **Six verbs, no more.** The store surface was derived from an audit of real
   consumers, not from what a database can do. No aggregations, no joins, no
   server-side sorting. If something needs those, it needs the scripts
   primitive, not a wider store.
3. **Single writer.** `bkn` owns its SQLite file. Nothing else opens it. The
   previous system's consumers bypassed the API and opened Mongo directly,
   which froze the data model permanently.
4. **Invariants live in one place.** Field normalization is declared on the
   collection and applied by the store on writes *and* on filter values. Never
   push an invariant out to callers.
5. **Fail loudly, never downgrade.** An encrypted write with no key configured
   is an error, not a plaintext write.
6. **The sandbox boundary must stay readable in one sitting.** Everything a
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

```sh
go test ./...
CGO_ENABLED=0 go build -ldflags "-s -w -X main.Version=$(git describe --tags --always)" -o bin/bkn .
```
