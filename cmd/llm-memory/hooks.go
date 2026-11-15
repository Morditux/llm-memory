package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Morditux/llm-memory/internal/briefing"
	"github.com/Morditux/llm-memory/internal/config"
	"github.com/Morditux/llm-memory/internal/store"
)

// Session-start hooks: the client runs `llm-memory hook <client>` when a
// session starts and injects the briefing into the model context, so the
// memory no longer depends on the LLM remembering to call get_context.

// hookClients lists the clients whose SessionStart hook is supported.
var hookClients = []string{"antigravity", "claude-code", "codex"}

const (
	// hookStdinLimit caps the JSON payload read from the client.
	hookStdinLimit = 1 << 20
	// hookStdinWait bounds the wait for a client that never closes stdin.
	hookStdinWait = 2 * time.Second
	// hookTimeout is the per-handler timeout written into client configs (seconds).
	hookTimeout = 15
	// hookPreamble tells the model where the injected text comes from.
	hookPreamble = "The following briefing was loaded from the llm-memory shared memory (MCP server) at session start. " +
		"You do not need to call get_context again for this namespace unless you want another focus or namespace.\n\n"
)

// hookPayload holds the stdin fields used to locate the project. Claude Code
// and Codex send cwd; Antigravity sends workspacePaths.
type hookPayload struct {
	Cwd            string   `json:"cwd"`
	WorkspacePaths []string `json:"workspacePaths"`
}

func (p hookPayload) dir() string {
	if p.Cwd != "" {
		return p.Cwd
	}
	for _, w := range p.WorkspacePaths {
		if w != "" {
			return w
		}
	}
	return ""
}

func isHookClient(c string) bool { return slices.Contains(hookClients, c) }

func unknownHookClient(w io.Writer, c string) error {
	fmt.Fprintf(w, "hooks are not supported for %q (expected %s)\n", c, strings.Join(hookClients, ", "))
	return errUsage
}

// readHookPayload reads the client's JSON payload. It never blocks for long
// and never fails: a missing or invalid payload just means "no project dir".
func readHookPayload(r io.Reader) hookPayload {
	var p hookPayload
	if r == nil {
		return p
	}
	if f, ok := r.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			return p // interactive terminal: nothing to read
		}
	}
	ch := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(io.LimitReader(r, hookStdinLimit))
		ch <- b
	}()
	select {
	case b := <-ch:
		_ = json.Unmarshal(b, &p)
	case <-time.After(hookStdinWait):
	}
	return p
}

// cmdHook prints the briefing in the format the client's SessionStart hook
// expects. Errors are reported on stderr but the exit status stays 0, so a
// memory problem never prevents a session from starting.
func (a app) cmdHook(ctx context.Context, args []string) error {
	fs := a.newFlagSet("hook")
	flags := config.Register(fs)
	maxChars := fs.Int("max-chars", briefing.DefaultMaxChars, "character budget of the briefing")
	focus := fs.String("focus", "", "optional topic or query to prioritize relevant memories")
	write := fs.Bool("write", false, "patch the client configuration file with this hook instead of printing the briefing")
	cfgPath := fs.String("config", "", "override target config file path (with --write)")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	client := strings.ToLower(pos[0])
	if !isHookClient(client) {
		return unknownHookClient(a.stderr, pos[0])
	}
	if *write {
		cfg, err := flags.Resolve(a.env)
		if err != nil {
			return err
		}
		var extra []string
		if isSet(fs, "max-chars") {
			extra = append(extra, "--max-chars", strconv.Itoa(*maxChars))
		}
		if isSet(fs, "focus") {
			extra = append(extra, "--focus", *focus)
		}
		return a.setupHooks(client, executablePath(), cfg.DB, hookNamespace(fs, a.env), true, *cfgPath, extra...)
	}
	if isSet(fs, "config") {
		fmt.Fprintln(a.stderr, "--config requires --write")
		return errUsage
	}

	payload := readHookPayload(a.stdin)
	env := a.env
	if dir := payload.dir(); dir != "" {
		env.Getwd = func() (string, error) { return dir, nil }
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	text, err := hookBriefing(ctx, flags, env, *maxChars, *focus)
	if err != nil {
		fmt.Fprintln(a.stderr, "llm-memory hook:", err)
		return nil
	}
	return writeJSON(a.stdout, hookOutput(client, hookPreamble+text))
}

func hookBriefing(ctx context.Context, flags *config.Flags, env config.Env, maxChars int, focus string) (string, error) {
	cfg, err := flags.Resolve(env)
	if err != nil {
		return "", err
	}
	st, err := store.Open(ctx, cfg.DB)
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()
	return briefing.Build(ctx, st, briefing.Options{Namespace: cfg.Namespace, MaxChars: maxChars, Focus: focus})
}

// hookOutput builds the stdout JSON for each client.
func hookOutput(client, text string) any {
	if client == "antigravity" {
		// Shape of the SessionStart example in the Antigravity hooks docs.
		return map[string]any{
			"injectSteps": []any{
				map[string]any{"systemMessage": map[string]any{"systemMessage": text}},
			},
		}
	}
	// Claude Code and Codex share the hookSpecificOutput format.
	return map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     "SessionStart",
			"additionalContext": text,
		},
	}
}

// ---- setup --hooks ----------------------------------------------------------

