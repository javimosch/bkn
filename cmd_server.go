package main

import (
	"flag"
	"os"
	"strconv"

	"github.com/javimosch/bkn/internal/auth"
	"github.com/javimosch/bkn/internal/daemon"
	"github.com/javimosch/bkn/internal/out"
	"github.com/javimosch/bkn/internal/script"
	"github.com/javimosch/bkn/internal/server"
	"github.com/javimosch/bkn/internal/store"
)

// bindFlags wires --host/--port with env fallbacks, flags winning.
func bindFlags(fs *flag.FlagSet) (*string, *int) {
	host := defaultHost
	if v := os.Getenv("BKN_HOST"); v != "" {
		host = v
	}
	port := defaultPort
	if v := os.Getenv("BKN_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	return fs.String("host", host, "bind address (loopback by default)"),
		fs.Int("port", port, "bind port")
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	host, port := bindFlags(fs)
	_ = parseFlags(fs, args)

	conn := open()
	defer conn.Close()

	st := store.New(conn)
	k := newKV(conn)
	reg := script.NewRegistry(conn)
	a, err := auth.New(conn, k)
	if err != nil {
		failAuth(err)
	}
	srv, err := server.New(
		server.Config{Host: *host, Port: *port, Version: Version},
		st, k, reg, script.NewRunner(reg, st, k, a), a,
	)
	if err != nil {
		out.Fail(out.InvalidValue, "unsafe_bind", err.Error(),
			"export BKN_ADMIN_TOKEN=$(openssl rand -hex 32)",
			"or bind loopback: bkn serve --host 127.0.0.1")
	}
	if err := srv.ListenAndServe(); err != nil {
		out.Fail(out.ConnectionError, "listen_failed", err.Error(),
			"check whether another process holds that port: bkn daemon status")
	}
}

func cmdDaemon(args []string) {
	need(args, 1, "bkn daemon <start|stop|status> [--host H] [--port P]")
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("daemon "+sub, flag.ExitOnError)
	host, port := bindFlags(fs)
	_ = parseFlags(fs, rest)

	switch sub {
	case "start":
		st, err := daemon.Start(*host, *port)
		if err != nil {
			out.Fail(out.ConnectionError, "daemon_unhealthy", err.Error(),
				"read the log: tail "+daemon.LogPath())
		}
		out.Data(map[string]any{"daemon": st, "log": daemon.LogPath()})

	case "stop":
		stopped, err := daemon.Stop(*host, *port)
		if err != nil {
			out.Fail(out.ConnectionError, "shutdown_failed", err.Error())
		}
		// Idempotent: stopping an already-stopped daemon is a success.
		out.Data(map[string]any{"stopped": stopped, "was_running": stopped})

	case "status":
		out.Data(map[string]any{"daemon": daemon.Probe(*host, *port)})

	default:
		out.Fail(out.InvalidArguments, "unknown_command", "unknown daemon subcommand "+sub)
	}
}
