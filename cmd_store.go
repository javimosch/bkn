package main

import (
	"flag"
	"strings"

	"github.com/javimosch/bkn/internal/out"
	"github.com/javimosch/bkn/internal/store"
)

const storeUsage = "bkn store <create|put|get|find|list|patch|delete|collections> ..."

func cmdStore(args []string) {
	need(args, 1, storeUsage)
	sub, rest := args[0], args[1:]

	conn := open()
	defer conn.Close()
	st := store.New(conn)

	parseRef := func(s string) store.Ref {
		ref, err := store.ParseRef(s)
		if err != nil {
			failStore(err)
		}
		return ref
	}

	switch sub {
	case "create":
		fs := flag.NewFlagSet("store create", flag.ExitOnError)
		var norms repeated
		fs.Var(&norms, "normalize", "field=rule, repeatable ("+strings.Join(store.ValidNormalizers(), "|")+")")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn store create <ns/coll> [--normalize field=rule]")

		rules := map[string]string{}
		for _, n := range norms {
			f, r, ok := strings.Cut(n, "=")
			if !ok {
				out.Fail(out.InvalidValue, "invalid_normalizer", "--normalize takes field=rule, got "+n)
			}
			rules[f] = r
		}
		c, err := st.EnsureCollection(parseRef(pos[0]), rules)
		if err != nil {
			failStore(err)
		}
		out.Data(map[string]any{"collection": c})

	case "put":
		fs := flag.NewFlagSet("store put", flag.ExitOnError)
		data := fs.String("data", "", "JSON object, @file, or - for stdin")
		id := fs.String("id", "", "caller-supplied record id")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn store put <ns/coll> --data <json> [--id <id>]")
		if *data == "" {
			out.Fail(out.ValidationError, "missing_data", "--data is required",
				`bkn store put myapp/users --data '{"email":"a@b.io"}'`)
		}
		rec, err := st.Put(parseRef(pos[0]), *id, readData(*data))
		if err != nil {
			failStore(err)
		}
		out.Data(map[string]any{"record": rec})

	case "get":
		need(rest, 2, "bkn store get <ns/coll> <id>")
		rec, err := st.Get(parseRef(rest[0]), rest[1])
		if err != nil {
			failStore(err)
		}
		out.Data(map[string]any{"record": rec})

	case "find":
		fs := flag.NewFlagSet("store find", flag.ExitOnError)
		var wheres repeated
		fs.Var(&wheres, "where", "field=value, repeatable")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn store find <ns/coll> --where field=value")
		if len(wheres) == 0 {
			out.Fail(out.ValidationError, "missing_filter", "find needs at least one --where",
				"bkn store find myapp/users --where email=a@b.io", "bkn store list myapp/users")
		}
		rec, err := st.Find(parseRef(pos[0]), parseFilters(wheres))
		if err != nil {
			failStore(err)
		}
		out.Data(map[string]any{"record": rec})

	case "list":
		fs := flag.NewFlagSet("store list", flag.ExitOnError)
		var wheres repeated
		fs.Var(&wheres, "where", "field=value, repeatable")
		limit := fs.Int("limit", 50, "maximum records")
		offset := fs.Int("offset", 0, "records to skip")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn store list <ns/coll> [--where field=value] [--limit N] [--offset N]")

		ref := parseRef(pos[0])
		recs, err := st.List(ref, parseFilters(wheres), *limit, *offset)
		if err != nil {
			failStore(err)
		}
		out.Data(map[string]any{
			"collection": ref.String(),
			"count":      len(recs),
			"limit":      *limit,
			"offset":     *offset,
			"records":    recs,
		})

	case "patch":
		fs := flag.NewFlagSet("store patch", flag.ExitOnError)
		data := fs.String("data", "", "JSON object of fields to merge")
		pos := parseFlags(fs, rest)
		need(pos, 2, "bkn store patch <ns/coll> <id> --data <json>")
		if *data == "" {
			out.Fail(out.ValidationError, "missing_data", "--data is required")
		}
		rec, err := st.Patch(parseRef(pos[0]), pos[1], readData(*data))
		if err != nil {
			failStore(err)
		}
		out.Data(map[string]any{"record": rec})

	case "delete":
		need(rest, 2, "bkn store delete <ns/coll> <id>")
		if err := st.Delete(parseRef(rest[0]), rest[1]); err != nil {
			failStore(err)
		}
		out.Data(map[string]any{"deleted": rest[1], "collection": rest[0]})

	case "collections":
		fs := flag.NewFlagSet("store collections", flag.ExitOnError)
		ns := fs.String("ns", "", "restrict to one namespace")
		_ = parseFlags(fs, rest)
		cols, err := st.Collections(*ns)
		if err != nil {
			failStore(err)
		}
		out.Data(map[string]any{"count": len(cols), "collections": cols})

	default:
		out.Fail(out.InvalidArguments, "unknown_command", "unknown store subcommand "+sub, "usage: "+storeUsage)
	}
}

func parseFilters(specs []string) []store.Filter {
	var filters []store.Filter
	for _, s := range specs {
		f, err := store.ParseFilter(s)
		if err != nil {
			out.Fail(out.InvalidValue, "invalid_filter", err.Error(), "--where email=a@b.io")
		}
		filters = append(filters, f)
	}
	return filters
}
