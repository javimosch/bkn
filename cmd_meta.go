package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/javimosch/bkn/internal/auth"
	"github.com/javimosch/bkn/internal/db"
	"github.com/javimosch/bkn/internal/files"
	"github.com/javimosch/bkn/internal/guide"
	"github.com/javimosch/bkn/internal/kv"
	"github.com/javimosch/bkn/internal/out"
	"github.com/javimosch/bkn/internal/script"
	"github.com/javimosch/bkn/internal/store"
)

func readAll() []byte {
	b, err := io.ReadAll(os.Stdin)
	if err != nil {
		out.Fail(out.InvalidValue, "unreadable_stdin", err.Error())
	}
	return b
}

func cmdGuide(args []string) {
	fs := flag.NewFlagSet("guide", flag.ExitOnError)
	human := fs.Bool("human", false, "render as markdown instead of JSON")
	_ = parseFlags(fs, args)

	if *human {
		md, err := guide.Human(Version)
		if err != nil {
			out.Fail(out.InternalError, "guide_error", err.Error())
		}
		fmt.Print(md)
		return
	}
	body, err := guide.Body(Version)
	if err != nil {
		out.Fail(out.InternalError, "guide_error", err.Error())
	}
	out.Raw(map[string]any{"ok": true, "version": out.SchemaVersion, "guide": body})
}

// helpJSON is the machine-readable command catalog (cli-output-spec §4).
func helpJSON() map[string]any {
	c := func(args []string, flags []string) map[string]any {
		return map[string]any{"args": args, "flags": flags, "auth": false}
	}
	none := []string{}
	return map[string]any{
		"version":     Version,
		"schema":      out.SchemaVersion,
		"output":      "json",
		"interactive": false,
		"commands": map[string]any{
			"version":            c(none, none),
			"help-json":          c(none, none),
			"guide":              c(none, []string{"--human"}),
			"store create":       c([]string{"ref"}, []string{"--normalize <field=rule>"}),
			"store put":          c([]string{"ref"}, []string{"--data <json|@file|->", "--id <id>"}),
			"store get":          c([]string{"ref", "id"}, none),
			"store find":         c([]string{"ref"}, []string{"--where <field=value>"}),
			"store list":         c([]string{"ref"}, []string{"--where <field=value>", "--limit <n>", "--offset <n>"}),
			"store patch":        c([]string{"ref", "id"}, []string{"--data <json|@file|->"}),
			"store delete":       c([]string{"ref", "id"}, none),
			"store collections":  c(none, []string{"--ns <namespace>"}),
			"kv get":             c([]string{"key"}, none),
			"kv set":             c([]string{"key", "value"}, []string{"--type <string|json|encrypted>", "--description <text>", "--public", "--stdin"}),
			"kv list":            c(none, []string{"--prefix <p>", "--public"}),
			"kv delete":          c([]string{"key"}, none),
			"kv rekey":           c(none, none),
			"script create":      c([]string{"name"}, []string{"--file <path|->", "--description <text>", "--timeout <ms>", "--allow-net <hosts>"}),
			"script test":        c(none, []string{"--file <path|->", "--input <json>", "--timeout <ms>", "--allow-net <hosts>"}),
			"script run":         c([]string{"name"}, []string{"--input <json|@file|->"}),
			"script list":        c(none, none),
			"script show":        c([]string{"name"}, none),
			"script update":      c([]string{"name"}, []string{"--file <path>", "--description <text>", "--timeout <ms>", "--allow-net <hosts>", "--enable", "--disable"}),
			"script delete":      c([]string{"name"}, none),
			"script runs":        c([]string{"name"}, []string{"--limit <n>"}),
			"auth user create":   c([]string{"email"}, []string{"--password-stdin", "--password <p>", "--name <n>", "--role <user|admin>"}),
			"auth user list":     c(none, []string{"--limit <n>", "--offset <n>"}),
			"auth user show":     c([]string{"email|id"}, none),
			"auth user update":   c([]string{"email|id"}, []string{"--name <n>", "--role <r>", "--password-stdin", "--enable", "--disable"}),
			"auth user delete":   c([]string{"email|id"}, none),
			"auth login":         c([]string{"email"}, []string{"--password-stdin", "--password <p>", "--org <slug>"}),
			"auth me":            c([]string{"access-token"}, none),
			"auth refresh":       c([]string{"refresh-token"}, none),
			"auth switch-org":    c([]string{"refresh-token", "org"}, none),
			"auth logout":        c([]string{"refresh-token"}, none),
			"auth sessions":      c([]string{"user"}, none),
			"auth revoke":        c([]string{"user"}, none),
			"auth memberships":   c([]string{"user"}, none),
			"auth can":           c([]string{"user", "org", "min-role"}, none),
			"auth org create":    c([]string{"slug"}, []string{"--name <n>"}),
			"auth org list":      c(none, none),
			"auth org show":      c([]string{"slug|id"}, none),
			"auth org delete":    c([]string{"slug|id"}, none),
			"auth member add":    c([]string{"org", "user"}, []string{"--role <owner|admin|member>"}),
			"auth member remove": c([]string{"org", "user"}, none),
			"auth member list":   c([]string{"org"}, none),
			"files ns create":    c([]string{"name"}, []string{"--backend <local|s3>", "--max-bytes <n>", "--allow-type <type>", "--public"}),
			"files ns list":      c(none, none),
			"files ns delete":    c([]string{"name"}, none),
			"files put":          c([]string{"namespace", "path"}, []string{"--name <n>", "--content-type <t>", "--meta <json>", "--overwrite", "--stdin"}),
			"files get":          c([]string{"namespace", "name"}, []string{"--out <path>"}),
			"files show":         c([]string{"namespace", "name"}, none),
			"files list":         c([]string{"namespace"}, []string{"--limit <n>", "--offset <n>"}),
			"files delete":       c([]string{"namespace", "name"}, none),
			"serve":              c(none, []string{"--host <h>", "--port <n>"}),
			"daemon start":       c(none, []string{"--host <h>", "--port <n>"}),
			"daemon stop":        c(none, []string{"--host <h>", "--port <n>"}),
			"daemon status":      c(none, []string{"--host <h>", "--port <n>"}),
		},
		"exit_codes": map[string]string{
			"0":   "success",
			"80":  "invalid arguments",
			"82":  "validation error",
			"85":  "invalid argument value",
			"90":  "precondition not met",
			"92":  "not found",
			"100": "connection error",
			"110": "internal error",
		},
		"env": []string{
			"BKN_DATA", "BKN_HOST", "BKN_PORT", "BKN_ADMIN_TOKEN",
			"BKN_ENCRYPTION_KEY", "BKN_ENCRYPTION_KEYS", "BKN_ENCRYPTION_KEY_ID",
			"BKN_SCRIPT_ALLOW_PRIVATE_NET", "BKN_AUTH_SECRET", "BKN_FILES_DIR",
			"S3_ENDPOINT", "S3_REGION", "S3_BUCKET", "S3_ACCESS_KEY_ID", "S3_SECRET_ACCESS_KEY", "S3_FORCE_PATH_STYLE",
			"SUPERBACKEND_ENCRYPTION_KEY", "SAASBACKEND_ENCRYPTION_KEY",
		},
		"defaults": map[string]any{
			"data":              db.Path(),
			"host":              defaultHost,
			"port":              defaultPort,
			"list_limit":        50,
			"kv_types":          kv.ValidTypes(),
			"normalizers":       store.ValidNormalizers(),
			"script_timeout_ms": script.DefaultTimeoutMS,
			"global_roles":      auth.GlobalRoles(),
			"org_roles":         auth.OrgRoles(),
			"access_ttl":        auth.AccessTTL.String(),
			"refresh_ttl":       auth.RefreshTTL.String(),
			"min_password_len":  auth.MinPasswordLen,
			"files_dir":         files.DefaultLocalRoot(),
			"file_backends":     files.Backends(),
			"file_max_bytes":    files.DefaultMaxBytes,
		},
		"see_also": []string{"bkn guide"},
	}
}

