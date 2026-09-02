package script

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dop251/goja"

	"github.com/javimosch/bkn/internal/kv"
	"github.com/javimosch/bkn/internal/store"
)

// Runner executes scripts against the core primitives.
type Runner struct {
	reg *Registry
	st  *store.Store
	kv  *kv.KV
}

func NewRunner(reg *Registry, st *store.Store, k *kv.KV) *Runner {
	return &Runner{reg: reg, st: st, kv: k}
}

// Result is the outcome of one execution.
type Result struct {
	Run   Run  `json:"run"`
	Value any  `json:"value"`
	OK    bool `json:"ok"`
}

// Run executes a stored script's main(input) and records the outcome.
//
// Every run gets a fresh goja Runtime: a Runtime is not safe for concurrent
// use, and a fresh one also means one script cannot leave state behind for the
// next.
func (r *Runner) Run(name string, input any) (Result, error) {
	s, err := r.reg.Get(name)
	if err != nil {
		return Result{}, err
	}
	if !s.Enabled {
		return Result{}, ErrDisabled
	}
	return r.Exec(s, input)
}

// Exec runs a script definition that need not be stored, which is what makes
// `script test` possible before anything is saved.
func (r *Runner) Exec(s Script, input any) (Result, error) {
	started := time.Now()
	run := Run{
		ID:        store.NewID(),
		Name:      s.Name,
		StartedAt: started.UTC().Format(time.RFC3339),
	}
	if input != nil {
		if b, err := json.Marshal(input); err == nil {
			run.Input = string(b)
		}
	}

	vm := goja.New()
	// Field names reach JS unchanged. Without this goja would expose Go field
	// names, so a record written as {"email":...} would read back as .Email.
	vm.SetFieldNameMapper(goja.TagFieldNameMapper("json", true))

	var logs strings.Builder
	host := r.newHost(vm, s, &logs)
	if err := vm.Set("bkn", host); err != nil {
		return Result{}, err
	}
	if err := vm.Set("console", map[string]any{
		"log":   host["log"],
		"error": host["log"],
		"warn":  host["log"],
		"info":  host["log"],
	}); err != nil {
		return Result{}, err
	}

	timeout := time.Duration(s.TimeoutMS) * time.Millisecond
	timer := time.AfterFunc(timeout, func() {
		vm.Interrupt(fmt.Sprintf("script exceeded its %dms timeout", s.TimeoutMS))
	})
	defer timer.Stop()

	finish := func(status, errMsg string, value any) (Result, error) {
		run.Status = status
		run.Error = errMsg
		run.Logs = logs.String()
		run.DurationMS = time.Since(started).Milliseconds()
		if value != nil {
			if b, err := json.Marshal(value); err == nil {
				run.Result = string(b)
			}
		}
		if s.Name != "" {
			if err := r.reg.RecordRun(run); err != nil {
				return Result{}, err
			}
		}
		return Result{Run: run, Value: value, OK: status == StatusOK}, nil
	}

	if _, err := vm.RunString(s.Code); err != nil {
		return finish(classify(err), cleanError(err), nil)
	}

	fn, ok := goja.AssertFunction(vm.Get("main"))
	if !ok {
		return finish(StatusError,
			"script must define a top-level function main(input); nothing else is called", nil)
	}

	value, err := fn(goja.Undefined(), vm.ToValue(input))
	if err != nil {
		return finish(classify(err), cleanError(err), nil)
	}

	var exported any
	if value != nil && !goja.IsUndefined(value) && !goja.IsNull(value) {
		exported = value.Export()
	}
	// Round-trip through JSON so the recorded result and the returned value are
	// the same shape a caller sees over HTTP.
	if b, err := json.Marshal(exported); err == nil {
		var normalized any
		if json.Unmarshal(b, &normalized) == nil {
			exported = normalized
		}
	}
	return finish(StatusOK, "", exported)
}

// cleanError makes a script error readable by a script author. goja appends
// the full stack, which for a host function is a chain of Go symbols that
// says nothing to someone debugging JavaScript - drop those frames and keep
// the ones inside the script.
func cleanError(err error) string {
	msg := strings.TrimPrefix(err.Error(), "GoError: ")
	if i := strings.Index(msg, " at github.com/javimosch/bkn/"); i >= 0 {
		msg = strings.TrimSpace(msg[:i])
	}
	return msg
}

func classify(err error) string {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		return StatusTimeout
	}
	return StatusError
}