func hookConfigPath(client string, env config.Env) (string, error) {
	home, err := env.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	switch client {
	case "antigravity":
		return filepath.Join(home, ".gemini", "config", "hooks.json"), nil
	case "claude-code":
		return filepath.Join(home, ".claude", "settings.json"), nil
	case "codex":
		return filepath.Join(home, ".codex", "hooks.json"), nil
	default:
		return "", fmt.Errorf("unknown client %q", client)
	}
}

// shellQuote quotes s for the shell the clients use to run hook commands.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"\\$`&|;<>()*?[]{}#~!%") {
		return s
	}
	if runtime.GOOS == "windows" {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hookCommand is the command line written into the client's hook config.
// extra holds additional flag/value pairs passed through to `llm-memory hook`.
func hookCommand(bin, client, db, namespace string, extra ...string) string {
	parts := []string{shellQuote(bin), "hook", client, "--db", shellQuote(db), "--namespace", shellQuote(namespace)}
	for _, e := range extra {
		parts = append(parts, shellQuote(e))
	}
	return strings.Join(parts, " ")
}

// hookNamespace returns the namespace written into the hook command. "auto"
// is kept unresolved: the hook detects the namespace from the session's
// directory, not from where the configuration command runs.
func hookNamespace(fs *flag.FlagSet, env config.Env) string {
	ns := strings.TrimSpace(env.Getenv(config.EnvNamespace))
	if isSet(fs, "namespace") {
		ns = fs.Lookup("namespace").Value.String()
	}
	if ns == "" {
		ns = config.AutoNamespace
	}
	return ns
}

// executablePath is the absolute path of the running binary, written into
// client configs so the hook does not depend on PATH.
func executablePath() string {
	bin, err := os.Executable()
	if err != nil || bin == "" {
		return "llm-memory"
	}
	return bin
}

// isOurHook recognizes a handler previously written by setup --hooks.
func isOurHook(cmd string) bool {
	return strings.Contains(cmd, "llm-memory") && strings.Contains(cmd, " hook ")
}

// hookHandler is one command handler in the client's config.
func hookHandler(cmd string) map[string]any {
	return map[string]any{"type": "command", "command": cmd, "timeout": hookTimeout}
}

// applySessionStartHook adds (or replaces) our SessionStart handler in a
// decoded hook config. Claude Code and Codex use
// {"hooks":{"SessionStart":[{"matcher":…,"hooks":[handler…]}]}};
// Antigravity uses {"<name>":{"SessionStart":[handler…]}}.
func applySessionStartHook(root map[string]any, client, cmd string) error {
	if client == "antigravity" {
		root["llm-memory"] = map[string]any{"SessionStart": []any{hookHandler(cmd)}}
		return nil
	}
	hooks := map[string]any{}
	if raw, ok := root["hooks"]; ok && raw != nil {
		m, ok := raw.(map[string]any)
		if !ok {
			return errors.New(`"hooks" is not a JSON object`)
		}
		hooks = m
	}
	var groups []any
	if raw, ok := hooks["SessionStart"]; ok && raw != nil {
		g, ok := raw.([]any)
		if !ok {
			return errors.New(`"hooks.SessionStart" is not a JSON array`)
		}
		groups = g
	}
	kept := make([]any, 0, len(groups)+1)
	for _, g := range groups {
		if gm, ok := g.(map[string]any); ok {
			if hs, ok := gm["hooks"].([]any); ok {
				var rest []any
				for _, h := range hs {
					if hm, ok := h.(map[string]any); ok {
						if c, _ := hm["command"].(string); isOurHook(c) {
							continue
						}
					}
					rest = append(rest, h)
				}
				if len(rest) == 0 {
					continue // the group only held our previous handler
				}
				gm["hooks"] = rest
			}
		}
		kept = append(kept, g)
	}
	// No matcher: run on startup, resume, clear and compact alike, so the
	// briefing is back in context after a compaction too.
	kept = append(kept, map[string]any{"hooks": []any{hookHandler(cmd)}})
	hooks["SessionStart"] = kept
	root["hooks"] = hooks
	return nil
}

// patchJSONFile decodes path (missing or empty = {}), applies fn and writes
// it back, keeping every other key.
func patchJSONFile(path string, fn func(map[string]any) error) error {
	root := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if err := fn(root); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	out = append(out, '\n')
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.WriteFile(path, out, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// setupHooks prints or writes the SessionStart hook configuration.
func (a app) setupHooks(client, bin, db, namespace string, write bool, cfgPath string, extra ...string) error {
	if !isHookClient(client) {
		return unknownHookClient(a.stderr, client)
	}
	cmd := hookCommand(bin, client, db, namespace, extra...)
	target := cfgPath
	if target == "" {
		var err error
		if target, err = hookConfigPath(client, a.env); err != nil {
			return err
		}
	}
	if write {
		if err := patchJSONFile(target, func(root map[string]any) error {
			return applySessionStartHook(root, client, cmd)
		}); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Updated %s (%s SessionStart hook)\n", target, client)
	} else {
		fmt.Fprintf(a.stderr, "# Add to %s (or rerun with --write):\n", target)
		root := map[string]any{}
		if err := applySessionStartHook(root, client, cmd); err != nil {
			return err
		}
		if err := writeJSON(a.stdout, root); err != nil {
			return err
		}
	}
	if client == "codex" {
		fmt.Fprintln(a.stderr, "# Codex: review and trust the new hook with /hooks before it runs.")
	}
	return nil
}
