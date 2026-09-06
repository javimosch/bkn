package main

import (
	"errors"
	"flag"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/javimosch/bkn/internal/files"
	"github.com/javimosch/bkn/internal/out"
)

const filesUsage = "bkn files <ns|put|get|show|list|delete> ..."

func failFiles(err error) {
	switch {
	case errors.Is(err, files.ErrNotFound):
		out.Fail(out.NotFound, "not_found", err.Error(), "bkn files list <namespace>")
	case errors.Is(err, files.ErrNoNamespace):
		out.Fail(out.NotFound, "no_namespace", err.Error(), "bkn files ns create <name>")
	case errors.Is(err, files.ErrExists):
		out.Fail(out.Conflict, "already_exists", err.Error(), "pass --overwrite to replace it")
	case errors.Is(err, files.ErrTooLarge):
		out.Fail(out.InvalidValue, "too_large", err.Error(),
			"raise the namespace limit: bkn files ns create <ns> --max-bytes N")
	case errors.Is(err, files.ErrTypeRefused):
		out.Fail(out.InvalidValue, "type_refused", err.Error())
	case errors.Is(err, files.ErrBadName), errors.Is(err, files.ErrBadNamespace):
		out.Fail(out.InvalidValue, "invalid_name", err.Error())
	case errors.Is(err, files.ErrBadBackend):
		out.Fail(out.NotAuthenticated, "backend_unavailable", err.Error(),
			"configure S3_BUCKET, S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY to enable the s3 backend")
	default:
		out.Fail(out.InternalError, "files_error", err.Error())
	}
}

// s3OrNil registers the S3 backend only when it is configured, so
// `files ns create --backend s3` fails with a clear message up front rather
// than at the first upload.
func s3OrNil() files.Backend {
	if s := files.S3FromEnv(); s != nil {
		return s
	}
	return nil
}

func cmdFiles(args []string) {
	need(args, 1, filesUsage)
	sub, rest := args[0], args[1:]

	conn := open()
	defer conn.Close()
	fs := files.New(conn, files.NewLocal(""), s3OrNil())

	switch sub {
	case "ns":
		cmdFilesNS(fs, rest)

	case "put":
		flags := flag.NewFlagSet("files put", flag.ExitOnError)
		name := flags.String("name", "", "stored name (defaults to the source file's base name)")
		contentType := flags.String("content-type", "", "override the detected content type")
		meta := flags.String("meta", "", "JSON object stored alongside the file")
		overwrite := flags.Bool("overwrite", false, "replace an existing file with the same name")
		stdin := flags.Bool("stdin", false, "read the content from stdin (--name is then required)")
		pos := parseFlags(flags, rest)

		var reader io.Reader
		var stored string
		if *stdin {
			need(pos, 1, "bkn files put <namespace> --stdin --name <name>")
			if *name == "" {
				out.Fail(out.ValidationError, "missing_name", "--name is required with --stdin")
			}
			reader, stored = os.Stdin, *name
		} else {
			need(pos, 2, "bkn files put <namespace> <path> [--name <name>]")
			f, err := os.Open(pos[1])
			if err != nil {
				out.Fail(out.InvalidValue, "unreadable_file", err.Error())
			}
			defer f.Close()
			reader = f
			stored = *name
			if stored == "" {
				stored = baseName(pos[1])
			}
		}

		var metadata map[string]any
		if *meta != "" {
			metadata = readData(*meta)
		}
		f, err := fs.Put(pos[0], stored, reader, files.PutOptions{
			ContentType: *contentType, Metadata: metadata, Overwrite: *overwrite,
		})
		if err != nil {
			failFiles(err)
		}
		out.Data(map[string]any{"file": f})

	case "get":
		flags := flag.NewFlagSet("files get", flag.ExitOnError)
		outPath := flags.String("out", "-", "write to this path, or - for stdout")
		pos := parseFlags(flags, rest)
		need(pos, 2, "bkn files get <namespace> <name> [--out <path>]")

		f, rc, err := fs.Get(pos[0], pos[1])
		if err != nil {
			failFiles(err)
		}
		defer rc.Close()

		if *outPath == "-" {
			// The bytes are the data, so they go to stdout raw. This is the
			// one command that does not emit a JSON envelope.
			if _, err := io.Copy(os.Stdout, rc); err != nil {
				out.Fail(out.InternalError, "write_failed", err.Error())
			}
			out.Log("[files] %s/%s (%d bytes, %s)", f.Namespace, f.Name, f.Size, f.ContentType)
			return
		}
		dest, err := os.Create(*outPath)
		if err != nil {
			out.Fail(out.InvalidValue, "unwritable_path", err.Error())
		}
		defer dest.Close()
		if _, err := io.Copy(dest, rc); err != nil {
			out.Fail(out.InternalError, "write_failed", err.Error())
		}
		out.Data(map[string]any{"file": f, "written_to": *outPath})

	case "show":
		need(rest, 2, "bkn files show <namespace> <name>")
		f, err := fs.Show(rest[0], rest[1])
		if err != nil {
			failFiles(err)
		}
		out.Data(map[string]any{"file": f})

	case "list":
		flags := flag.NewFlagSet("files list", flag.ExitOnError)
		limit := flags.Int("limit", 50, "maximum files")
		offset := flags.Int("offset", 0, "files to skip")
		pos := parseFlags(flags, rest)
		need(pos, 1, "bkn files list <namespace> [--limit N] [--offset N]")
		list, err := fs.List(pos[0], *limit, *offset)
		if err != nil {
			failFiles(err)
		}
		out.Data(map[string]any{"namespace": pos[0], "count": len(list), "files": list})

	case "delete":
		need(rest, 2, "bkn files delete <namespace> <name>")
		if err := fs.Delete(rest[0], rest[1]); err != nil {
			failFiles(err)
		}
		out.Data(map[string]any{"deleted": rest[1], "namespace": rest[0]})

	default:
		out.Fail(out.InvalidArguments, "unknown_command", "unknown files subcommand "+sub, "usage: "+filesUsage)
	}
}

