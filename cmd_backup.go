package main

import (
	"flag"
	"io"
	"os"
	"path/filepath"

	"github.com/javimosch/bkn/internal/db"
	"github.com/javimosch/bkn/internal/out"
)

// cmdBackup writes a consistent snapshot of the datastore, safely, while the
// server is running.
//
// --stdout exists because the job that needs this pipes the artifact straight
// into gzip over ssh. VACUUM INTO can only write to a path, so the snapshot
// goes to a temp file, is streamed out, and is removed. In that mode nothing
// else may touch stdout, so the usual result document goes to stderr instead.
func cmdBackup(args []string) {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	to := fs.String("to", "", "write the snapshot to this path")
	toStdout := fs.Bool("stdout", false, "stream the snapshot to stdout")
	pos := parseFlags(fs, args)

	dest := *to
	if dest == "" && len(pos) > 0 {
		dest = pos[0]
	}
	switch {
	case *toStdout && dest != "":
		out.Fail(out.InvalidValue, "conflicting_flags",
			"--stdout streams the snapshot; it cannot also write to a path",
			"bkn backup --stdout > snap.db", "bkn backup --to /tmp/snap.db")
	case !*toStdout && dest == "":
		out.Fail(out.InvalidValue, "missing_destination",
			"say where the snapshot goes",
			"bkn backup --to /tmp/bkn-snap.db", "bkn backup --stdout | gzip -c > snap.db.gz")
	}

	if *toStdout {
		tmp, err := os.MkdirTemp("", "bkn-snap")
		if err != nil {
			out.Fail(out.InternalError, "temp_failed", err.Error())
		}
		defer os.RemoveAll(tmp)
		dest = filepath.Join(tmp, "snapshot.db")
	}

	if err := db.Snapshot(dest); err != nil {
		out.Fail(out.InternalError, "snapshot_failed", err.Error(),
			"BKN_DATA must point at an existing database, and the destination must not already exist")
	}

	// A snapshot nobody checked is a guess. Proving it readable here is what
	// separates this from the method it replaces, which can produce a file
	// that opens fine and has silently lost the most recent commits.
	result, err := db.Verify(dest)
	if err != nil || result != "ok" {
		msg := "the snapshot failed its integrity check: " + result
		if err != nil {
			msg = err.Error()
		}
		_ = os.Remove(dest)
		out.Fail(out.InternalError, "snapshot_corrupt", msg)
	}

	info, err := os.Stat(dest)
	if err != nil {
		out.Fail(out.InternalError, "snapshot_failed", err.Error())
	}

	if *toStdout {
		f, err := os.Open(dest)
		if err != nil {
			out.Fail(out.InternalError, "snapshot_failed", err.Error())
		}
		defer f.Close()
		out.Log("[backup] %d bytes, integrity ok", info.Size())
		if _, err := io.Copy(os.Stdout, f); err != nil {
			out.Fail(out.InternalError, "write_failed", err.Error())
		}
		return
	}

	out.Data(map[string]any{
		"snapshot": dest, "bytes": info.Size(), "integrity": result, "source": db.Path(),
	})
}
