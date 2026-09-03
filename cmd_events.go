package main

import (
	"errors"
	"flag"
	"strings"

	"github.com/javimosch/bkn/internal/events"
	"github.com/javimosch/bkn/internal/out"
)

const eventsUsage = "bkn events <emit|list|stats|streams|prune> ..."

func failEvents(err error) {
	switch {
	case errors.Is(err, events.ErrBadStream), errors.Is(err, events.ErrBadLevel),
		errors.Is(err, events.ErrBadGroupBy), errors.Is(err, events.ErrBadDuration):
		out.Fail(out.InvalidValue, "validation_error", err.Error())
	default:
		out.Fail(out.InternalError, "events_error", err.Error())
	}
}

// queryFlags wires the filters shared by list and stats.
func queryFlags(fs *flag.FlagSet) func() events.Query {
	typ := fs.String("type", "", "only this event type")
	level := fs.String("level", "", strings.Join(events.Levels(), "|"))
	source := fs.String("source", "", "only this source")
	subject := fs.String("subject", "", "only this subject")
	since := fs.String("since", "", "RFC3339 timestamp or a relative age like 24h or 7d")
	until := fs.String("until", "", "RFC3339 timestamp or a relative age")
	limit := fs.Int("limit", 50, "maximum events")
	offset := fs.Int("offset", 0, "events to skip")
	return func() events.Query {
		return events.Query{
			Type: *typ, Level: *level, Source: *source, Subject: *subject,
			Since: *since, Until: *until, Limit: *limit, Offset: *offset,
		}
	}
}

func cmdEvents(args []string) {
	need(args, 1, eventsUsage)
	sub, rest := args[0], args[1:]

	conn := open()
	defer conn.Close()
	log := events.New(conn)

	switch sub {
	case "emit":
		fs := flag.NewFlagSet("events emit", flag.ExitOnError)
		level := fs.String("level", events.LevelInfo, strings.Join(events.Levels(), "|"))
		source := fs.String("source", "", "who emitted this")
		subject := fs.String("subject", "", "what it is about")
		data := fs.String("data", "", "JSON object payload")
		pos := parseFlags(fs, rest)
		need(pos, 2, "bkn events emit <stream> <type> [--level] [--subject] [--data <json>]")

		var payload map[string]any
		if *data != "" {
			payload = readData(*data)
		}
		e, err := log.Emit(events.Event{
			Stream: pos[0], Type: pos[1], Level: *level,
			Source: *source, Subject: *subject, Data: payload,
		})
		if err != nil {
			failEvents(err)
		}
		out.Data(map[string]any{"event": e})

	case "list":
		fs := flag.NewFlagSet("events list", flag.ExitOnError)
		query := queryFlags(fs)
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn events list <stream> [--type T] [--level L] [--since 24h] [--limit N]")

		list, err := log.List(pos[0], query())
		if err != nil {
			failEvents(err)
		}
		out.Data(map[string]any{"stream": pos[0], "count": len(list), "events": list})

	case "stats":
		fs := flag.NewFlagSet("events stats", flag.ExitOnError)
		by := fs.String("by", "type", strings.Join(events.GroupBys(), "|"))
		query := queryFlags(fs)
		pos := parseFlags(fs, rest)
		need(pos, 1, "bkn events stats <stream> [--by type|level|source|subject] [--since 24h]")

		buckets, err := log.Stats(pos[0], *by, query())
		if err != nil {
			failEvents(err)
		}
		total := 0
		for _, b := range buckets {
			total += b.Count
		}
		out.Data(map[string]any{"stream": pos[0], "by": *by, "total": total, "buckets": buckets})

	case "streams":
		list, err := log.Streams()
		if err != nil {
			failEvents(err)
		}
		out.Data(map[string]any{"count": len(list), "streams": list})

	case "prune":
		fs := flag.NewFlagSet("events prune", flag.ExitOnError)
		olderThan := fs.String("older-than", "", "delete events older than this age, e.g. 30d")
		stream := fs.String("stream", "", "restrict to one stream (default: all)")
		_ = parseFlags(fs, rest)
		if *olderThan == "" {
			out.Fail(out.ValidationError, "missing_age", "--older-than is required",
				"bkn events prune --older-than 30d")
		}
		n, err := log.Prune(*stream, *olderThan)
		if err != nil {
			failEvents(err)
		}
		out.Data(map[string]any{"pruned": n, "older_than": *olderThan, "stream": *stream})

	default:
		out.Fail(out.InvalidArguments, "unknown_command", "unknown events subcommand "+sub, "usage: "+eventsUsage)
	}
}
