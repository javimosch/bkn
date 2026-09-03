package main

import (
	"errors"
	"flag"
	"strings"

	"github.com/javimosch/bkn/internal/cron"
	"github.com/javimosch/bkn/internal/events"
	"github.com/javimosch/bkn/internal/files"
	"github.com/javimosch/bkn/internal/out"
	"github.com/javimosch/bkn/internal/script"
	"github.com/javimosch/bkn/internal/store"
	"time"
)

const cronUsage = "bkn cron <create|list|show|update|delete|run|tick> ..."

func failCron(err error) {
	switch {
	case errors.Is(err, cron.ErrNotFound):
		out.Fail(out.NotFound, "not_found", err.Error(), "bkn cron list")
	case errors.Is(err, cron.ErrExists):
		out.Fail(out.Conflict, "already_exists", err.Error())
	case errors.Is(err, cron.ErrBadName):
		out.Fail(out.InvalidValue, "invalid_name", err.Error())
	case errors.Is(err, cron.ErrBadSchedule), errors.Is(err, cron.ErrBadField):
		out.Fail(out.InvalidValue, "invalid_schedule", err.Error(),
			`5 fields ("0 3 * * *"), or `+strings.Join(cron.Shortcuts(), ", "))
	case errors.Is(err, script.ErrNotFound):
		out.Fail(out.NotFound, "no_script", err.Error(), "bkn script list")
	default:
		out.Fail(out.InternalError, "cron_error", err.Error())
	}
}

func cmdCron(args []string) {
	need(args, 1, cronUsage)
	sub, rest := args[0], args[1:]

	conn := open()
	defer conn.Close()

	reg := cron.NewRegistry(conn)
	scripts := script.NewRegistry(conn)
	k := newKV(conn)
	a, err := authFor(conn, k)
	if err != nil {
		failAuth(err)
	}
	eventLog := events.New(conn)
	runner := script.NewRunner(scripts, store.New(conn), k, a,
		files.New(conn, files.NewLocal(""), s3OrNil()), eventLog)
	scheduler := cron.NewScheduler(reg, runner, eventLog)

	switch sub {
	case "create":
		fs := flag.NewFlagSet("cron create", flag.ExitOnError)
		schedule := fs.String("schedule", "", `cron expression or @shortcut, e.g. "0 3 * * *"`)
		scriptName := fs.String("script", "", "the script to run")
		input := fs.String("input", "", "JSON passed to the script's main(input)")
		pos := parseFlags(fs, rest)
		need(pos, 1, `bkn cron create <name> --schedule "0 3 * * *" --script <script>`)
		if *schedule == "" || *scriptName == "" {
			out.Fail(out.ValidationError, "missing_arguments", "--schedule and --script are both required",
				`bkn cron create nightly --schedule "0 3 * * *" --script cleanup`)
		}
		// Catch a typo in the script name now rather than at 3am.
		if _, err := scripts.Get(*scriptName); err != nil {
			failCron(err)
		}
		var payload map[string]any
		if *input != "" {
			payload = readData(*input)
		}
		j, err := reg.Create(cron.Job{
			Name: pos[0], Schedule: *schedule, Script: *scriptName, Input: payload,
		})
		if err != nil {
			failCron(err)
		}
		out.Data(map[string]any{"job": j})

	case "list":
		jobs, err := reg.List()
		if err != nil {
			failCron(err)
		}
		out.Data(map[string]any{"count": len(jobs), "jobs": jobs})

	case "show":
		need(rest, 1, "bkn cron show <name>")
		j, err := reg.Get(rest[0])
		if err != nil {
			failCron(err)
		}
		out.Data(map[string]any{"job": j})

	case "update":
		fs := flag.NewFlagSet("cron update", flag.ExitOnError)
		schedule := fs.String("schedule", "", "replace the schedule")
		scriptName := fs.String("script", "", "replace the script")
		input := fs.String("input", "", "replace the input JSON")
		enable := fs.Bool("enable", false, "enable the job")
		disable := fs.Bool("disable", false, "disable the job")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn cron update <name> [--schedule S] [--script S] [--enable|--disable]")
		if *enable && *disable {
			out.Fail(out.InvalidArguments, "conflicting_flags", "--enable and --disable are mutually exclusive")
		}

		var schedulePtr, scriptPtr *string
		var inputPtr *map[string]any
		var enabled *bool
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "schedule":
				schedulePtr = schedule
			case "script":
				scriptPtr = scriptName
			case "input":
				payload := readData(*input)
				inputPtr = &payload
			case "enable":
				yes := true
				enabled = &yes
			case "disable":
				no := false
				enabled = &no
			}
		})
		j, err := reg.Update(pos[0], schedulePtr, scriptPtr, inputPtr, enabled)
		if err != nil {
			failCron(err)
		}
		out.Data(map[string]any{"job": j})

	case "delete":
		need(rest, 1, "bkn cron delete <name>")
		if err := reg.Delete(rest[0]); err != nil {
			failCron(err)
		}
		out.Data(map[string]any{"deleted": rest[0]})

	case "run":
		need(rest, 1, "bkn cron run <name>")
		res, err := scheduler.RunNow(rest[0])
		if err != nil {
			failCron(err)
		}
		if res.Status != script.StatusOK {
			out.Fail(out.InternalError, "job_failed", res.Error,
				"bkn script runs "+res.Script+" --limit 1")
		}
		out.Data(map[string]any{"result": res})

	case "tick":
		// The escape hatch for anyone who does not want a long-running
		// daemon: point systemd, launchd or a real crontab at this.
		results, err := scheduler.Tick(time.Now())
		if err != nil {
			failCron(err)
		}
		out.Data(map[string]any{"ran": len(results), "results": results})

	default:
		out.Fail(out.InvalidArguments, "unknown_command", "unknown cron subcommand "+sub, "usage: "+cronUsage)
	}
}
