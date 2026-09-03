package script_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/javimosch/bkn/internal/auth"
	"github.com/javimosch/bkn/internal/db"
	"github.com/javimosch/bkn/internal/kv"
	"github.com/javimosch/bkn/internal/script"
	"github.com/javimosch/bkn/internal/store"
)

func setup(t *testing.T) (*script.Registry, *script.Runner, *store.Store) {
	t.Helper()
	t.Setenv("BKN_DATA", t.TempDir()+"/test.db")
	t.Setenv("BKN_ENCRYPTION_KEY", strings.Repeat("a", 32))
	conn, err := db.Open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return newParts(t, conn)
}

func newParts(t *testing.T, conn *sql.DB) (*script.Registry, *script.Runner, *store.Store) {
	t.Helper()
	kr, _ := kv.LoadKeyring()
	st := store.New(conn)
	k := kv.New(conn, kr, 0)
	reg := script.NewRegistry(conn)
	a, err := auth.New(conn, k)
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	return reg, script.NewRunner(reg, st, k, a), st
}

func run(t *testing.T, r *script.Runner, code string, input any) script.Result {
	t.Helper()
	res, err := r.Exec(script.Script{Code: code, TimeoutMS: 2000, Enabled: true}, input)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	return res
}

func TestScriptReadsAndWritesTheStore(t *testing.T) {
	_, runner, st := setup(t)
	ref, _ := store.ParseRef("app/items")
	if _, err := st.Put(ref, "i1", map[string]any{"n": 1}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res := run(t, runner, `
		function main(input) {
			const got = bkn.store.get("app/items", "i1");
			bkn.store.put("app/items", {n: input.n}, "i2");
			return { read: got.n, count: bkn.store.list("app/items", {}).length };
		}`, map[string]any{"n": 2})

	if !res.OK {
		t.Fatalf("run failed: %s", res.Run.Error)
	}
	v := res.Value.(map[string]any)
	if v["read"] != float64(1) || v["count"] != float64(2) {
		t.Errorf("value = %v", v)
	}
}

// A missing record is null, not an exception: forcing try/catch around every
// lookup is not how JS reads.
func TestMissingRecordsAreNull(t *testing.T) {
	_, runner, _ := setup(t)
	res := run(t, runner, `
		function main() {
			return {
				get: bkn.store.get("app/none", "x"),
				find: bkn.store.find("app/none", {a: 1}),
				kv: bkn.kv.get("nothing.here"),
				del: bkn.store.delete("app/none", "x")
			};
		}`, nil)
	if !res.OK {
		t.Fatalf("run failed: %s", res.Run.Error)
	}
	v := res.Value.(map[string]any)
	for _, k := range []string{"get", "find", "kv"} {
		if v[k] != nil {
			t.Errorf("%s = %v, want null", k, v[k])
		}
	}
	if v["del"] != false {
		t.Errorf("delete of a missing record = %v, want false", v["del"])
	}
}

func TestScriptUsesKVIncludingSecrets(t *testing.T) {
	_, runner, _ := setup(t)
	res := run(t, runner, `
		function main() {
			bkn.kv.set("app.token", "s3cret", {type: "encrypted"});
			return bkn.kv.get("app.token");
		}`, nil)
	if !res.OK {
		t.Fatalf("run failed: %s", res.Run.Error)
	}
	if res.Value != "s3cret" {
		t.Errorf("value = %v, want the decrypted secret", res.Value)
	}
}

// The sandbox boundary is the whole point of this primitive, so it gets a test
// that names every escape a script author might reach for.
func TestSandboxDeniesHostAccess(t *testing.T) {
	_, runner, _ := setup(t)
	for _, probe := range []string{
		`require("fs")`,
		`process.exit(1)`,
		`globalThis.fetch("http://example.com")`,
		`setTimeout(function(){}, 1)`,
		`new (Function.prototype.bind.call(Function, null, "return process")())()`,
	} {
		res := run(t, runner, "function main(){ return "+probe+" }", nil)
		if res.OK && res.Value != nil {
			t.Errorf("%s returned %v instead of failing", probe, res.Value)
		}
	}
}

func TestNetworkIsDeniedUnlessAllowListed(t *testing.T) {
	_, runner, _ := setup(t)
	res, err := runner.Exec(script.Script{
		Code:      `function main(){ return bkn.http.fetch("https://example.com") }`,
		TimeoutMS: 2000,
		Enabled:   true,
	}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.OK {
		t.Fatal("fetch succeeded with an empty allow_net")
	}
	if !strings.Contains(res.Run.Error, "allow_net") {
		t.Errorf("error = %q, want it to name allow_net", res.Run.Error)
	}
}

func TestAllowListMatching(t *testing.T) {
	cases := []struct {
		host  string
		allow []string
		want  bool
	}{
		{"api.stripe.com", []string{"api.stripe.com"}, true},
		{"api.stripe.com", []string{}, false},
		{"api.stripe.com", []string{"stripe.com"}, false},
		{"api.stripe.com", []string{"*.stripe.com"}, true},
		{"stripe.com", []string{"*.stripe.com"}, true},
		{"evilstripe.com", []string{"*.stripe.com"}, false},
		{"API.Stripe.COM", []string{"api.stripe.com"}, true},
	}
	for _, tc := range cases {
		if got := script.HostAllowedForTest(tc.host, tc.allow); got != tc.want {
			t.Errorf("hostAllowed(%q, %v) = %v, want %v", tc.host, tc.allow, got, tc.want)
		}
	}
}

// An unbounded script must not be able to wedge the process.
func TestTimeoutInterruptsAnInfiniteLoop(t *testing.T) {
	_, runner, _ := setup(t)
	res, err := runner.Exec(script.Script{
		Code:      `function main(){ while(true){} }`,
		TimeoutMS: 200,
		Enabled:   true,
	}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Run.Status != script.StatusTimeout {
		t.Errorf("status = %q, want timeout", res.Run.Status)
	}
	if res.Run.DurationMS > 2000 {
		t.Errorf("took %dms, want the timeout to have fired", res.Run.DurationMS)
	}
}

func TestMissingMainIsAClearError(t *testing.T) {
	_, runner, _ := setup(t)
	res := run(t, runner, `const x = 1;`, nil)
	if res.OK {
		t.Fatal("a script with no main() succeeded")
	}
	if !strings.Contains(res.Run.Error, "main(input)") {
		t.Errorf("error = %q, want it to name main(input)", res.Run.Error)
	}
}

// goja reports host failures with a Go symbol chain that means nothing to
// someone debugging JavaScript.
func TestErrorsDoNotLeakGoInternals(t *testing.T) {
	_, runner, _ := setup(t)
	res := run(t, runner, `function main(){ bkn.store.get("not-a-ref", "x") }`, nil)
	if res.OK {
		t.Fatal("an invalid ref was accepted")
	}
	if strings.Contains(res.Run.Error, "github.com/javimosch") {
		t.Errorf("error leaks internal frames: %q", res.Run.Error)
	}
}

func TestRunsAreRecordedWithLogs(t *testing.T) {
	reg, runner, _ := setup(t)
	if _, err := reg.Create(script.Script{
		Name: "logger",
		Code: `function main(){ console.log("hello", 42); return {done: true} }`,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := runner.Run("logger", map[string]any{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	runs, err := reg.Runs("logger", 10)
	if err != nil || len(runs) != 1 {
		t.Fatalf("Runs = %d, %v", len(runs), err)
	}
	if runs[0].Status != script.StatusOK {
		t.Errorf("status = %q", runs[0].Status)
	}
	if !strings.Contains(runs[0].Logs, "hello 42") {
		t.Errorf("logs = %q, want the console.log output", runs[0].Logs)
	}
}

// Editing code must not silently reset a timeout or an allowlist.
func TestUpdateOnlyChangesWhatIsPassed(t *testing.T) {
	reg, _, _ := setup(t)
	if _, err := reg.Create(script.Script{
		Name: "keeper", Code: "function main(){}", TimeoutMS: 9000,
		AllowNet: []string{"api.stripe.com"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	code := "function main(){ return 1 }"
	got, err := reg.Update("keeper", &code, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.TimeoutMS != 9000 {
		t.Errorf("timeout = %d, want 9000 preserved", got.TimeoutMS)
	}
	if len(got.AllowNet) != 1 || got.AllowNet[0] != "api.stripe.com" {
		t.Errorf("allow_net = %v, want it preserved", got.AllowNet)
	}
}

func TestDisabledScriptsDoNotRun(t *testing.T) {
	reg, runner, _ := setup(t)
	if _, err := reg.Create(script.Script{Name: "off", Code: "function main(){ return 1 }"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	no := false
	if _, err := reg.Update("off", nil, nil, nil, nil, &no); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := runner.Run("off", nil); err != script.ErrDisabled {
		t.Errorf("Run of a disabled script = %v, want ErrDisabled", err)
	}
}

func TestDuplicateNameAndBadNameAreRejected(t *testing.T) {
	reg, _, _ := setup(t)
	if _, err := reg.Create(script.Script{Name: "dup", Code: "function main(){}"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := reg.Create(script.Script{Name: "dup", Code: "function main(){}"}); err != script.ErrExists {
		t.Errorf("duplicate Create = %v, want ErrExists", err)
	}
	for _, bad := range []string{"Bad", "9lives", "has space", ""} {
		if _, err := reg.Create(script.Script{Name: bad, Code: "function main(){}"}); err == nil {
			t.Errorf("Create accepted the bad name %q", bad)
		}
	}
}
