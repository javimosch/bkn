package main

import (
	"strings"
	"testing"

	"github.com/javimosch/bkn/internal/guide"
)

// cli-guide-spec §1 requires these fields. The guide is embedded, so a missing
// field is a build-time fact - catch it here rather than in an agent's parser.
func TestGuideHasEveryRequiredField(t *testing.T) {
	body, err := guide.Body("test")
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	for _, field := range []string{"bkn", "one_liner", "model", "loop", "concepts", "commands", "examples", "gotchas", "version"} {
		if _, ok := body[field]; !ok {
			t.Errorf("guide is missing the required field %q", field)
		}
	}
	if body["version"] != "test" {
		t.Errorf("version = %v, want the tool version injected at render time", body["version"])
	}
	for _, ex := range body["examples"].([]any) {
		e := ex.(map[string]any)
		if e["goal"] == nil || e["do"] == nil {
			t.Errorf("example missing goal or do: %v", e)
		}
	}
}

// help-json is the command catalog (cli-output-spec §4).
func TestHelpJSONCatalogIsComplete(t *testing.T) {
	c := helpJSON()
	for _, field := range []string{"version", "output", "commands", "exit_codes", "env"} {
		if _, ok := c[field]; !ok {
			t.Errorf("help-json is missing %q", field)
		}
	}
	if _, ok := c["exit_codes"].(map[string]string)["92"]; !ok {
		t.Error("help-json does not document exit code 92")
	}
}

// commandName extracts "auth member add" from
// "bkn auth member add <org> <user> [--role owner|admin|member]".
func commandName(usage string) string {
	var parts []string
	for _, tok := range strings.Fields(usage) {
		if tok == "bkn" {
			continue
		}
		if strings.HasPrefix(tok, "<") || strings.HasPrefix(tok, "[") || strings.HasPrefix(tok, "-") {
			break
		}
		parts = append(parts, tok)
	}
	return strings.Join(parts, " ")
}

// The guide and the command catalog must describe the same tool.
//
// This exists because they silently diverged: a whole namespace was added to
// the guide and to the dispatcher while help-json kept listing the old set,
// and the previous spot-check of five command names passed anyway. An agent
// reading help-json would simply not have known those commands existed.
func TestGuideAndHelpJSONAgree(t *testing.T) {
	body, err := guide.Body("test")
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	catalog := helpJSON()["commands"].(map[string]any)

	documented := map[string]bool{}
	for group, list := range body["commands"].(map[string]any) {
		for _, usage := range list.([]any) {
			name := commandName(usage.(string))
			if name == "" {
				t.Errorf("guide group %q has an unparseable usage string %q", group, usage)
				continue
			}
			documented[name] = true
			if _, ok := catalog[name]; !ok {
				t.Errorf("guide documents %q but help-json does not list it", name)
			}
		}
	}
	for name := range catalog {
		if !documented[name] {
			t.Errorf("help-json lists %q but the guide does not document it", name)
		}
	}
}
