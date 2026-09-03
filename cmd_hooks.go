package main

import (
	"encoding/json"
	"errors"
	"flag"
	"strings"

	"github.com/javimosch/bkn/internal/hooks"
	"github.com/javimosch/bkn/internal/out"
	"github.com/javimosch/bkn/internal/script"
)

const hooksUsage = "bkn hooks <create|list|show|update|delete|test> ..."

func failHooks(err error) {
	switch {
	case errors.Is(err, hooks.ErrNotFound):
		out.Fail(out.NotFound, "not_found", err.Error(), "bkn hooks list")
	case errors.Is(err, hooks.ErrExists):
		out.Fail(out.Conflict, "already_exists", err.Error())
	case errors.Is(err, hooks.ErrBadName):
		out.Fail(out.InvalidValue, "invalid_name", err.Error())
	case errors.Is(err, hooks.ErrDisabled):
		out.Fail(out.NotAuthenticated, "hook_disabled", err.Error(), "bkn hooks update <name> --enable")
	case errors.Is(err, script.ErrNotFound):
		out.Fail(out.NotFound, "no_script", err.Error(), "bkn script list")
	default:
		out.Fail(out.InternalError, "hooks_error", err.Error())
	}
}

func cmdHooks(args []string) {
	need(args, 1, hooksUsage)
	sub, rest := args[0], args[1:]

	conn := open()
	defer conn.Close()
	reg := hooks.NewRegistry(conn)
	scripts := script.NewRegistry(conn)

	switch sub {
	case "create":
		fs := flag.NewFlagSet("hooks create", flag.ExitOnError)
		scriptName := fs.String("script", "", "the script that handles deliveries")
		maxBytes := fs.Int64("max-bytes", hooks.DefaultMaxBytes, "largest accepted payload")
		rateLimit := fs.Int("rate-limit", 0, "requests per minute per client IP (0 = unlimited)")
		var origins repeated
		fs.Var(&origins, "allow-origin", "browser origin permitted to call this hook, repeatable")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn hooks create <name> --script <script>")
		if *scriptName == "" {
			out.Fail(out.ValidationError, "missing_script", "--script is required",
				"bkn hooks create stripe --script stripe-webhook")
		}
		if _, err := scripts.Get(*scriptName); err != nil {
			failHooks(err)
		}
		h, err := reg.Create(hooks.Hook{
			Name: pos[0], Script: *scriptName, MaxBytes: *maxBytes,
			AllowOrigin: origins, RateLimit: *rateLimit,
		})
		if err != nil {
			failHooks(err)
		}
		// This route is public by design; say so at the moment someone
		// creates one rather than only in the docs.
		out.Log("[hooks] %s is PUBLIC and unauthenticated - %s decides who may write",
			h.Path, h.Script)
		if h.RateLimit == 0 && len(h.AllowOrigin) > 0 {
			// A browser-reachable endpoint with no limit is a spam funnel.
			out.Log("[hooks] %s accepts browser requests with no rate limit; consider --rate-limit", h.Name)
		}
		out.Data(map[string]any{"hook": h})

	case "list":
		list, err := reg.List()
		if err != nil {
			failHooks(err)
		}
		out.Data(map[string]any{"count": len(list), "hooks": list})

	case "show":
		need(rest, 1, "bkn hooks show <name>")
		h, err := reg.Get(rest[0])
		if err != nil {
			failHooks(err)
		}
		out.Data(map[string]any{"hook": h})

	case "update":
		fs := flag.NewFlagSet("hooks update", flag.ExitOnError)
		scriptName := fs.String("script", "", "replace the handling script")
		maxBytes := fs.Int64("max-bytes", 0, "replace the payload limit")
		enable := fs.Bool("enable", false, "enable the hook")
		disable := fs.Bool("disable", false, "disable the hook")
		rateLimit := fs.Int("rate-limit", 0, "replace the per-minute limit")
		var origins repeated
		fs.Var(&origins, "allow-origin", "replace the permitted browser origins")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn hooks update <name> [--script S] [--max-bytes N] [--enable|--disable]")
		if *enable && *disable {
			out.Fail(out.InvalidArguments, "conflicting_flags", "--enable and --disable are mutually exclusive")
		}

		var scriptPtr *string
		var bytesPtr *int64
		var enabled *bool
		var originsPtr *[]string
		var ratePtr *int
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "script":
				scriptPtr = scriptName
			case "max-bytes":
				bytesPtr = maxBytes
			case "rate-limit":
				ratePtr = rateLimit
			case "allow-origin":
				list := []string(origins)
				originsPtr = &list
			case "enable":
				yes := true
				enabled = &yes
			case "disable":
				no := false
				enabled = &no
			}
		})
		h, err := reg.Update(pos[0], scriptPtr, bytesPtr, enabled, originsPtr, ratePtr)
		if err != nil {
			failHooks(err)
		}
		out.Data(map[string]any{"hook": h})

	case "delete":
		need(rest, 1, "bkn hooks delete <name>")
		if err := reg.Delete(rest[0]); err != nil {
			failHooks(err)
		}
		out.Data(map[string]any{"deleted": rest[0]})

	case "test":
		// Replay a delivery locally, so a signature check can be developed
		// without a public URL or a provider's retry schedule.
		fs := flag.NewFlagSet("hooks test", flag.ExitOnError)
		body := fs.String("body", "", "raw request body: inline, @file, or -")
		method := fs.String("method", "POST", "request method to simulate")
		var headers repeated
		fs.Var(&headers, "header", "header as name=value, repeatable")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn hooks test <name> --body <raw> [--header name=value]")

		h, err := reg.Get(pos[0])
		if err != nil {
			failHooks(err)
		}
		delivery := hooks.Delivery{
			Hook: h.Name, Method: *method, Query: map[string]string{},
			Headers: map[string]string{}, Body: readRaw(*body),
		}
		delivery.BodyBase64 = base64Of(delivery.Body)
		for _, raw := range headers {
			name, value, ok := strings.Cut(raw, "=")
			if !ok {
				out.Fail(out.InvalidValue, "invalid_header", "--header takes name=value, got "+raw)
			}
			delivery.Headers[strings.ToLower(strings.TrimSpace(name))] = value
		}

		dispatcher := hooks.NewDispatcher(reg, runnerFor(conn), eventsFor(conn))
		res, err := dispatcher.Deliver(h, delivery)
		if err != nil {
			failHooks(err)
		}
		encoded, _ := json.Marshal(res.Body)
		if res.Status >= 400 {
			out.Fail(out.InternalError, "hook_failed", string(encoded))
		}
		out.Data(map[string]any{"status": res.Status, "body": res.Body})

	default:
		out.Fail(out.InvalidArguments, "unknown_command", "unknown hooks subcommand "+sub, "usage: "+hooksUsage)
	}
}
