package main

import (
	"errors"
	"flag"

	"os"
	"strings"

	"github.com/javimosch/bkn/internal/feedback"
	"github.com/javimosch/bkn/internal/out"
	"github.com/javimosch/bkn/internal/telemetry"
	"github.com/javimosch/bkn/internal/update"
)

// --- update ---------------------------------------------------------------

func cmdUpdate(args []string) {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "report whether an update is available, change nothing")
	force := fs.Bool("force", false, "re-download and swap even when the version matches")
	_ = parseFlags(fs, args)

	local, err := update.LocalVersion()
	if err != nil {
		out.Fail(out.InternalError, "version_unavailable", err.Error())
	}

	if *check {
		rel, err := update.Fetch(10_000_000_000)
		if err != nil {
			failUpdate(err)
		}
		if rel.Version == local {
			out.Data(map[string]any{"update_available": false, "version": local})
			return
		}
		// Exit 5 is "an update exists", not a failure - a shell can branch on
		// it without parsing anything.
		out.Log("[update] %s -> %s available. Run: bkn update", local, rel.Version)
		out.Raw(map[string]any{
			"ok": true, "version": out.SchemaVersion,
			"update_available": true, "from": local, "to": rel.Version,
		})
		os.Exit(5)
	}

	res, err := update.Apply(*force, out.Log)
	if err != nil {
		failUpdate(err)
	}
	if !res.Updated {
		out.Data(map[string]any{"updated": false, "version": res.From,
			"note": "already up to date"})
		return
	}
	out.Data(map[string]any{"updated": true, "from": res.From, "to": res.To,
		"path": res.Path, "backup": res.Backup})
}

func failUpdate(err error) {
	switch {
	case errors.Is(err, update.ErrNoArtifact):
		out.Fail(out.NotFound, "no_artifact", err.Error(),
			"the server is reachable but publishing nothing")
	case errors.Is(err, update.ErrVerify):
		out.Fail(out.ExternalError, "verification_failed", err.Error(),
			"the download did not match; nothing was replaced")
	case errors.Is(err, update.ErrSmokeTest):
		out.Fail(out.ExternalError, "smoke_test_failed", err.Error(),
			"the downloaded binary does not run; nothing was replaced")
	default:
		out.Fail(out.ConnectionError, "update_failed", err.Error(),
			"check BKN_SERVER, or retry")
	}
}

func cmdInstall(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	prefix := fs.String("prefix", update.DefaultPrefix(), "directory to install into")
	_ = parseFlags(fs, args)

	dest, err := update.Install(*prefix)
	if err != nil {
		if errors.Is(err, update.ErrPermission) {
			out.Fail(out.NotAuthenticated, "permission_denied", err.Error(),
				"choose a writable --prefix; bkn will not escalate privileges")
		}
		out.Fail(out.InternalError, "install_failed", err.Error())
	}
	if !update.OnPath(*prefix) {
		// Said, not done: installing must not edit anyone's shell config.
		out.Log("[install] %s is not on your PATH; add it to run bkn by name", *prefix)
	}
	out.Data(map[string]any{"installed": dest})
}

func cmdUninstall(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	prefix := fs.String("prefix", update.DefaultPrefix(), "directory to remove from")
	_ = parseFlags(fs, args)

	dest, removed, err := update.Uninstall(*prefix)
	if err != nil {
		out.Fail(out.NotAuthenticated, "permission_denied", err.Error())
	}
	// Removing something that is not there is a success.
	out.Data(map[string]any{"removed": removed, "path": dest})
}

// --- feedback -------------------------------------------------------------

func cmdFeedback(args []string) {
	fs := flag.NewFlagSet("feedback", flag.ExitOnError)
	kind := fs.String("kind", "note", strings.Join(feedback.Kinds(), "|"))
	context := fs.String("context", "", "what you were doing")
	reporter := fs.String("reporter", "", "who is filing this (default: $USER, else agent)")
	pos := parseFlags(fs, args)
	need(pos, 1, `bkn feedback "<message>" [--kind bug|idea|praise|note] [--context "..."]`)

	who := *reporter
	if who == "" {
		who = feedback.Reporter()
	}
	submission := feedback.Submission{
		ID: feedback.NewID(), App: "bkn", Message: strings.Join(pos, " "),
		Kind: *kind, Version: Version, Context: *context, Reporter: who,
	}
	if err := feedback.Validate(submission); err != nil {
		out.Fail(out.ValidationError, "invalid_feedback", err.Error(),
			"--kind "+strings.Join(feedback.Kinds(), "|"))
	}

	res := feedback.Send(submission)
	for _, note := range res.Notes {
		out.Log("[feedback] %s", note)
	}
	if res.Stored == 0 && res.Relayed == 0 {
		// Still exit 0: reporting feedback must never be the thing that fails,
		// or the report is lost along with whatever prompted it.
		out.Log("[feedback] nothing reached a collector; your message was not stored")
	}
	out.Data(map[string]any{"feedback": res})
}

// --- telemetry ------------------------------------------------------------

func cmdTelemetry(args []string) {
	fs := flag.NewFlagSet("telemetry", flag.ExitOnError)
	on := fs.Bool("on", false, "enable telemetry and persist the choice")
	off := fs.Bool("off", false, "disable telemetry and persist the choice")
	_ = parseFlags(fs, args)
	if *on && *off {
		out.Fail(out.InvalidArguments, "conflicting_flags", "--on and --off are mutually exclusive")
	}

	reporter := telemetry.New(update.Home(), Version)
	if *on || *off {
		if err := reporter.SetEnabled(*on); err != nil {
			out.Fail(out.InternalError, "telemetry_error", err.Error())
		}
		if *on {
			for _, line := range strings.Split(telemetryNotice(reporter), "\n") {
				out.Log("%s", line)
			}
		}
	}
	out.Data(map[string]any{"telemetry": reporter.Status("telemetry")})
}

func telemetryNotice(r *telemetry.Reporter) string {
	lines, _ := r.Status("telemetry")["notice"].([]string)
	return strings.Join(lines, "\n")
}

// reportTelemetry is called once, after a command's output is already written.
func reportTelemetry(verb string, exitCode int) {
	defer func() {
		// Telemetry must never be why a command fails.
		_ = recover()
	}()
	telemetry.New(update.Home(), Version).Report(verb, exitCode)
}
