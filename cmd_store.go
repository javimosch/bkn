package main

import (
	"flag"
	"strings"

	"github.com/javimosch/bkn/internal/out"
	"github.com/javimosch/bkn/internal/store"
)

const storeUsage = "bkn store <create|access|put|get|find|list|count|patch|delete|collections> ..."

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
		var norms, retainPer, accessRules repeated
		fs.Var(&norms, "normalize", "field=rule, repeatable ("+strings.Join(store.ValidNormalizers(), "|")+")")
		fs.Var(&retainPer, "retain-per", "partition the bound by this field, repeatable or comma-separated")
		fs.Var(&accessRules, "access", "verb=audience, repeatable or comma-separated ("+
			strings.Join(store.Verbs(), "|")+" = "+strings.Join(store.Audiences(), "|")+")")
		retainLast := fs.String("retain-last", "", "keep at most N documents, newest first; 0 removes the bound")
		ownerField := fs.String("owner-field", "", "document field holding the owning user id, for --access ...=owner")
		orgField := fs.String("org-field", "", "document field holding the owning org id, for --access ...=org")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn store create <ns/coll> [--normalize field=rule] [--retain-last N [--retain-per field]] "+
			"[--access read=owner --owner-field user_id]")

		rules := map[string]string{}
		for _, n := range norms {
			f, r, ok := strings.Cut(n, "=")
			if !ok {
				out.Fail(out.InvalidValue, "invalid_normalizer", "--normalize takes field=rule, got "+n)
			}
			rules[f] = r
		}
		setRetain := *retainLast != "" || len(retainPer) > 0
		retain, err := store.ParseRetention(*retainLast, retainPer)
		if err != nil {
			failStore(err)
		}
		ref := parseRef(pos[0])
		setAccess := len(accessRules) > 0 || *ownerField != "" || *orgField != ""
		acc, err := store.ParseAccess(accessRules, *ownerField, *orgField)
		if err != nil {
			failStore(err)
		}
		c, err := st.EnsureCollectionWith(ref, rules, retain, setRetain)
		if err != nil {
			failStore(err)
		}
		if setAccess {
			if c, err = st.SetAccess(ref, acc); err != nil {
				failStore(err)
			}
		}
		out.Data(map[string]any{"collection": c})

	case "access":
		fs := flag.NewFlagSet("store access", flag.ExitOnError)
		var accessRules repeated
		fs.Var(&accessRules, "access", "verb=audience, repeatable or comma-separated ("+
			strings.Join(store.Verbs(), "|")+" = "+strings.Join(store.Audiences(), "|")+")")
		ownerField := fs.String("owner-field", "", "document field holding the owning user id")
		orgField := fs.String("org-field", "", "document field holding the owning org id")
		clear := fs.Bool("clear", false, "remove the policy, returning the collection to admin-only")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn store access <ns/coll> [--access read=owner --owner-field user_id] [--clear]")
		ref := parseRef(pos[0])

		// With no flags this reads rather than writes, so an operator can ask
		// what a collection currently allows without risking changing it.
		if !*clear && len(accessRules) == 0 && *ownerField == "" && *orgField == "" {
			c, err := st.Describe(ref)
			if err != nil {
				failStore(err)
			}
			out.Data(map[string]any{"collection": c.Ref, "access": c.Access, "rules": c.Access.String()})
			return
		}
		acc := store.Access{}
		if !*clear {
			var err error
			if acc, err = store.ParseAccess(accessRules, *ownerField, *orgField); err != nil {
				failStore(err)
			}
		}
		c, err := st.SetAccess(ref, acc)
		if err != nil {
			failStore(err)
		}
		out.Data(map[string]any{"collection": c})

	case "put":
		fs := flag.NewFlagSet("store put", flag.ExitOnError)
		data := fs.String("data", "", "JSON object, @file, or - for stdin")
		id := fs.String("id", "", "caller-supplied record id")
		ifAbsent := fs.Bool("if-absent", false, "only insert when the id is free")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn store put <ns/coll> --data <json> [--id <id>]")
		if *data == "" {
			out.Fail(out.ValidationError, "missing_data", "--data is required",
				`bkn store put myapp/users --data '{"email":"a@b.io"}'`)
		}
		if *ifAbsent {
			rec, created, err := st.PutIfAbsent(parseRef(pos[0]), *id, readData(*data))
			if err != nil {
				failStore(err)
			}
			// The exit code carries the answer, so a shell can branch on
			// "did I win the race" without parsing anything.
			if !created {
				out.Fail(out.Conflict, "already_exists",
					"a record with that id already exists", "bkn store get "+pos[0]+" "+rec["id"].(string))
			}
			out.Data(map[string]any{"record": rec, "created": true})
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
		fs.Var(&wheres, "where", "predicate, repeatable: field=value, field>value, field:in=a,b")
		orderBy := fs.String("order-by", "", "document field to sort by, optionally field:desc")
		limit := fs.Int("limit", 50, "maximum records")
		offset := fs.Int("offset", 0, "records to skip")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn store list <ns/coll> [--where field=value] [--order-by field:desc] [--limit N]")

		ref := parseRef(pos[0])
		filters := parseFilters(wheres)
		field, desc := parseOrderBy(*orderBy)
		recs, err := st.List(ref, store.ListOptions{
			Filters: filters, OrderBy: field, Desc: desc, Limit: *limit, Offset: *offset,
		})
		if err != nil {
			failStore(err)
		}
		total, err := st.Count(ref, filters)
		if err != nil {
			failStore(err)
		}
		out.Data(map[string]any{
			"collection": ref.String(),
			"count":      len(recs),
			"total":      total,
			"limit":      *limit,
			"offset":     *offset,
			"order_by":   *orderBy,
			"records":    recs,
		})

	case "count":
		fs := flag.NewFlagSet("store count", flag.ExitOnError)
		var wheres repeated
		fs.Var(&wheres, "where", "predicate, repeatable: field=value, field>value, field:in=a,b")
		by := fs.String("by", "", "group by this document field; omit for a plain total")
		limit := fs.Int("limit", store.DefaultRollupLimit, "maximum buckets returned")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn store count <ns/coll> [--where field=value] [--by field]")

		ref := parseRef(pos[0])
		filters := parseFilters(wheres)
		if *by == "" {
			total, err := st.Count(ref, filters)
			if err != nil {
				failStore(err)
			}
			out.Data(map[string]any{"collection": ref.String(), "total": total})
			return
		}
		rollup, err := st.CountBy(ref, filters, *by, *limit)
		if err != nil {
			failStore(err)
		}
		out.Data(map[string]any{
			"collection": ref.String(), "by": rollup.By, "total": rollup.Total,
			"groups": rollup.Groups, "truncated": rollup.Truncated(), "buckets": rollup.Buckets,
		})

	case "patch":
		fs := flag.NewFlagSet("store patch", flag.ExitOnError)
		data := fs.String("data", "", "JSON object of fields to merge; a field may be an operator such as {\"$inc\":1}")
		var conds, absent repeated
		fs.Var(&conds, "if", "precondition, repeatable and ANDed: field=value")
		fs.Var(&absent, "if-absent", "field must be missing, null or empty; repeatable")
		pos := parseFlags(fs, rest)
		need(pos, 2, "bkn store patch <ns/coll> <id> --data <json>")
		if *data == "" {
			out.Fail(out.ValidationError, "missing_data", "--data is required")
		}
		opts := store.PatchOptions{IfAbsent: absent}
		for _, c := range conds {
			field, want, ok := strings.Cut(c, "=")
			if !ok || field == "" {
				out.Fail(out.InvalidValue, "invalid_precondition",
					"--if must be field=value, got "+c,
					`bkn store patch app/runs r1 --data '{"status":"done"}' --if status=running`)
			}
			if opts.If == nil {
				opts.If = map[string]string{}
			}
			opts.If[field] = want
		}
		rec, err := st.PatchWith(parseRef(pos[0]), pos[1], readData(*data), opts)
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

// parseOrderBy reads "price" or "price:desc". Recency remains the default, so
// an omitted flag behaves exactly as it did before ordering existed.
func parseOrderBy(spec string) (string, bool) {
	if spec == "" {
		return "", true
	}
	field, direction, ok := strings.Cut(spec, ":")
	if !ok {
		return field, false
	}
	switch direction {
	case "desc":
		return field, true
	case "asc":
		return field, false
	default:
		out.Fail(out.InvalidValue, "invalid_order",
			"--order-by takes <field>, <field>:asc or <field>:desc, got "+spec)
		return "", false
	}
}

func parseFilters(specs []string) []store.Filter {
	var filters []store.Filter
	for _, s := range specs {
		f, err := store.ParseFilter(s)
		if err != nil {
			out.Fail(out.InvalidValue, "invalid_filter", err.Error(),
				"--where email=a@b.io", "--where price>20", "--where status:in=draft,live")
		}
		filters = append(filters, f)
	}
	return filters
}
