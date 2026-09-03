package main

import (
	"database/sql"
	"encoding/base64"
	"io"
	"os"
	"strings"

	"github.com/javimosch/bkn/internal/events"
	"github.com/javimosch/bkn/internal/files"
	"github.com/javimosch/bkn/internal/out"
	"github.com/javimosch/bkn/internal/script"
	"github.com/javimosch/bkn/internal/store"
)

// runnerFor assembles a script runner with every primitive attached. Every
// command that runs a script goes through here, so the sandbox has the same
// capabilities whichever entry point invoked it.
func runnerFor(conn *sql.DB) *script.Runner {
	k := newKV(conn)
	a, err := authFor(conn, k)
	if err != nil {
		failAuth(err)
	}
	return script.NewRunner(
		script.NewRegistry(conn), store.New(conn), k, a,
		files.New(conn, files.NewLocal(""), s3OrNil()), events.New(conn),
	)
}

func eventsFor(conn *sql.DB) *events.Log { return events.New(conn) }

// readRaw resolves a raw (non-JSON) argument: inline, @file, or - for stdin.
// A webhook signature covers exact bytes, so this must not reformat anything.
func readRaw(spec string) string {
	switch {
	case spec == "":
		return ""
	case spec == "-":
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			out.Fail(out.InvalidValue, "unreadable_stdin", err.Error())
		}
		return string(raw)
	case strings.HasPrefix(spec, "@"):
		raw, err := os.ReadFile(spec[1:])
		if err != nil {
			out.Fail(out.InvalidValue, "unreadable_file", err.Error())
		}
		return string(raw)
	default:
		return spec
	}
}

func base64Of(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
