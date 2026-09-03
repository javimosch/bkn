package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"

	"github.com/javimosch/bkn/internal/auth"
	"github.com/javimosch/bkn/internal/files"
	"github.com/javimosch/bkn/internal/out"
	"github.com/javimosch/bkn/internal/script"
	"github.com/javimosch/bkn/internal/store"
)

const scriptUsage = "bkn script <create|list|show|update|delete|run|test|runs> ..."

func failScript(err error) {
	switch {
	case errors.Is(err, script.ErrNotFound):
		out.Fail(out.NotFound, "not_found", err.Error(), "bkn script list")
	case errors.Is(err, script.ErrExists):
		out.Fail(out.Conflict, "already_exists", err.Error(), "bkn script update <name> --file <path>")
	case errors.Is(err, script.ErrBadName):
		out.Fail(out.InvalidValue, "invalid_name", err.Error())
	case errors.Is(err, script.ErrDisabled):
		out.Fail(out.NotAuthenticated, "script_disabled", err.Error(),
			"bkn script update <name> --enable")
	default:
		out.Fail(out.InternalError, "script_error", err.Error())
	}
}

// readCode resolves --file, accepting - for stdin.
func readCode(path string) string {
	var raw []byte
	var err error
	if path == "-" {
		raw, err = io.ReadAll(os.Stdin)
	} else {
		raw, err = os.ReadFile(path)
	}
	if err != nil {
		out.Fail(out.InvalidValue, "unreadable_file", err.Error())
	}
	return string(raw)
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func cmdScript(args []string) {
	need(args, 1, scriptUsage)
	sub, rest := args[0], args[1:]

	conn := open()
	defer conn.Close()
	reg := script.NewRegistry(conn)
	st := store.New(conn)
	k := newKV(conn)
	a, err := auth.New(conn, k)
	if err != nil {
		failAuth(err)
	}
	runner := script.NewRunner(reg, st, k, a, files.New(conn, files.NewLocal(""), s3OrNil()))

	switch sub {
	case "create":
		fs := flag.NewFlagSet("script create", flag.ExitOnError)
		file := fs.String("file", "", "path to the JS file, or - for stdin")
		desc := fs.String("description", "", "what this script does")
		timeout := fs.Int("timeout", script.DefaultTimeoutMS, "run budget in milliseconds")
		allowNet := fs.String("allow-net", "", "comma-separated hosts the script may reach (\"*.example.com\" allowed)")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn script create <name> --file <path>")
		if *file == "" {
			out.Fail(out.ValidationError, "missing_file", "--file is required",
				"bkn script create daily-digest --file digest.js")
		}
		s, err := reg.Create(script.Script{
			Name:        pos[0],
			Code:        readCode(*file),
			Description: *desc,
			TimeoutMS:   *timeout,
			AllowNet:    splitList(*allowNet),
		})
		if err != nil {
			failScript(err)
		}
		s.Code = "" // the caller already has it; keep stdout small
		out.Data(map[string]any{"script": s})

	case "list":
		scripts, err := reg.List()
		if err != nil {
			failScript(err)
		}
		out.Data(map[string]any{"count": len(scripts), "scripts": scripts})

	case "show":
		need(rest, 1, "bkn script show <name>")
		s, err := reg.Get(rest[0])
		if err != nil {
			failScript(err)
		}
		out.Data(map[string]any{"script": s})

	case "update":
		fs := flag.NewFlagSet("script update", flag.ExitOnError)
		file := fs.String("file", "", "replace the code from this file, or - for stdin")
		desc := fs.String("description", "", "replace the description")
		timeout := fs.Int("timeout", 0, "replace the run budget in milliseconds")
		allowNet := fs.String("allow-net", "", "replace the allowed hosts")
		enable := fs.Bool("enable", false, "enable the script")
		disable := fs.Bool("disable", false, "disable the script")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn script update <name> [--file <path>] [--allow-net hosts] [--enable|--disable]")
		if *enable && *disable {
			out.Fail(out.InvalidArguments, "conflicting_flags", "--enable and --disable are mutually exclusive")
		}

		// Only flags the caller actually passed are applied, so editing the
		// code cannot silently reset a timeout or an allowlist.
		var code, description, netList *string
		var timeoutPtr *int
		var enabled *bool
		var allowed []string
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "file":
				c := readCode(*file)
				code = &c
			case "description":
				description = desc
			case "timeout":
				timeoutPtr = timeout
			case "allow-net":
				allowed = splitList(*allowNet)
				netList = allowNet
			case "enable":
				t := true
				enabled = &t
			case "disable":
				f := false
				enabled = &f
			}
		})
		var allowPtr *[]string
		if netList != nil {
			allowPtr = &allowed
		}
		s, err := reg.Update(pos[0], code, description, timeoutPtr, allowPtr, enabled)
		if err != nil {
			failScript(err)
		}
		s.Code = ""
		out.Data(map[string]any{"script": s})

	case "delete":
		need(rest, 1, "bkn script delete <name>")
		if err := reg.Delete(rest[0]); err != nil {
			failScript(err)
		}
		out.Data(map[string]any{"deleted": rest[0]})

	case "run":
		fs := flag.NewFlagSet("script run", flag.ExitOnError)
		input := fs.String("input", "", "JSON passed to main(input): inline, @file, or -")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn script run <name> [--input <json>]")

		res, err := runner.Run(pos[0], scriptInput(*input))
		if err != nil {
			failScript(err)
		}
		emitRun(res)

	case "test":
		fs := flag.NewFlagSet("script test", flag.ExitOnError)
		file := fs.String("file", "", "JS file to run without storing it, or - for stdin")
		input := fs.String("input", "", "JSON passed to main(input)")
		timeout := fs.Int("timeout", script.DefaultTimeoutMS, "run budget in milliseconds")
		allowNet := fs.String("allow-net", "", "hosts this run may reach")
		_ = parseFlags(fs, rest)
		if *file == "" {
			out.Fail(out.ValidationError, "missing_file", "--file is required",
				"bkn script test --file draft.js --input '{}'")
		}
		res, err := runner.Exec(script.Script{
			Code:      readCode(*file),
			TimeoutMS: *timeout,
			AllowNet:  splitList(*allowNet),
			Enabled:   true,
		}, scriptInput(*input))
		if err != nil {
			failScript(err)
		}
		emitRun(res)

	case "runs":
		fs := flag.NewFlagSet("script runs", flag.ExitOnError)
		limit := fs.Int("limit", 20, "how many runs to return")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn script runs <name> [--limit N]")
		runs, err := reg.Runs(pos[0], *limit)
		if err != nil {
			failScript(err)
		}
		out.Data(map[string]any{"count": len(runs), "runs": runs})

	default:
		out.Fail(out.InvalidArguments, "unknown_command", "unknown script subcommand "+sub, "usage: "+scriptUsage)
	}
}

