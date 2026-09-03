package script

import (
	"github.com/javimosch/bkn/internal/events"
)

// newEventsAPI builds the `bkn.events` namespace.
func (r *Runner) newEventsAPI(throw func(error)) map[string]any {
	if r.events == nil {
		return nil
	}
	log := r.events

	return map[string]any{
		"emit": func(stream, eventType string, opts map[string]any) any {
			e := events.Event{Stream: stream, Type: eventType}
			if opts != nil {
				if v, ok := opts["level"].(string); ok {
					e.Level = v
				}
				if v, ok := opts["source"].(string); ok {
					e.Source = v
				}
				if v, ok := opts["subject"].(string); ok {
					e.Subject = v
				}
				if v, ok := opts["data"].(map[string]any); ok {
					e.Data = v
				}
			}
			if e.Source == "" {
				// Attributing an event to the script that emitted it is more
				// useful than making every script remember to say so.
				e.Source = "script"
			}
			out, err := log.Emit(e)
			if err != nil {
				throw(err)
			}
			return out
		},
		"list": func(stream string, opts map[string]any) []any {
			q := events.Query{Limit: 50}
			if opts != nil {
				str := func(key string) string {
					if v, ok := opts[key].(string); ok {
						return v
					}
					return ""
				}
				q.Type, q.Level, q.Source = str("type"), str("level"), str("source")
				q.Subject, q.Since, q.Until = str("subject"), str("since"), str("until")
				if n, ok := toInt(opts["limit"]); ok {
					q.Limit = n
				}
				if n, ok := toInt(opts["offset"]); ok {
					q.Offset = n
				}
			}
			list, err := log.List(stream, q)
			if err != nil {
				throw(err)
			}
			out := make([]any, len(list))
			for i, e := range list {
				out[i] = e
			}
			return out
		},
		"stats": func(stream, by string, opts map[string]any) []any {
			q := events.Query{}
			if opts != nil {
				if v, ok := opts["since"].(string); ok {
					q.Since = v
				}
				if v, ok := opts["level"].(string); ok {
					q.Level = v
				}
			}
			buckets, err := log.Stats(stream, by, q)
			if err != nil {
				throw(err)
			}
			out := make([]any, len(buckets))
			for i, b := range buckets {
				out[i] = b
			}
			return out
		},
		"prune": func(stream, olderThan string) int {
			n, err := log.Prune(stream, olderThan)
			if err != nil {
				throw(err)
			}
			return n
		},
	}
}