func cmdFilesNS(fs *files.Store, args []string) {
	need(args, 1, "bkn files ns <create|list|delete> ...")
	sub, rest := args[0], args[1:]

	switch sub {
	case "create":
		flags := flag.NewFlagSet("files ns create", flag.ExitOnError)
		backend := flags.String("backend", files.BackendLocal, strings.Join(files.Backends(), "|"))
		maxBytes := flags.String("max-bytes", "0", "size cap per file (0 uses the default)")
		public := flags.Bool("public", false, "serve these files over HTTP without auth")
		verifyType := flags.Bool("verify-type", false,
			"decide a file's type from its bytes, refusing an upload whose declared type disagrees")
		var allow repeated
		flags.Var(&allow, "allow-type", "permitted content type, repeatable (\"image/*\")")
		pos := parseFlags(flags, rest)
		need(pos, 1, "bkn files ns create <name> [--backend local|s3] [--max-bytes N] [--allow-type image/*] [--public] [--verify-type]")

		n, err := strconv.ParseInt(*maxBytes, 10, 64)
		if err != nil || n < 0 {
			out.Fail(out.InvalidValue, "invalid_value", "--max-bytes must be a non-negative integer")
		}
		ns, err := fs.EnsureNamespace(files.Namespace{
			Name: pos[0], Backend: *backend, MaxBytes: n, AllowTypes: allow,
			Public: *public, VerifyType: *verifyType,
		})
		if err != nil {
			failFiles(err)
		}
		out.Data(map[string]any{"namespace": ns})

	case "list":
		list, err := fs.Namespaces()
		if err != nil {
			failFiles(err)
		}
		out.Data(map[string]any{
			"count": len(list), "namespaces": list, "backends_available": fs.Available(),
		})

	case "delete":
		need(rest, 1, "bkn files ns delete <name>")
		if err := fs.DeleteNamespace(rest[0]); err != nil {
			failFiles(err)
		}
		out.Data(map[string]any{"deleted": rest[0]})

	default:
		out.Fail(out.InvalidArguments, "unknown_command", "unknown files ns subcommand "+sub)
	}
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	return p
}