func printHelp() {
	fmt.Fprint(os.Stderr, `bkn - single-binary backend core (store + kv over embedded SQLite)

  store   namespaced document collections
    bkn store create <ns/coll> [--normalize field=rule]
    bkn store put <ns/coll> --data <json> [--id <id>]
    bkn store get <ns/coll> <id>
    bkn store find <ns/coll> --where field=value
    bkn store list <ns/coll> [--where field=value] [--limit N] [--offset N]
    bkn store patch <ns/coll> <id> --data <json>
    bkn store delete <ns/coll> <id>
    bkn store collections [--ns <namespace>]

  kv      typed settings, optionally encrypted
    bkn kv get <key>
    bkn kv set <key> <value> [--type string|json|encrypted] [--public]
    bkn kv list [--prefix <p>] [--public]
    bkn kv delete <key>
    bkn kv rekey

  auth    users, organizations, memberships, tokens
    bkn auth user create <email> --password-stdin [--name N] [--role R]
    bkn auth login <email> --password-stdin [--org <slug>]
    bkn auth me <access-token> | refresh <t> | logout <t> | switch-org <t> <org>
    bkn auth org create <slug> | list | show <slug> | delete <slug>
    bkn auth member add <org> <user> [--role R] | remove | list
    bkn auth can <user> <org> <owner|admin|member>
    bkn auth sessions <user> | revoke <user> | memberships <user>

  files   namespaced blob storage, local or S3
    bkn files ns create <name> [--backend local|s3] [--allow-type image/*] [--public]
    bkn files ns list | delete <name>
    bkn files put <ns> <path> [--name N] [--overwrite]
    bkn files get <ns> <name> [--out <path>]
    bkn files show <ns> <name> | list <ns> | delete <ns> <name>

  script  sandboxed JavaScript over store, kv, auth and files
    bkn script create <name> --file <path> [--allow-net hosts] [--timeout MS]
    bkn script test --file <path> [--input <json>]
    bkn script run <name> [--input <json>]
    bkn script list | show <name> | delete <name>
    bkn script update <name> [--file <path>] [--enable|--disable]
    bkn script runs <name> [--limit N]

  server
    bkn serve [--host 127.0.0.1] [--port 7799]
    bkn daemon start|stop|status

  meta
    bkn guide [--human]     the mental model - read this first
    bkn help-json           machine-readable command catalog
    bkn version

Data lives at `+db.Path()+` (override with BKN_DATA).
`)
}
