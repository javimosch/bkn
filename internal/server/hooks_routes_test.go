package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/javimosch/bkn/internal/db"
	"github.com/javimosch/bkn/internal/hooks"
	"github.com/javimosch/bkn/internal/store"
)

func TestHookLimiterCountsPerKey(t *testing.T) {
	l := newHookLimiter()
	for i := 0; i < 3; i++ {
		if !l.allow("hook|1.2.3.4", 3) {
			t.Fatalf("request %d was refused inside the budget", i+1)
		}
	}
	if l.allow("hook|1.2.3.4", 3) {
		t.Error("the fourth request was allowed past a limit of 3")
	}
	// A different client has its own budget, and so does a different hook.
	if !l.allow("hook|5.6.7.8", 3) {
		t.Error("a different client was refused")
	}
	if !l.allow("other|1.2.3.4", 3) {
		t.Error("a different hook was refused")
	}
	// Zero means unlimited, which is right for a provider webhook.
	for i := 0; i < 50; i++ {
		if !l.allow("unlimited|1.2.3.4", 0) {
			t.Fatal("a zero limit refused a request")
		}
	}
}

// Behind a proxy every request carries the proxy's address, so without
// consulting the forwarded header the limit becomes global.
func TestClientIPPrefersTheForwardedAddress(t *testing.T) {
	cases := []struct {
		remote, forwarded, want string
	}{
		{"10.0.0.1:5000", "", "10.0.0.1"},
		{"10.0.0.1:5000", "203.0.113.9", "203.0.113.9"},
		{"10.0.0.1:5000", "203.0.113.9, 70.41.3.18", "203.0.113.9"},
		{"10.0.0.1:5000", "  203.0.113.9  ", "203.0.113.9"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.RemoteAddr = tc.remote
		if tc.forwarded != "" {
			r.Header.Set("X-Forwarded-For", tc.forwarded)
		}
		if got := clientIP(r); got != tc.want {
			t.Errorf("clientIP(remote=%q, xff=%q) = %q, want %q", tc.remote, tc.forwarded, got, tc.want)
		}
	}
}

func TestOriginAllowed(t *testing.T) {
	cases := []struct {
		allow  []string
		origin string
		want   bool
	}{
		{nil, "https://site.example", false},
		{[]string{}, "https://site.example", false},
		{[]string{"https://site.example"}, "https://site.example", true},
		{[]string{"https://site.example"}, "https://evil.example", false},
		{[]string{"https://site.example"}, "https://SITE.example", true},
		{[]string{"*"}, "https://anything.example", true},
		{[]string{"https://a.example", "https://b.example"}, "https://b.example", true},
	}
	for _, tc := range cases {
		h := hooks.Hook{AllowOrigin: tc.allow}
		if got := h.OriginAllowed(tc.origin); got != tc.want {
			t.Errorf("OriginAllowed(%v, %q) = %v, want %v", tc.allow, tc.origin, got, tc.want)
		}
	}
}

// A CSV export or an RSS feed is a string; JSON-encoding it would wrap the
// whole document in quotes and escape every newline.
func TestHookBodyHonoursTheScriptsContentType(t *testing.T) {
	cases := []struct {
		name     string
		res      hooks.Response
		wantBody string
		wantType string
	}{
		{
			name:     "csv passes through verbatim",
			res:      hooks.Response{Status: 200, Body: "a,b\r\n1,2\r\n", Headers: map[string]string{"Content-Type": "text/csv"}},
			wantBody: "a,b\r\n1,2\r\n",
			wantType: "text/csv",
		},
		{
			name:     "an object is encoded as JSON",
			res:      hooks.Response{Status: 200, Body: map[string]any{"ok": true}},
			wantBody: "{\"ok\":true}\n",
			wantType: "application/json; charset=utf-8",
		},
		{
			name:     "a string with a JSON content type is still encoded",
			res:      hooks.Response{Status: 200, Body: "plain", Headers: map[string]string{"Content-Type": "application/json"}},
			wantBody: "\"plain\"\n",
			wantType: "application/json",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			for k, v := range tc.res.Headers {
				w.Header().Set(k, v)
			}
			writeHookBody(w, tc.res)

			if got := w.Body.String(); got != tc.wantBody {
				t.Errorf("body = %q, want %q", got, tc.wantBody)
			}
			if got := w.Header().Get("Content-Type"); got != tc.wantType {
				t.Errorf("content-type = %q, want %q", got, tc.wantType)
			}
		})
	}
}

