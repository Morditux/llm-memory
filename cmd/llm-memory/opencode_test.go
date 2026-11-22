package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Morditux/llm-memory/internal/config"
)

func TestStripJSONC(t *testing.T) {
	input := "\xef\xbb\xbf{\n" +
		"  // Line comment\n" +
		`  "$schema": "https://opencode.ai/config.json", // inline comment` + "\n" +
		"  /* Multi-line\n" +
		"     block comment with \"quotes\" and // slashes */\n" +
		`  "note": "keep /* this */ and // this and \"escaped\" trailing, }",` + "\n" +
		`  "arr": [1, 2, ],` + "\n" +
		"}\n"

	var parsed map[string]any
	hasComments, err := decodeJSONC([]byte(input), &parsed)
	if err != nil {
		t.Fatalf("decodeJSONC failed: %v", err)
	}
	if !hasComments {
		t.Errorf("expected hasComments=true")
	}
	if parsed["$schema"] != "https://opencode.ai/config.json" {
		t.Errorf("schema mangled: %v", parsed["$schema"])
	}
	if parsed["note"] != `keep /* this */ and // this and "escaped" trailing, }` {
		t.Errorf("string content mangled: %q", parsed["note"])
	}
	arr, ok := parsed["arr"].([]any)
	if !ok || len(arr) != 2 {
		t.Errorf("arr unexpected: %v", parsed["arr"])
	}

	// Plain JSON without comments sets hasComments=false.
	var plain map[string]any
	hasComments, err = decodeJSONC([]byte(`{"a":1}`), &plain)
	if err != nil || hasComments || plain["a"] != float64(1) {
		t.Errorf("plain JSON: hasComments=%v err=%v parsed=%v", hasComments, err, plain)
	}

	// Invalid JSON returns an error.
	if _, err := decodeJSONC([]byte(`{"a": }`), &plain); err == nil {
		t.Errorf("expected error for invalid JSONC")
	}
}

