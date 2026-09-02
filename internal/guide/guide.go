// Package guide serves the embedded operator manual per cli-guide-spec v1.0.
// The guide travels inside the binary: it is never fetched at runtime, so it
// works offline and can never drift from the build it ships with.
package guide

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed guide.json
var raw []byte

// Body returns the guide as a generic object, with the tool version injected
// so a reader can detect a stale build.
func Body(version string) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	m["version"] = version
	return m, nil
}

// Human renders the guide as readable markdown for the --human flag.
func Human(version string) (string, error) {
	m, err := Body(version)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# bkn %s\n\n%v\n\n", version, m["bkn"])
	fmt.Fprintf(&b, "%v\n\n", m["one_liner"])

	section := func(title string, v any) {
		fmt.Fprintf(&b, "## %s\n\n", title)
		switch t := v.(type) {
		case map[string]any:
			keys := make([]string, 0, len(t))
			for k := range t {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				if list, ok := t[k].([]any); ok {
					fmt.Fprintf(&b, "**%s**\n", k)
					for _, item := range list {
						fmt.Fprintf(&b, "    %v\n", item)
					}
					b.WriteString("\n")
					continue
				}
				fmt.Fprintf(&b, "- **%s** — %v\n", k, t[k])
			}
		case []any:
			for _, item := range t {
				if ex, ok := item.(map[string]any); ok {
					fmt.Fprintf(&b, "- %v\n", ex["goal"])
					if steps, ok := ex["do"].([]any); ok {
						for _, s := range steps {
							fmt.Fprintf(&b, "      %v\n", s)
						}
					}
					continue
				}
				fmt.Fprintf(&b, "- %v\n", item)
			}
		}
		b.WriteString("\n")
	}

	section("Model", m["model"])
	section("Loop", m["loop"])
	section("Concepts", m["concepts"])
	section("Commands", m["commands"])
	section("Examples", m["examples"])
	section("Gotchas", m["gotchas"])
	return b.String(), nil
}

// LLMsTxt is the plain-text breadcrumb served at GET /llms.txt: the front door
// for an agent that knows only the host.
func LLMsTxt(version string) string {
	return strings.Join([]string{
		"# bkn " + version,
		"",
		"A single-binary backend core: namespaced document collections (store) and",
		"typed settings (kv) over embedded SQLite. The CLI is the primary interface;",
		"these HTTP routes mirror it.",
		"",
		"Full mental model:  GET /guide   (or run: bkn guide)",
		"Command catalog:    bkn help-json",
		"",
		"Quick start:",
		"  curl -s $HOST/v1/store/myapp/users",
		"  curl -s $HOST/v1/kv?public=1",
		"  curl -s $HOST/_health",
		"",
	}, "\n")
}
