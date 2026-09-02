package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/javimosch/bkn/internal/kv"
	"github.com/javimosch/bkn/internal/out"
)

const kvUsage = "bkn kv <get|set|list|delete|rekey> ..."

func cmdKV(args []string) {
	need(args, 1, kvUsage)
	sub, rest := args[0], args[1:]

	conn := open()
	defer conn.Close()
	k := newKV(conn)

	switch sub {
	case "get":
		need(rest, 1, "bkn kv get <key>")
		e, err := k.Get(rest[0])
		if err != nil {
			failKV(err)
		}
		out.Data(map[string]any{"entry": e})

	case "set":
		fs := flag.NewFlagSet("kv set", flag.ExitOnError)
		typ := fs.String("type", kv.TypeString, strings.Join(kv.ValidTypes(), "|"))
		desc := fs.String("description", "", "what this setting is for")
		public := fs.Bool("public", false, "readable without auth over HTTP")
		stdin := fs.Bool("stdin", false, "read the value from stdin instead of an argument")
		pos := parseFlags(fs, rest)

		var key, value string
		if *stdin {
			need(pos, 1, "bkn kv set <key> --stdin")
			key = pos[0]
			// Reading a secret from stdin keeps it out of the process table
			// and the shell history.
			value = strings.TrimRight(string(readAll()), "\n")
		} else {
			need(pos, 2, "bkn kv set <key> <value> [--type "+strings.Join(kv.ValidTypes(), "|")+"]")
			key, value = pos[0], pos[1]
		}

		e, err := k.Set(key, value, *typ, *desc, *public)
		if err != nil {
			failKV(err)
		}
		if e.Type == kv.TypeEncrypted {
			e.Value = "" // never echo a secret back onto stdout
		}
		out.Data(map[string]any{"entry": e})

	case "list":
		fs := flag.NewFlagSet("kv list", flag.ExitOnError)
		prefix := fs.String("prefix", "", "only keys starting with this")
		public := fs.Bool("public", false, "only entries marked public")
		_ = parseFlags(fs, rest)
		entries, err := k.List(*prefix, *public)
		if err != nil {
			failKV(err)
		}
		out.Data(map[string]any{"count": len(entries), "entries": entries})

	case "delete":
		need(rest, 1, "bkn kv delete <key>")
		if err := k.Delete(rest[0]); err != nil {
			failKV(err)
		}
		out.Data(map[string]any{"deleted": rest[0]})

	case "rekey":
		kr, err := kv.LoadKeyring()
		if err != nil {
			failKV(err)
		}
		out.Log("[rekey] re-encrypting under key %q (available: %s)",
			kr.ActiveKeyID(), strings.Join(kr.KeyIDs(), ", "))
		res, err := k.Rekey()
		if err != nil {
			out.Fail(out.InvalidValue, "rekey_failed", err.Error(),
				"check BKN_ENCRYPTION_KEYS still contains the key that sealed every entry")
		}
		if len(res.Failed) > 0 {
			// Partial rotation is still progress; the exit code says some
			// entries need a key that is no longer configured.
			out.Log("[rekey] %d rekeyed, %d already current, %d unreadable", res.Rekeyed, res.Skipped, len(res.Failed))
			for key, msg := range res.Failed {
				out.Log("[rekey] %s: %s", key, msg)
			}
			out.Fail(out.InvalidValue, "rekey_incomplete",
				fmt.Sprintf("%d of %d entries could not be re-encrypted", len(res.Failed), res.Rekeyed+res.Skipped+len(res.Failed)),
				"add the missing key to BKN_ENCRYPTION_KEYS as <keyId>:<material>")
		}
		out.Data(map[string]any{"rekey": res})

	default:
		out.Fail(out.InvalidArguments, "unknown_command", "unknown kv subcommand "+sub, "usage: "+kvUsage)
	}
}
