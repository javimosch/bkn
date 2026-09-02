package main

import (
	"flag"
	"reflect"
	"testing"
)

// The stdlib flag package stops at the first positional argument, so
// `store put myapp/users --data '{}'` silently dropped --data. Agents write
// commands in the order the usage string shows them; this locks that in.
func TestParseFlagsAllowsInterleavedPositionals(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantData string
		wantID   string
		wantPos  []string
	}{
		{"flags after positionals", []string{"myapp/users", "--data", "{}", "--id", "u1"}, "{}", "u1", []string{"myapp/users"}},
		{"flags before positionals", []string{"--data", "{}", "myapp/users"}, "{}", "", []string{"myapp/users"}},
		{"interleaved", []string{"--data", "{}", "myapp/users", "--id", "u1"}, "{}", "u1", []string{"myapp/users"}},
		{"inline value", []string{"myapp/users", "--data={}"}, "{}", "", []string{"myapp/users"}},
		{"single dash", []string{"myapp/users", "-data", "{}"}, "{}", "", []string{"myapp/users"}},
		{"two positionals", []string{"myapp/users", "u1", "--data", "{}"}, "{}", "", []string{"myapp/users", "u1"}},
		{"terminator", []string{"--data", "{}", "--", "--not-a-flag"}, "{}", "", []string{"--not-a-flag"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			data := fs.String("data", "", "")
			id := fs.String("id", "", "")
			pos := parseFlags(fs, tc.args)

			if *data != tc.wantData {
				t.Errorf("--data = %q, want %q", *data, tc.wantData)
			}
			if *id != tc.wantID {
				t.Errorf("--id = %q, want %q", *id, tc.wantID)
			}
			if !reflect.DeepEqual(pos, tc.wantPos) {
				t.Errorf("positionals = %v, want %v", pos, tc.wantPos)
			}
		})
	}
}

// A boolean flag must not swallow the argument that follows it.
func TestParseFlagsBooleansDoNotConsumeTheNextArgument(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	public := fs.Bool("public", false, "")
	typ := fs.String("type", "", "")

	pos := parseFlags(fs, []string{"mykey", "--public", "myvalue", "--type", "string"})
	if !*public {
		t.Error("--public was not set")
	}
	if *typ != "string" {
		t.Errorf("--type = %q, want string", *typ)
	}
	if !reflect.DeepEqual(pos, []string{"mykey", "myvalue"}) {
		t.Errorf("positionals = %v, want [mykey myvalue]", pos)
	}
}
