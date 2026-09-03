package hooks

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/javimosch/bkn/internal/events"
	"github.com/javimosch/bkn/internal/script"
)

// EventStream is where deliveries are recorded.
const EventStream = "hooks"

// Dispatcher turns an inbound request into a script run.
type Dispatcher struct {
	reg    *Registry
	runner *script.Runner
	log    *events.Log
}

func NewDispatcher(reg *Registry, runner *script.Runner, log *events.Log) *Dispatcher {
	return &Dispatcher{reg: reg, runner: runner, log: log}
}

// ReadDelivery turns an HTTP request into the input a hook script receives.
//
// Both encodings of the body are provided: `body` is the string a script will
// normally hash or JSON.parse, and `body_base64` is the exact bytes, for a
// payload that is not valid UTF-8 or a signature computed over raw octets.
func ReadDelivery(name string, r *http.Request, maxBytes int64) (Delivery, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBytes))
	if err != nil {
		return Delivery{}, err
	}
	headers := map[string]string{}
	for key := range r.Header {
		// Lower-cased because a script should not have to guess whether a
		// proxy sent Stripe-Signature or stripe-signature.
		headers[lower(key)] = r.Header.Get(key)
	}
	query := map[string]string{}
	for key, values := range r.URL.Query() {
		if len(values) > 0 {
			query[key] = values[0]
		}
	}
	return Delivery{
		Hook: name, Method: r.Method, Headers: headers, Query: query,
		Body:       string(raw),
		BodyBase64: base64.StdEncoding.EncodeToString(raw),
		ReceivedAt: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

// Deliver runs the hook's script and converts its return value into an HTTP
// response.
func (d *Dispatcher) Deliver(h Hook, delivery Delivery) (Response, error) {
	if !h.Enabled {
		return Response{}, ErrDisabled
	}
	input, err := toMap(delivery)
	if err != nil {
		return Response{}, err
	}

	res, err := d.runner.Run(h.Script, input)
	if err != nil {
		d.emit(events.LevelError, "hook.error", h.Name, map[string]any{
			"script": h.Script, "error": err.Error(),
		})
		return Response{}, err
	}
	if !res.OK {
		// A hook whose script failed must not answer 200: providers retry on
		// a failure response, and swallowing it loses the delivery.
		d.emit(events.LevelError, "hook.failed", h.Name, map[string]any{
			"script": h.Script, "run_id": res.Run.ID, "error": res.Run.Error,
		})
		return Response{
			Status: http.StatusInternalServerError,
			Body:   map[string]any{"ok": false, "error": res.Run.Error, "run_id": res.Run.ID},
		}, nil
	}

	d.emit(events.LevelInfo, "hook.ok", h.Name, map[string]any{
		"script": h.Script, "run_id": res.Run.ID, "duration_ms": res.Run.DurationMS,
	})
	return shapeResponse(res.Value), nil
}

// shapeResponse lets a script control the reply by returning
// {status, body, headers}, and otherwise wraps whatever it returned.
func shapeResponse(value any) Response {
	out := Response{Status: http.StatusOK, Body: value}
	shape, ok := value.(map[string]any)
	if !ok {
		return out
	}
	status, hasStatus := shape["status"]
	if !hasStatus {
		return out
	}
	if n, ok := status.(float64); ok && n >= 100 && n <= 599 {
		out.Status = int(n)
	}
	if body, ok := shape["body"]; ok {
		out.Body = body
	} else {
		out.Body = map[string]any{"ok": out.Status < 400}
	}
	if headers, ok := shape["headers"].(map[string]any); ok {
		out.Headers = map[string]string{}
		for k, v := range headers {
			if s, ok := v.(string); ok {
				out.Headers[k] = s
			}
		}
	}
	return out
}

func toMap(v any) (map[string]any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(raw, &out)
}

func (d *Dispatcher) emit(level, eventType, hook string, data map[string]any) {
	if d.log == nil {
		return
	}
	_, _ = d.log.Emit(events.Event{
		Stream: EventStream, Type: eventType, Level: level,
		Source: "hooks", Subject: hook, Data: data,
	})
}