func scriptInput(spec string) any {
	if spec == "" {
		return map[string]any{}
	}
	var raw []byte
	var err error
	switch {
	case spec == "-":
		raw, err = io.ReadAll(os.Stdin)
	case strings.HasPrefix(spec, "@"):
		raw, err = os.ReadFile(spec[1:])
	default:
		raw = []byte(spec)
	}
	if err != nil {
		out.Fail(out.InvalidValue, "unreadable_input", err.Error())
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		out.Fail(out.ValidationError, "invalid_json", "--input must be JSON: "+err.Error())
	}
	return v
}

// emitRun prints the run and exits non-zero when the script itself failed, so
// `bkn script run` can be used directly in a pipeline.
func emitRun(res script.Result) {
	if res.Run.Logs != "" {
		// Script logs are context, not the command's data.
		for _, line := range strings.Split(strings.TrimRight(res.Run.Logs, "\n"), "\n") {
			out.Log("[script] %s", line)
		}
	}
	if !res.OK {
		code := out.InternalError
		typ := "script_failed"
		if res.Run.Status == script.StatusTimeout {
			code, typ = out.ExternalError, "script_timeout"
		}
		// An unsaved `script test` run has no name, so there is no run history
		// to point at.
		var suggestions []string
		if res.Run.Name != "" {
			suggestions = append(suggestions, "bkn script runs "+res.Run.Name+" --limit 1")
		}
		out.Fail(code, typ, res.Run.Error, suggestions...)
	}
	out.Data(map[string]any{"value": res.Value, "run": res.Run})
}
