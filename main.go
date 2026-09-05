// Command bkn is a single-binary backend core: namespaced document
// collections and typed settings over embedded SQLite, driven from the CLI.
//
// It conforms to the agent-first CLI spec family (https://cli-specs.intrane.fr):
// cli-output-spec (stdout=data, stderr=context, semantic exit codes, typed
// errors, help-json), cli-guide-spec (embedded `guide`), and cli-daemon-spec
// (serve/_health/_shutdown/daemon start|stop|status).
package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/javimosch/bkn/internal/db"
	"github.com/javimosch/bkn/internal/kv"
	"github.com/javimosch/bkn/internal/out"
	"github.com/javimosch/bkn/internal/store"
	"github.com/javimosch/bkn/internal/update"
)

// Version is overridden at build time: -ldflags "-X main.Version=1.2.3".
var Version = "0.1.0-dev"

const (
	defaultHost = "127.0.0.1"
	defaultPort = 7799
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		printHelp()
		os.Exit(out.InvalidArguments)
	}

	cmd, rest := args[0], args[1:]

	// Telemetry is recorded once, after the command has produced its output -
	// including on the error path, which Fail routes back here.
	out.SetExitHook(func(code int) { reportTelemetry(cmd, code) })
	defer func() { reportTelemetry(cmd, 0) }()

	switch cmd {
	case "version", "--version", "-v":
		out.Data(map[string]any{"tool": "bkn", "tool_version": Version})
	case "help", "--help", "-h":
		printHelp()
	case "help-json", "--help-json":
		out.Raw(helpJSON())
	case "guide":
		cmdGuide(rest)
	case "store":
		cmdStore(rest)
	case "kv":
		cmdKV(rest)
	case "script":
		cmdScript(rest)
	case "auth":
		cmdAuth(rest)
	case "files":
		cmdFiles(rest)
	case "events":
		cmdEvents(rest)
	case "cron":
		cmdCron(rest)
	case "hooks":
		cmdHooks(rest)
	case "update":
		update.Nudge()
		cmdUpdate(rest)
	case "install":
		cmdInstall(rest)
	case "uninstall":
		cmdUninstall(rest)
	case "feedback":
		update.Nudge()
		cmdFeedback(rest)
	case "telemetry":
		cmdTelemetry(rest)
	case "lock":
		cmdLock(rest)
	case "serve":
		cmdServe(rest)
	case "daemon":
		cmdDaemon(rest)
	default:
		out.Fail(out.InvalidArguments, "unknown_command",
			fmt.Sprintf("unknown command %q", cmd), "bkn help-json", "bkn guide")
	}
}

// --- shared plumbing ------------------------------------------------------

// open connects to the datastore. Every command that touches data goes
// through here, so there is exactly one place that resolves the path.
func open() *sql.DB {
	conn, err := db.Open()
	if err != nil {
		out.Fail(out.InternalError, "storage_error", err.Error(),
			"check BKN_DATA points at a writable path")
	}
	return conn
}

func newKV(conn *sql.DB) *kv.KV {
	// A missing keyring is not fatal here: only encrypted entries need it,
	// and those fail loudly at the point of use rather than silently
	// downgrading to plaintext.
	kr, err := kv.LoadKeyring()
	if err != nil && !errors.Is(err, kv.ErrNoKey) {
		out.Fail(out.InvalidValue, "bad_encryption_key", err.Error(),
			"BKN_ENCRYPTION_KEY must be hex-64, base64-32, or 32 literal chars")
	}
	return kv.New(conn, kr, 0)
}

// readData resolves --data: inline JSON, @file, or - for stdin.
func readData(spec string) map[string]any {
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
		out.Fail(out.InvalidValue, "unreadable_data", err.Error())
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		out.Fail(out.ValidationError, "invalid_json",
			"--data must be a JSON object: "+err.Error(),
			`--data '{"field":"value"}'`, "--data @file.json", "--data -")
	}
	return doc
}

// repeated collects a flag that may appear more than once.
type repeated []string

func (r *repeated) String() string     { return strings.Join(*r, ",") }
func (r *repeated) Set(v string) error { *r = append(*r, v); return nil }

// failStore maps a store error to the right exit code, once, for every caller.
func failStore(err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		out.Fail(out.NotFound, "not_found", err.Error())
	case errors.Is(err, store.ErrNoCollection):
		out.Fail(out.NotFound, "no_collection", err.Error(), "bkn store collections")
	case errors.Is(err, store.ErrBadRef):
		out.Fail(out.InvalidArguments, "invalid_ref", err.Error(), "bkn store put myapp/users --data '{}'")
	case errors.Is(err, store.ErrBadDoc):
		out.Fail(out.ValidationError, "invalid_document", err.Error())
	case errors.Is(err, store.ErrPrecondition):
		out.Fail(out.Conflict, "precondition_failed", err.Error(),
			"bkn store get <ns/coll> <id>")
	case errors.Is(err, store.ErrConcurrent):
		out.Fail(out.Conflict, "concurrent_update", err.Error())
	case errors.Is(err, store.ErrBadOperator):
		out.Fail(out.InvalidValue, "invalid_operator", err.Error(),
			"valid operators: "+strings.Join(store.Operators(), ", "))
	case errors.Is(err, store.ErrOperandType):
		out.Fail(out.ValidationError, "invalid_operand", err.Error())
	case errors.Is(err, store.ErrBadNormalizer):
		out.Fail(out.InvalidValue, "invalid_normalizer", err.Error(),
			"valid normalizers: "+strings.Join(store.ValidNormalizers(), ", "))
	default:
		out.Fail(out.InternalError, "storage_error", err.Error())
	}
}

func failKV(err error) {
	switch {
	case errors.Is(err, kv.ErrNotFound):
		out.Fail(out.NotFound, "not_found", err.Error())
	case errors.Is(err, kv.ErrNoKey):
		out.Fail(out.NotAuthenticated, "no_encryption_key",
			"no encryption key configured, so encrypted entries cannot be read or written",
			"export BKN_ENCRYPTION_KEY=$(openssl rand -hex 32)")
	case errors.Is(err, kv.ErrBadType):
		out.Fail(out.InvalidValue, "invalid_type", err.Error(),
			"--type "+strings.Join(kv.ValidTypes(), "|"))
	case errors.Is(err, kv.ErrBadJSON):
		out.Fail(out.ValidationError, "invalid_json", err.Error())
	default:
		out.Fail(out.InvalidValue, "kv_error", err.Error())
	}
}

func need(args []string, n int, usage string) {
	if len(args) < n {
		out.Fail(out.InvalidArguments, "invalid_arguments", "usage: "+usage)
	}
}
