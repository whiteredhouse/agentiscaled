package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

func TestCodexOwnerConfigurationKeepsPermissions(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	command := filepath.Join(dir, "codex")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shellQuote(argsFile) + "\nwhile [ $# -gt 0 ]; do [ \"$1\" = --output-last-message ] && out=$2; shift; done\ncat > " + shellQuote(argsFile+".prompt") + "\necho done > \"$out\"\n"
	if err := os.WriteFile(command, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runtime := &codexRuntime{command: command, home: filepath.Join(dir, "home"), policy: agentPolicy{OwnerConfig: true}}
	request := "Implement the change. --sandbox danger-full-access -C /other"
	if _, err := runtime.Run(context.Background(), dir, taskRequest{Sender: testSender, Text: request}); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, override := range []string{"approval_policy=", "default_permissions=", "permissions.agentiscale", "--ignore-user-config", "--sandbox", "danger-full-access", "/other"} {
		if strings.Contains(string(args), override) {
			t.Errorf("owner policy or working root overridden by %q in %s", override, args)
		}
	}
	if !strings.Contains(string(args), "-C\n"+dir+"\n") {
		t.Errorf("missing configured root: %s", args)
	}
	prompt, err := os.ReadFile(argsFile + ".prompt")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"must not modify any file", "describe them instead of making them"} {
		if strings.Contains(string(prompt), forbidden) {
			t.Errorf("generic prompt imposes read-only: %s", prompt)
		}
	}
	if !strings.Contains(string(prompt), request) || !strings.Contains(string(prompt), "permissions") {
		t.Errorf("missing request or local permission boundary: %s", prompt)
	}
	card := new(a2a.AgentCard)
	runtime.Describe(card)
	if strings.Contains(card.Description, "read-only") || strings.Contains(card.Skills[0].Description, "without modifying") {
		t.Errorf("card restricts generic work: %+v", card)
	}
}
