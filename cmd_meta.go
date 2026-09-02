package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/javimosch/bkn/internal/db"
	"github.com/javimosch/bkn/internal/guide"
	"github.com/javimosch/bkn/internal/kv"
	"github.com/javimosch/bkn/internal/out"
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
			"version":           c(none, none),
			"help-json":         c(none, none),
			"guide":             c(none, []string{"--human"}),
			"store create":      c([]string{"ref"}, []string{"--normalize <field=rule>"}),
			"store put":         c([]string{"ref"}, []string{"--data <json|@file|->", "--id <id>"}),
			"store get":         c([]string{"ref", "id"}, none),
			"store find":        c([]string{"ref"}, []string{"--where <field=value>"}),
			"store list":        c([]string{"ref"}, []string{"--where <field=value>", "--limit <n>", "--offset <n>"}),
			"store patch":       c([]string{"ref", "id"}, []string{"--data <json|@file|->"}),
			"store delete":      c([]string{"ref", "id"}, none),
			"store collections": c(none, []string{"--ns <namespace>"}),
			"kv get":            c([]string{"key"}, none),
			"kv set":            c([]string{"key", "value"}, []string{"--type <string|json|encrypted>", "--description <text>", "--public", "--stdin"}),
			"kv list":           c(none, []string{"--prefix <p>", "--public"}),
			"kv delete":         c([]string{"key"}, none),
			"kv rekey":          c(none, none),
			"serve":             c(none, []string{"--host <h>", "--port <n>"}),
			"daemon start":      c(none, []string{"--host <h>", "--port <n>"}),
			"daemon stop":       c(none, []string{"--host <h>", "--port <n>"}),
			"daemon status":     c(none, []string{"--host <h>", "--port <n>"}),
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
			"SUPERBACKEND_ENCRYPTION_KEY", "SAASBACKEND_ENCRYPTION_KEY",
		},
		"defaults": map[string]any{
			"data":        db.Path(),
			"host":        defaultHost,
			"port":        defaultPort,
			"list_limit":  50,
			"kv_types":    kv.ValidTypes(),
			"normalizers": store.ValidNormalizers(),
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