func TestOpenCodeV2SetupAndJSONC(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	db := filepath.Join(home, "mem.db")

	runEnv := func(args ...string) result {
		t.Helper()
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), args, app{
			stdin:  strings.NewReader(""),
			stdout: &stdout,
			stderr: &stderr,
			env: config.Env{
				Getenv:      func(string) string { return "" },
				UserHomeDir: func() (string, error) { return home, nil },
				Getwd:       func() (string, error) { return proj, nil },
			},
		})
		return result{stdout.String(), stderr.String(), err}
	}

	// 1. Existing global opencode.jsonc with comments, trailing commas, other server, and legacy v1 entry.
	ocDir := filepath.Join(home, ".config", "opencode")
	if err := os.MkdirAll(ocDir, 0o755); err != nil {
		t.Fatal(err)
	}
	jsoncPath := filepath.Join(ocDir, "opencode.jsonc")
	initialJSONC := `{
  // User comment
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "llm-memory": {
      "type": "local",
      "command": ["old-bin", "serve"],
      "enabled": true,
    },
    "servers": {
      "other": {
        "type": "remote",
        "url": "https://example.com/mcp",
      },
    },
  },
}
`
	if err := os.WriteFile(jsoncPath, []byte(initialJSONC), 0o600); err != nil {
		t.Fatal(err)
	}

	r := runEnv("setup", "--db", db, "--write", "opencode")
	if r.err != nil {
		t.Fatalf("setup --write opencode: %v\n%s", r.err, r.errOut)
	}
	if !strings.Contains(r.errOut, "JSONC comments") {
		t.Errorf("expected JSONC comment warning on stderr, got %q", r.errOut)
	}

	data, err := os.ReadFile(jsoncPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("written file is not valid JSON: %v\n%s", err, data)
	}
	mcpObj := doc["mcp"].(map[string]any)
	if _, hasLegacy := mcpObj["llm-memory"]; hasLegacy {
		t.Errorf("legacy v1 mcp.llm-memory was not cleaned up:\n%s", data)
	}
	servers := mcpObj["servers"].(map[string]any)
	if _, hasOther := servers["other"]; !hasOther {
		t.Errorf("existing server 'other' was lost:\n%s", data)
	}
	lmEntry := servers["llm-memory"].(map[string]any)
	if _, hasEnabled := lmEntry["enabled"]; hasEnabled {
		t.Errorf("opencode v2 entry must not contain 'enabled':\n%s", data)
	}
	if lmEntry["type"] != "local" {
		t.Errorf("unexpected type: %v", lmEntry["type"])
	}

	// Remove cleans up mcp.servers.llm-memory and global AGENTS.md.
	if r := runEnv("setup", "--remove", "opencode"); r.err != nil {
		t.Fatalf("setup --remove opencode: %v\n%s", r.err, r.errOut)
	}
	data, _ = os.ReadFile(jsoncPath)
	if strings.Contains(string(data), "llm-memory") || !strings.Contains(string(data), "other") {
		t.Errorf("after remove:\n%s", data)
	}
	if _, err := os.Stat(filepath.Join(ocDir, "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected empty global AGENTS.md to be removed, stat err=%v", err)
	}

	// 2. Project scope (--project) detects .opencode/opencode.jsonc if present, or creates ./opencode.json.
	if r := runEnv("setup", "--db", db, "--write", "--project", "opencode"); r.err != nil {
		t.Fatalf("setup --write --project opencode: %v\n%s", r.err, r.errOut)
	}
	projCfg := filepath.Join(proj, "opencode.json")
	if _, err := os.Stat(projCfg); err != nil {
		t.Errorf("expected %s to exist: %v", projCfg, err)
	}
	projAgents := filepath.Join(proj, "AGENTS.md")
	if b, err := os.ReadFile(projAgents); err != nil || !strings.Contains(string(b), instructionStartMarker) {
		t.Errorf("expected project AGENTS.md (%v):\n%s", err, b)
	}

	// If .opencode/opencode.jsonc exists in another project, --project targets it.
	proj2 := t.TempDir()
	ocSub := filepath.Join(proj2, ".opencode", "opencode.jsonc")
	if err := os.MkdirAll(filepath.Dir(ocSub), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ocSub, []byte("{// empty\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out2, err2 bytes.Buffer
	err = run(context.Background(), []string{"setup", "--db", db, "--write", "--project", "opencode"}, app{
		stdin: strings.NewReader(""), stdout: &out2, stderr: &err2,
		env: config.Env{
			Getenv:      func(string) string { return "" },
			UserHomeDir: func() (string, error) { return home, nil },
			Getwd:       func() (string, error) { return proj2, nil },
		},
	})
	if err != nil {
		t.Fatalf("proj2 setup: %v\n%s", err, err2.String())
	}
	if b, err := os.ReadFile(ocSub); err != nil || !strings.Contains(string(b), `"llm-memory"`) {
		t.Errorf("expected .opencode/opencode.jsonc to be updated (%v):\n%s", err, b)
	}
}

func TestOpenCodeV2HooksAndDoctor(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proj, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(home, "mem.db")

	runEnv := func(stdin string, args ...string) result {
		t.Helper()
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), args, app{
			stdin:  strings.NewReader(stdin),
			stdout: &stdout,
			stderr: &stderr,
			env: config.Env{
				Getenv:      func(string) string { return "" },
				UserHomeDir: func() (string, error) { return home, nil },
				Getwd:       func() (string, error) { return proj, nil },
			},
		})
		return result{stdout.String(), stderr.String(), err}
	}

	projName := filepath.Base(proj)
	if r := runEnv("", "remember", "--db", db, "--namespace", projName, "--kind", "decision", "Use OpenCode v2 plugin"); r.err != nil {
		t.Fatal(r.err)
	}
	if r := runEnv("", "remember", "--db", db, "--namespace", projName, "--kind", "task_state", "Finish OpenCode hooks"); r.err != nil {
		t.Fatal(r.err)
	}

	// 1. Live hook execution for SessionStart & PreCompact.
	r := runEnv(`{"cwd":"`+filepath.ToSlash(proj)+`"}`, "hook", "--db", db, "--namespace", "auto", "opencode")
	if r.err != nil {
		t.Fatalf("hook opencode SessionStart: %v\n%s", r.err, r.errOut)
	}
	var out struct {
		Event   string `json:"event"`
		Context string `json:"context"`
	}
	if err := json.Unmarshal([]byte(r.out), &out); err != nil {
		t.Fatalf("invalid hook JSON: %v\n%s", err, r.out)
	}
	if out.Event != "SessionStart" || !strings.Contains(out.Context, "Use OpenCode v2 plugin") {
		t.Errorf("unexpected SessionStart output: %+v", out)
	}

	r = runEnv(`{"cwd":"`+filepath.ToSlash(proj)+`"}`, "hook", "--db", db, "--namespace", "auto", "--event", "PreCompact", "opencode")
	if r.err != nil {
		t.Fatalf("hook opencode PreCompact: %v\n%s", r.err, r.errOut)
	}
	if err := json.Unmarshal([]byte(r.out), &out); err != nil || out.Event != "PreCompact" || !strings.Contains(out.Context, "Finish OpenCode hooks") {
		t.Errorf("unexpected PreCompact output (%v): %+v", err, out)
	}

	// Stop event is rejected for OpenCode.
	if r := runEnv("", "hook", "--event", "Stop", "opencode"); !errors.Is(r.err, errUsage) {
		t.Errorf("expected errUsage for Stop on opencode, got %v", r.err)
	}
	if r := runEnv("", "setup", "--hooks", "--events", "SessionStart,Stop", "opencode"); !errors.Is(r.err, errUsage) {
		t.Errorf("expected errUsage for setup --hooks --events Stop on opencode, got %v", r.err)
	}

	// 2. Print mode emits v2 plugin JS.
	r = runEnv("", "setup", "--hooks", "--db", db, "opencode")
	if r.err != nil {
		t.Fatalf("setup --hooks opencode: %v", r.err)
	}
	for _, want := range []string{
		`import { Plugin } from "@opencode/plugin"`,
		`Plugin.define(`,
		`id: "llm-memory"`,
		`ctx.session.hook("context"`,
		`ctx.session.hook("compaction"`,
	} {
		if !strings.Contains(r.out, want) {
			t.Errorf("printed plugin missing %q:\n%s", want, r.out)
		}
	}

	// 3. Write global plugin + project plugin + hook --write with extra flags.
	if r := runEnv("", "setup", "--hooks", "--write", "--db", db, "opencode"); r.err != nil {
		t.Fatalf("setup --hooks --write opencode: %v", r.err)
	}
	globalPlugin := filepath.Join(home, ".config", "opencode", "plugins", "llm-memory.js")
	if b, err := os.ReadFile(globalPlugin); err != nil || !strings.Contains(string(b), `"--namespace"`) {
		t.Errorf("global plugin unexpected (%v):\n%s", err, b)
	}

	if r := runEnv("", "hook", "--write", "--project", "--db", db, "--max-chars", "3000", "--focus", "auth flow", "opencode"); r.err != nil {
		t.Fatalf("hook --write --project opencode: %v\n%s", r.err, r.errOut)
	}
	projPlugin := filepath.Join(proj, ".opencode", "plugins", "llm-memory.js")
	if b, err := os.ReadFile(projPlugin); err != nil || !strings.Contains(string(b), `"3000"`) || !strings.Contains(string(b), `"auth flow"`) {
		t.Errorf("project plugin unexpected (%v):\n%s", err, b)
	}

	// 4. Doctor checks both opencode.jsonc MCP and plugins/llm-memory.js (valid and broken binary).
	ocJSONC := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	if err := os.WriteFile(ocJSONC, []byte(`{"mcp":{"servers":{"llm-memory":{"type":"local","command":["/nonexistent/llm-memory-bin","serve"]}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	docRes := runEnv("", "doctor", "--db", db, "--json")
	if docRes.err != nil {
		t.Fatalf("doctor: %v\n%s", docRes.err, docRes.errOut)
	}
	var doc struct {
		Clients []clientCheck `json:"clients"`
	}
	if err := json.Unmarshal([]byte(docRes.out), &doc); err != nil {
		t.Fatalf("unmarshal doctor: %v", err)
	}
	var sawBrokenMCP, sawOKHook bool
	for _, c := range doc.Clients {
		if c.Client == "opencode" && c.Kind == "mcp" && !c.OK {
			sawBrokenMCP = true
		}
		if c.Client == "opencode" && c.Kind == "hook" && c.OK {
			sawOKHook = true
		}
	}
	if !sawBrokenMCP || !sawOKHook {
		t.Errorf("unexpected doctor checks for opencode: %+v", doc.Clients)
	}

	// 5. Remove refuses non-llm-memory file via --config, and deletes real plugin.
	otherJS := filepath.Join(home, "other.js")
	if err := os.WriteFile(otherJS, []byte("export default {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := runEnv("", "hook", "--remove", "--config", otherJS, "opencode"); r.err == nil {
		t.Errorf("expected error removing non-llm-memory plugin")
	}
	if r := runEnv("", "setup", "--hooks", "--remove", "--project", "opencode"); r.err != nil {
		t.Fatalf("remove project plugin: %v", r.err)
	}
	if _, err := os.Stat(projPlugin); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected %s to be deleted, err=%v", projPlugin, err)
	}
}

func TestSetupAutomaticInstructions(t *testing.T) {
	home := t.TempDir()
	proj := t.TempDir()
	db := filepath.Join(home, "mem.db")

	runEnv := func(args ...string) result {
		t.Helper()
		var stdout, stderr bytes.Buffer
		err := run(context.Background(), args, app{
			stdin:  strings.NewReader(""),
			stdout: &stdout,
			stderr: &stderr,
			env: config.Env{
				Getenv:      func(string) string { return "" },
				UserHomeDir: func() (string, error) { return home, nil },
				Getwd:       func() (string, error) { return proj, nil },
			},
		})
		return result{stdout.String(), stderr.String(), err}
	}

	// Preserve existing user content and avoid duplicating legacy manual sections.
	claudePath := filepath.Join(home, ".claude", "CLAUDE.md")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := "# Personal rules\n\nBe concise.\n\n## Shared memory (llm-memory MCP server)\n\nOld manual text.\n\n## Other section\n\nKeep me.\n"
	if err := os.WriteFile(claudePath, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	// Install twice (idempotent).
	for range 2 {
		if r := runEnv("setup", "--db", db, "--write", "claude-code"); r.err != nil {
			t.Fatalf("setup claude-code: %v\n%s", r.err, r.errOut)
		}
	}
	got, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, "Be concise.") || !strings.Contains(s, "## Other section\n\nKeep me.") {
		t.Errorf("existing content lost:\n%s", s)
	}
	if strings.Contains(s, "Old manual text.") {
		t.Errorf("legacy manual block was not replaced:\n%s", s)
	}
	if strings.Count(s, instructionStartMarker) != 1 || strings.Count(s, instructionHeading) != 1 {
		t.Errorf("expected exactly 1 instruction block:\n%s", s)
	}

	// Remove removes only our block and keeps surrounding user sections.
	if r := runEnv("setup", "--remove", "claude-code"); r.err != nil {
		t.Fatalf("remove claude-code: %v", r.err)
	}
	got, _ = os.ReadFile(claudePath)
	s = string(got)
	if strings.Contains(s, instructionStartMarker) || !strings.Contains(s, "Be concise.") || !strings.Contains(s, "Keep me.") {
		t.Errorf("unexpected CLAUDE.md after remove:\n%s", s)
	}

	// --no-instructions skips creating instruction files.
	if r := runEnv("setup", "--db", db, "--write", "--no-instructions", "codex"); r.err != nil {
		t.Fatalf("setup --no-instructions codex: %v", r.err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codex", "AGENTS.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected no AGENTS.md with --no-instructions, err=%v", err)
	}

	// Project-scoped setup for codex writes ./AGENTS.md and ./.codex/config.toml.
	if r := runEnv("setup", "--db", db, "--write", "--project", "codex"); r.err != nil {
		t.Fatalf("setup --project codex: %v\n%s", r.err, r.errOut)
	}
	if _, err := os.Stat(filepath.Join(proj, ".codex", "config.toml")); err != nil {
		t.Errorf("expected project .codex/config.toml: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(proj, "AGENTS.md")); err != nil || !strings.Contains(string(b), instructionStartMarker) {
		t.Errorf("expected project AGENTS.md (%v):\n%s", err, b)
	}
}
