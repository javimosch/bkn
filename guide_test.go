package main

import (
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
	cmds := c["commands"].(map[string]any)
	for _, want := range []string{"guide", "store put", "store find", "kv set", "kv rekey", "serve", "daemon start"} {
		if _, ok := cmds[want]; !ok {
			t.Errorf("help-json does not list %q", want)
		}
	}
	if _, ok := c["exit_codes"].(map[string]string)["92"]; !ok {
		t.Error("help-json does not document exit code 92")
	}
}
