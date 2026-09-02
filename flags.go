package main

import (
	"flag"
	"strings"

	"github.com/javimosch/bkn/internal/out"
)

// boolFlag is the unexported interface the flag package uses to decide whether
// a flag consumes the following argument.
type boolFlag interface{ IsBoolFlag() bool }

// parseFlags parses args against fs and returns the positional arguments,
// allowing flags and positionals to be interleaved.
//
// The stdlib flag package stops parsing at the first non-flag argument, so
// `store put myapp/users --data '{}'` would silently ignore --data. Agents
// write commands in the order the usage string shows them, so the parser
// accommodates that rather than the other way round.
func parseFlags(fs *flag.FlagSet, args []string) []string {
	var flagArgs, positional []string

	for i := 0; i < len(args); i++ {
		a := args[i]

		if a == "--" { // everything after is positional, by convention
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(a) < 2 || a[0] != '-' {
			positional = append(positional, a)
			continue
		}

		flagArgs = append(flagArgs, a)
		name, hasInline := strings.CutPrefix(a, "--")
		if !hasInline {
			name = strings.TrimPrefix(a, "-")
		}
		if n, _, inline := strings.Cut(name, "="); inline {
			_ = n
			continue // --flag=value carries its own value
		}

		f := fs.Lookup(name)
		if f == nil {
			continue // let fs.Parse report the unknown flag
		}
		if bf, ok := f.Value.(boolFlag); ok && bf.IsBoolFlag() {
			continue // booleans never consume the next argument
		}
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}

	if err := fs.Parse(flagArgs); err != nil {
		out.Fail(out.InvalidArguments, "invalid_arguments", err.Error())
	}
	return positional
}
