package script

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/dop251/goja"

	"github.com/javimosch/bkn/internal/kv"
	"github.com/javimosch/bkn/internal/store"
)

// maxResponseBytes caps what one fetch can pull into the VM.
const maxResponseBytes = 1 << 20

// newHost builds the `bkn` global: the entire surface a script can reach.
//
// Everything a script can do is in this file. That is the point - the sandbox
// boundary should be readable in one sitting. A script has no filesystem, no
// process control, no timers, and no network beyond its own allow_net list.
func (r *Runner) newHost(vm *goja.Runtime, s Script, logs *strings.Builder) map[string]any {
	throw := func(err error) {
		panic(vm.NewGoError(err))
	}

	ref := func(raw string) store.Ref {
		parsed, err := store.ParseRef(raw)
		if err != nil {
			throw(err)
		}
		return parsed
	}

	// nilOnNotFound turns a missing record into JS null. Throwing would force
	// every lookup into a try/catch, which is not how JS code reads.
	nilOnNotFound := func(rec store.Record, err error) any {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrNoCollection) {
			return nil
		}
		if err != nil {
			throw(err)
		}
		return map[string]any(rec)
	}

	filtersFrom := func(obj map[string]any) []store.Filter {
		filters := make([]store.Filter, 0, len(obj))
		for field, val := range obj {
			filters = append(filters, store.Filter{Field: field, Value: fmt.Sprintf("%v", val)})
		}
		return filters
	}

	storeAPI := map[string]any{
		"get": func(refStr, id string) any {
			return nilOnNotFound(r.st.Get(ref(refStr), id))
		},
		"find": func(refStr string, where map[string]any) any {
			return nilOnNotFound(r.st.Find(ref(refStr), filtersFrom(where)))
		},
		"put": func(refStr string, doc map[string]any, id string) any {
			rec, err := r.st.Put(ref(refStr), id, doc)
			if err != nil {
				throw(err)
			}
			return map[string]any(rec)
		},
		"patch": func(refStr, id string, fields map[string]any) any {
			rec, err := r.st.Patch(ref(refStr), id, fields)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err != nil {
				throw(err)
			}
			return map[string]any(rec)
		},
		"delete": func(refStr, id string) bool {
			err := r.st.Delete(ref(refStr), id)
			if errors.Is(err, store.ErrNotFound) {
				return false
			}
			if err != nil {
				throw(err)
			}
			return true
		},
		"list": func(refStr string, opts map[string]any) []any {
			var filters []store.Filter
			limit, offset := 50, 0
			if opts != nil {
				if w, ok := opts["where"].(map[string]any); ok {
					filters = filtersFrom(w)
				}
				if n, ok := toInt(opts["limit"]); ok {
					limit = n
				}
				if n, ok := toInt(opts["offset"]); ok {
					offset = n
				}
			}
			recs, err := r.st.List(ref(refStr), filters, limit, offset)
			if err != nil {
				throw(err)
			}
			out := make([]any, len(recs))
			for i, rec := range recs {
				out[i] = map[string]any(rec)
			}
			return out
		},
		"collections": func(ns string) []any {
			cols, err := r.st.Collections(ns)
			if err != nil {
				throw(err)
			}
			out := make([]any, len(cols))
			for i, c := range cols {
				out[i] = c
			}
			return out
		},
	}

	kvAPI := map[string]any{
		"get": func(key string) any {
			e, err := r.kv.Get(key)
			if errors.Is(err, kv.ErrNotFound) {
				return nil
			}
			if err != nil {
				throw(err)
			}
			return e.Value
		},
		"set": func(key, value string, opts map[string]any) any {
			typ, desc, public := kv.TypeString, "", false
			if opts != nil {
				if v, ok := opts["type"].(string); ok && v != "" {
					typ = v
				}
				if v, ok := opts["description"].(string); ok {
					desc = v
				}
				if v, ok := opts["public"].(bool); ok {
					public = v
				}
			}
			e, err := r.kv.Set(key, value, typ, desc, public)
			if err != nil {
				throw(err)
			}
			if e.Type == kv.TypeEncrypted {
				e.Value = ""
			}
			return e
		},
		"list": func(prefix string) []any {
			entries, err := r.kv.List(prefix, false)
			if err != nil {
				throw(err)
			}
			out := make([]any, len(entries))
			for i, e := range entries {
				out[i] = e
			}
			return out
		},
		"delete": func(key string) bool {
			err := r.kv.Delete(key)
			if errors.Is(err, kv.ErrNotFound) {
				return false
			}
			if err != nil {
				throw(err)
			}
			return true
		},
	}

	api := map[string]any{
		"store": storeAPI,
		"kv":    kvAPI,
		"http":  map[string]any{"fetch": r.fetchFunc(vm, s, throw)},
		"log": func(call goja.FunctionCall) goja.Value {
			parts := make([]string, 0, len(call.Arguments))
			for _, a := range call.Arguments {
				parts = append(parts, a.String())
			}
			logs.WriteString(strings.Join(parts, " ") + "\n")
			return goja.Undefined()
		},
		"id":  func() string { return store.NewID() },
		"now": func() string { return time.Now().UTC().Format(time.RFC3339) },
	}
	if authAPI := r.newAuthAPI(throw); authAPI != nil {
		api["auth"] = authAPI
	}
	if filesAPI := r.newFilesAPI(throw); filesAPI != nil {
		api["files"] = filesAPI
	}
	return api
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		return int(n), true
	case int:
		return n, true
	case float64:
		return int(n), true
	}
	return 0, false
}

