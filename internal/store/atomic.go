package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Update operators and preconditions exist because `patch` was a lost update.
// It read a document, merged in Go, and wrote the whole thing back, so two
// concurrent patches of different fields kept only one of them. The core hit
// the same problem first: cron's claim() compare-and-sets a job's next run in
// one UPDATE precisely so two tickers cannot fire it twice. Applications had
// no way to say the same thing.
//
// Both are primitives rather than query features: they remove read-modify-write
// races from every caller, and userland cannot implement them safely — a script
// that reads, adds one, and writes back is the very race being fixed.

var (
	ErrBadOperator  = errors.New("unknown update operator")
	ErrOperandType  = errors.New("operator does not apply to this value")
	ErrPrecondition = errors.New("precondition failed")
	ErrConcurrent   = errors.New("document changed concurrently, retries exhausted")
)

// operators is deliberately closed and small. Each one earned its place by
// appearing in a real codebase's SQL, where it was expressed as an atomic
// UPDATE that no document store could serve.
var operators = map[string]struct{}{
	"$inc": {}, "$append": {}, "$push": {}, "$setIfEmpty": {},
}

// Operators lists the supported update operators, for help text and errors.
func Operators() []string { return []string{"$append", "$inc", "$push", "$setIfEmpty"} }

// asOperator reports whether a patch value is an operator expression: an object
// whose single key is one of the known operators. A plain object is still a
// plain value, so existing documents that happen to contain objects are safe.
func asOperator(v any) (string, any, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return "", nil, false
	}
	for k, operand := range m {
		if !strings.HasPrefix(k, "$") {
			return "", nil, false
		}
		return k, operand, true
	}
	return "", nil, false
}

// applyOperator computes a field's new value from its current one. A missing
// field is treated as the operator's identity: incrementing an absent counter
// yields the operand, appending to an absent string yields the operand.
func applyOperator(op string, cur any, operand any) (any, error) {
	if _, known := operators[op]; !known {
		return nil, fmt.Errorf("%w: %s (known: %s)", ErrBadOperator, op, strings.Join(Operators(), ", "))
	}
	switch op {
	case "$inc":
		delta, ok := toFloat(operand)
		if !ok {
			return nil, fmt.Errorf("%w: $inc needs a number, got %T", ErrOperandType, operand)
		}
		if cur == nil {
			return delta, nil
		}
		base, ok := toFloat(cur)
		if !ok {
			return nil, fmt.Errorf("%w: $inc on a %T field", ErrOperandType, cur)
		}
		return base + delta, nil

	case "$append":
		suffix, ok := operand.(string)
		if !ok {
			return nil, fmt.Errorf("%w: $append needs a string, got %T", ErrOperandType, operand)
		}
		if cur == nil {
			return suffix, nil
		}
		base, ok := cur.(string)
		if !ok {
			return nil, fmt.Errorf("%w: $append on a %T field", ErrOperandType, cur)
		}
		return base + suffix, nil

	case "$push":
		if cur == nil {
			return []any{operand}, nil
		}
		base, ok := cur.([]any)
		if !ok {
			return nil, fmt.Errorf("%w: $push on a %T field", ErrOperandType, cur)
		}
		return append(append([]any{}, base...), operand), nil

	case "$setIfEmpty":
		if isEmpty(cur) {
			return operand, nil
		}
		return cur, nil
	}
	return nil, ErrBadOperator
}

// isEmpty is what "$setIfEmpty" and "--if-absent" both mean: the field is
// missing, null, or the empty string. A zero number and false are values.
func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	}
	return false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// PatchOptions carries preconditions. They are checked against the document a
// patch is about to be applied to, and the write only lands on that exact
// document — so a precondition that held when it was checked still holds when
// the write commits.
//
// Conditions are ANDed. There is deliberately no OR, matching the store's
// query surface: an either/or guard belongs in the caller's control flow, not
// in a predicate language growing inside bkn.
type PatchOptions struct {
	If       map[string]string // field must equal this value, compared as text
	IfAbsent []string          // field must be missing, null, or empty
}

func (o PatchOptions) empty() bool { return len(o.If) == 0 && len(o.IfAbsent) == 0 }

// check evaluates the preconditions against a document.
func (o PatchOptions) check(doc map[string]any) error {
	for field, want := range o.If {
		got, present := doc[field]
		if !present {
			return fmt.Errorf("%w: %s is absent, want %q", ErrPrecondition, field, want)
		}
		if asText(got) != want {
			return fmt.Errorf("%w: %s is %q, want %q", ErrPrecondition, field, asText(got), want)
		}
	}
	for _, field := range o.IfAbsent {
		if !isEmpty(doc[field]) {
			return fmt.Errorf("%w: %s is %q, want it absent", ErrPrecondition, field, asText(doc[field]))
		}
	}
	return nil
}

// asText renders a scalar the way --where does, so `--if status=ok` compares
// the same way a filter would.
func asText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
