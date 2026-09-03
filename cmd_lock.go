package main

import (
	"flag"
	"time"

	"github.com/javimosch/bkn/internal/out"
	"github.com/javimosch/bkn/internal/store"
)

func cmdLock(args []string) {
	need(args, 1, "bkn lock <list|acquire|release> ...")
	sub, rest := args[0], args[1:]

	conn := open()
	defer conn.Close()
	locks := store.NewLocks(conn)

	switch sub {
	case "list":
		list, err := locks.List()
		if err != nil {
			out.Fail(out.InternalError, "lock_error", err.Error())
		}
		out.Data(map[string]any{"count": len(list), "locks": list})

	case "acquire":
		fs := flag.NewFlagSet("lock acquire", flag.ExitOnError)
		ttl := fs.Duration("ttl", time.Minute, "how long the lease lasts")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn lock acquire <key> [--ttl 15m]")
		lock, err := locks.Acquire(pos[0], *ttl)
		if err == store.ErrLockHeld {
			out.Fail(out.Conflict, "lock_held", err.Error(), "bkn lock list")
		}
		if err != nil {
			out.Fail(out.InternalError, "lock_error", err.Error())
		}
		out.Data(map[string]any{"lock": lock})

	case "release":
		fs := flag.NewFlagSet("lock release", flag.ExitOnError)
		force := fs.Bool("force", false, "release regardless of owner")
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn lock release <key> [<owner>] [--force]")

		var released bool
		var err error
		if *force {
			released, err = locks.ForceRelease(pos[0])
		} else {
			need(pos, 2, "bkn lock release <key> <owner>, or pass --force")
			released, err = locks.Release(pos[0], pos[1])
		}
		if err != nil {
			out.Fail(out.InternalError, "lock_error", err.Error())
		}
		out.Data(map[string]any{"released": released, "key": pos[0]})

	default:
		out.Fail(out.InvalidArguments, "unknown_command", "unknown lock subcommand "+sub)
	}
}
