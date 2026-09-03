package hooks_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/javimosch/bkn/internal/db"
	"github.com/javimosch/bkn/internal/events"
	"github.com/javimosch/bkn/internal/hooks"
	"github.com/javimosch/bkn/internal/kv"
	"github.com/javimosch/bkn/internal/script"
	"github.com/javimosch/bkn/internal/store"
)

func setup(t *testing.T) (*hooks.Registry, *hooks.Dispatcher, *script.Registry) {
	t.Helper()
	t.Setenv("BKN_DATA", t.TempDir()+"/test.db")
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	scripts := script.NewRegistry(conn)
	log := events.New(conn)
	runner := script.NewRunner(scripts, store.New(conn), kv.New(conn, nil, 0), nil, nil, log)
	reg := hooks.NewRegistry(conn)
	return reg, hooks.NewDispatcher(reg, runner, log), scripts
}

func bind(t *testing.T, reg *hooks.Registry, scripts *script.Registry, name, code string) hooks.Hook {
	t.Helper()
	if _, err := scripts.Create(script.Script{Name: name, Code: code}); err != nil {
		t.Fatalf("script.Create: %v", err)
	}
	h, err := reg.Create(hooks.Hook{Name: name, Script: name})
	if err != nil {
		t.Fatalf("hooks.Create: %v", err)
	}
	return h
}

func deliver(t *testing.T, name, body string, headers map[string]string) hooks.Delivery {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/"+name+"?a=1", strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	d, err := hooks.ReadDelivery(name, req, hooks.DefaultMaxBytes)
	if err != nil {
		t.Fatalf("ReadDelivery: %v", err)
	}
	return d
}

// A signature covers exact bytes, so the body must reach the script
// untouched - no parse, no re-serialize, no whitespace normalisation.
func TestDeliveryPreservesTheRawBody(t *testing.T) {
	const body = "{\n  \"id\":  \"evt_1\",\n  \"emoji\": \"€ ✓\"\n}\n"
	d := deliver(t, "h", body, map[string]string{"Stripe-Signature": "t=1,v1=abc"})

	if d.Body != body {
		t.Errorf("body was altered:\n got %q\nwant %q", d.Body, body)
	}
	if d.BodyBase64 == "" {
		t.Error("body_base64 was not populated")
	}
	// Headers are lower-cased so a script need not guess a proxy's casing.
	if d.Headers["stripe-signature"] != "t=1,v1=abc" {
		t.Errorf("headers = %v, want a lower-cased stripe-signature", d.Headers)
	}
	if d.Query["a"] != "1" {
		t.Errorf("query = %v", d.Query)
	}
	if d.Method != http.MethodPost {
		t.Errorf("method = %q", d.Method)
	}
}

func TestBodyIsCappedAtTheHookLimit(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/hooks/h", strings.NewReader(strings.Repeat("x", 500)))
	d, err := hooks.ReadDelivery("h", req, 100)
	if err != nil {
		t.Fatalf("ReadDelivery: %v", err)
	}
	if len(d.Body) != 100 {
		t.Errorf("body length = %d, want it capped at 100", len(d.Body))
	}
}

func TestScriptControlsTheResponse(t *testing.T) {
	reg, dispatcher, scripts := setup(t)
	h := bind(t, reg, scripts, "shaped", `
		function main(d) {
			if (d.headers["x-token"] !== "ok") {
				return { status: 401, body: { error: "no" } };
			}
			return { status: 202, body: { received: JSON.parse(d.body).id }, headers: { "X-Thing": "yes" } };
		}`)

	res, err := dispatcher.Deliver(h, deliver(t, "shaped", `{"id":"evt_9"}`, map[string]string{"X-Token": "ok"}))
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if res.Status != 202 {
		t.Errorf("status = %d, want 202", res.Status)
	}
	if res.Headers["X-Thing"] != "yes" {
		t.Errorf("headers = %v", res.Headers)
	}

	res, err = dispatcher.Deliver(h, deliver(t, "shaped", `{"id":"evt_9"}`, nil))
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if res.Status != 401 {
		t.Errorf("rejected delivery status = %d, want 401", res.Status)
	}
}

// A provider retries on failure. Answering 200 for a script that threw would
// silently drop the delivery.
func TestFailingScriptDoesNotAnswerOK(t *testing.T) {
	reg, dispatcher, scripts := setup(t)
	h := bind(t, reg, scripts, "broken", `function main(){ throw new Error("kaboom") }`)

	res, err := dispatcher.Deliver(h, deliver(t, "broken", "{}", nil))
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if res.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 so the provider retries", res.Status)
	}
}

func TestPlainReturnValueBecomesA200Body(t *testing.T) {
	reg, dispatcher, scripts := setup(t)
	h := bind(t, reg, scripts, "plain", `function main(){ return { handled: true } }`)

	res, err := dispatcher.Deliver(h, deliver(t, "plain", "{}", nil))
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("status = %d, want 200", res.Status)
	}
	body, ok := res.Body.(map[string]any)
	if !ok || body["handled"] != true {
		t.Errorf("body = %#v", res.Body)
	}
}

func TestDisabledHookIsRefused(t *testing.T) {
	reg, dispatcher, scripts := setup(t)
	h := bind(t, reg, scripts, "off", `function main(){ return 1 }`)

	no := false
	if _, err := reg.Update("off", nil, nil, &no, nil, nil); err != nil {
		t.Fatalf("Update: %v", err)
	}
	updated, err := reg.Get("off")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := dispatcher.Deliver(updated, deliver(t, "off", "{}", nil)); !errors.Is(err, hooks.ErrDisabled) {
		t.Errorf("Deliver on a disabled hook = %v, want ErrDisabled", err)
	}
	_ = h
}

func TestRegistryValidation(t *testing.T) {
	reg, _, scripts := setup(t)
	bind(t, reg, scripts, "taken", `function main(){ return 1 }`)

	if _, err := reg.Create(hooks.Hook{Name: "taken", Script: "taken"}); !errors.Is(err, hooks.ErrExists) {
		t.Errorf("duplicate = %v, want ErrExists", err)
	}
	for _, bad := range []string{"Bad", "9lives", "has space", ""} {
		if _, err := reg.Create(hooks.Hook{Name: bad, Script: "taken"}); !errors.Is(err, hooks.ErrBadName) {
			t.Errorf("Create(%q) = %v, want ErrBadName", bad, err)
		}
	}
	if _, err := reg.Get("absent"); !errors.Is(err, hooks.ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
	h, err := reg.Get("taken")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if h.Path != "/v1/hooks/taken" {
		t.Errorf("path = %q", h.Path)
	}
	if h.MaxBytes != hooks.DefaultMaxBytes {
		t.Errorf("max_bytes = %d, want the default", h.MaxBytes)
	}
}