// A loopback bind means "only a co-resident process can reach me" — until a
// reverse proxy listens publicly and forwards here, which is the standard
// deployment. A forwarded request must not inherit local trust.
func TestProxiedRequestsDoNotGetTheLoopbackExemption(t *testing.T) {
	open := &Server{cfg: Config{Host: "127.0.0.1"}}

	direct := httptest.NewRequest(http.MethodGet, "/v1/store/a/b", nil)
	if !open.authed(direct) {
		t.Error("a direct loopback request was refused on an open server")
	}

	for _, header := range []string{
		"X-Forwarded-For", "X-Forwarded-Host", "X-Real-Ip", "Forwarded", "X-Forwarded-Proto",
	} {
		r := httptest.NewRequest(http.MethodGet, "/v1/store/a/b", nil)
		r.Header.Set(header, "203.0.113.9")
		if open.authed(r) {
			t.Errorf("a request carrying %s was treated as local", header)
		}
	}

	// With a token configured, the token decides and nothing else does.
	gated := &Server{cfg: Config{Host: "127.0.0.1"}, admin: "s3cret"}
	withToken := httptest.NewRequest(http.MethodGet, "/v1/store/a/b", nil)
	withToken.Header.Set("Authorization", "Bearer s3cret")
	withToken.Header.Set("X-Forwarded-For", "203.0.113.9")
	if !gated.authed(withToken) {
		t.Error("a valid token was refused because the request was proxied")
	}
	if gated.authed(direct) {
		t.Error("a request with no token passed on a token-gated server")
	}
}

// A liveness probe that answers 200 because the listener is up reports only
// that Go is running. This process is useless without its datastore, so an
// unreachable one must be a 503 — that is the difference between a probe a
// supervisor can act on and one that always says yes.
func TestHealthReportsAnUnreachableDatastore(t *testing.T) {
	t.Setenv("BKN_DATA", t.TempDir()+"/health.db")
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	srv := &Server{cfg: Config{Host: "127.0.0.1"}, st: store.New(conn)}

	healthy := httptest.NewRecorder()
	srv.handleHealth(healthy, httptest.NewRequest(http.MethodGet, "/_health", nil))
	if healthy.Code != http.StatusOK {
		t.Errorf("healthy probe = %d, want 200", healthy.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(healthy.Body.Bytes(), &body)
	if body["ok"] != true || body["service"] != "bkn" || body["pid"] == nil {
		t.Errorf("healthy body = %v, want ok/service/pid", body)
	}

	// Take the datastore away underneath it.
	conn.Close()

	sick := httptest.NewRecorder()
	srv.handleHealth(sick, httptest.NewRequest(http.MethodGet, "/_health", nil))
	if sick.Code != http.StatusServiceUnavailable {
		t.Errorf("probe with a dead datastore = %d, want 503", sick.Code)
	}
	_ = json.Unmarshal(sick.Body.Bytes(), &body)
	if body["ok"] != false {
		t.Errorf("unhealthy body = %v, want ok:false", body)
	}
}

// Relocating BKN_HOME must move everything, including the shutdown token.
func TestTokenPathFollowsTheToolHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BKN_HOME", home)
	if got := TokenPath(); got != filepath.Join(home, "shutdown.token") {
		t.Errorf("TokenPath() = %q, want it inside BKN_HOME", got)
	}
}