// fetchFunc builds bkn.http.fetch, gated by the script's allow_net list.
//
// The call is synchronous. goja has no event loop, so there are no promises
// and no async/await; a script that needs three HTTP calls makes them in
// sequence. That is a real constraint, and it is documented in the guide.
func (r *Runner) fetchFunc(vm *goja.Runtime, s Script, throw func(error)) func(string, map[string]any) any {
	return func(rawURL string, opts map[string]any) any {
		u, err := url.Parse(rawURL)
		if err != nil {
			throw(fmt.Errorf("invalid url %q: %w", rawURL, err))
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			throw(fmt.Errorf("only http and https are allowed, got %q", u.Scheme))
		}
		if !hostAllowed(u.Hostname(), s.AllowNet) {
			name := s.Name
			if name == "" {
				name = "<name>"
			}
			throw(fmt.Errorf("host %q is not in this script's allow_net list %v; "+
				"grant it with: bkn script update %s --allow-net %s",
				u.Hostname(), s.AllowNet, name, u.Hostname()))
		}

		method, body := http.MethodGet, io.Reader(nil)
		headers := map[string]string{}
		timeout := 10 * time.Second

		if opts != nil {
			if m, ok := opts["method"].(string); ok && m != "" {
				method = strings.ToUpper(m)
			}
			if h, ok := opts["headers"].(map[string]any); ok {
				for k, v := range h {
					headers[k] = fmt.Sprintf("%v", v)
				}
			}
			if b, ok := opts["body"]; ok && b != nil {
				body = strings.NewReader(bodyString(vm, b, headers))
			}
			if ms, ok := toInt(opts["timeout_ms"]); ok && ms > 0 {
				timeout = time.Duration(ms) * time.Millisecond
			}
		}

		req, err := http.NewRequest(method, rawURL, body)
		if err != nil {
			throw(err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		client := &http.Client{
			Timeout:   timeout,
			Transport: guardedTransport(),
			CheckRedirect: func(rr *http.Request, via []*http.Request) error {
				// A redirect must not be a way out of the allowlist.
				if !hostAllowed(rr.URL.Hostname(), s.AllowNet) {
					return fmt.Errorf("redirect to disallowed host %q", rr.URL.Hostname())
				}
				return nil
			},
		}
		resp, err := client.Do(req)
		if err != nil {
			throw(err)
		}
		defer resp.Body.Close()

		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		if err != nil {
			throw(err)
		}

		respHeaders := map[string]any{}
		for k := range resp.Header {
			respHeaders[strings.ToLower(k)] = resp.Header.Get(k)
		}
		out := map[string]any{
			"status":  resp.StatusCode,
			"ok":      resp.StatusCode >= 200 && resp.StatusCode < 300,
			"headers": respHeaders,
			"body":    string(raw),
			"json":    nil,
		}
		if parsed, err := vm.RunString("JSON.parse"); err == nil {
			if fn, ok := goja.AssertFunction(parsed); ok {
				if v, err := fn(goja.Undefined(), vm.ToValue(string(raw))); err == nil {
					out["json"] = v.Export()
				}
			}
		}
		return out
	}
}

func bodyString(vm *goja.Runtime, b any, headers map[string]string) string {
	if s, ok := b.(string); ok {
		return s
	}
	// A non-string body is JSON, and gets the content type unless one was set.
	if _, set := headers["Content-Type"]; !set {
		if _, set := headers["content-type"]; !set {
			headers["Content-Type"] = "application/json"
		}
	}
	if v, err := vm.RunString("JSON.stringify"); err == nil {
		if fn, ok := goja.AssertFunction(v); ok {
			if out, err := fn(goja.Undefined(), vm.ToValue(b)); err == nil {
				return out.String()
			}
		}
	}
	return fmt.Sprintf("%v", b)
}

// hostAllowed matches an exact hostname or a leading-wildcard pattern
// ("*.example.com"). An empty list means the script has no network at all.
func hostAllowed(host string, allow []string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, pattern := range allow {
		pattern = strings.ToLower(strings.TrimSpace(pattern))
		if pattern == "" {
			continue
		}
		if pattern == host {
			return true
		}
		if suffix, ok := strings.CutPrefix(pattern, "*."); ok {
			if host == suffix || strings.HasSuffix(host, "."+suffix) {
				return true
			}
		}
	}
	return false
}

// guardedTransport blocks connections to loopback, link-local and private
// addresses, checked after DNS resolution.
//
// The allow_net list is a hostname allowlist, so on its own it cannot stop a
// granted name from resolving to 169.254.169.254 (the cloud metadata endpoint)
// or to something inside the host's own network. Checking the resolved IP at
// dial time closes that, including for a name whose DNS answer changes between
// the check and the connection.
//
// Set BKN_SCRIPT_ALLOW_PRIVATE_NET=1 when a script must reach a service on the
// local network - a deliberate choice, not a default.
func guardedTransport() *http.Transport {
	allowPrivate := os.Getenv("BKN_SCRIPT_ALLOW_PRIVATE_NET") == "1"
	dialer := &net.Dialer{Timeout: 10 * time.Second}

	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if !allowPrivate {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if isPrivate(ip.IP) {
					return nil, fmt.Errorf("refusing to connect to the private address %s "+
						"(set BKN_SCRIPT_ALLOW_PRIVATE_NET=1 to allow)", ip.IP)
				}
			}
		}
		return dialer.DialContext(ctx, network, addr)
	}
	return t
}

func isPrivate(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}
