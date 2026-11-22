package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Morditux/llm-memory/internal/bootstrap"
	"github.com/Morditux/llm-memory/internal/briefing"
	"github.com/Morditux/llm-memory/internal/config"
	"github.com/Morditux/llm-memory/internal/store"
)

// Client hooks: the client runs `llm-memory hook <client>` on lifecycle events
// (SessionStart, PreCompact, Stop) so the memory no longer depends solely on
// the LLM remembering to call get_context or handoff.

// hookClients lists the clients whose hooks are supported.
var hookClients = []string{"antigravity", "claude-code", "codex", "opencode"}

// opencodeHookEvents lists the hook events supported by the OpenCode v2 plugin.
var opencodeHookEvents = []string{hookEventSessionStart, hookEventPreCompact}

// hookEvents lists the supported hook lifecycle events.
var hookEvents = []string{"SessionStart", "PreCompact", "Stop"}

const (
	hookEventSessionStart = "SessionStart"
	hookEventPreCompact   = "PreCompact"
	hookEventStop         = "Stop"

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

func validateHookEvents(client string, events []string) error {
	if client != "opencode" {
		return nil
	}
	for _, ev := range events {
		if !slices.Contains(opencodeHookEvents, ev) {
			return fmt.Errorf("hook event %q is not supported for opencode (expected %s)", ev, strings.Join(opencodeHookEvents, ", "))
		}
	}
	return nil
}

func resolveWd(env config.Env) (string, error) {
	if env.Getwd != nil {
		wd, err := env.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		return wd, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	return wd, nil
}

func unknownHookClient(w io.Writer, c string) error {
	fmt.Fprintf(w, "hooks are not supported for %q (expected %s)\n", c, strings.Join(hookClients, ", "))
	return errUsage
}

func normalizeHookEvent(raw string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "sessionstart", "session-start", "session_start":
		return hookEventSessionStart, nil
	case "precompact", "pre-compact", "pre_compact":
		return hookEventPreCompact, nil
	case "stop":
		return hookEventStop, nil
	default:
		return "", fmt.Errorf("unsupported hook event %q (expected %s)", raw, strings.Join(hookEvents, ", "))
	}
}

func parseHookEvents(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{hookEventSessionStart}, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		ev, err := normalizeHookEvent(part)
		if err != nil {
			return nil, err
		}
		if !seen[ev] {
			seen[ev] = true
			out = append(out, ev)
		}
	}
	if len(out) == 0 {
		return []string{hookEventSessionStart}, nil
	}
	return out, nil
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

// cmdHook prints the hook output in the format the client expects. Errors are
// reported on stderr but the exit status stays 0 during live hook execution,
// so a memory problem never prevents a session from starting.
func (a app) cmdHook(ctx context.Context, args []string) error {
	fs := a.newFlagSet("hook")
	flags := config.Register(fs)
	eventFlag := fs.String("event", hookEventSessionStart, "hook event: "+strings.Join(hookEvents, ", "))
	eventsFlag := fs.String("events", hookEventSessionStart, "comma-separated events to install with --write: "+strings.Join(hookEvents, ", "))
	maxChars := fs.Int("max-chars", briefing.DefaultMaxChars, "character budget of the briefing")
	focus := fs.String("focus", "", "optional topic or query to prioritize relevant memories")
	write := fs.Bool("write", false, "patch the client configuration file with this hook instead of printing the briefing")
	remove := fs.Bool("remove", false, "remove llm-memory hooks from the client configuration file")
	project := fs.Bool("project", false, "target project-local hook configuration (with --write or --remove)")
	cfgPath := fs.String("config", "", "override target config file path (with --write or --remove)")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	client := strings.ToLower(pos[0])
	if !isHookClient(client) {
		return unknownHookClient(a.stderr, pos[0])
	}
	if *write && *remove {
		fmt.Fprintln(a.stderr, "cannot combine --write and --remove")
		return errUsage
	}
	if *project && !*write && !*remove {
		fmt.Fprintln(a.stderr, "--project requires --write or --remove")
		return errUsage
	}
	if *remove {
		return a.removeHooks(client, *project, *cfgPath)
	}
	if *write {
		evSpec := *eventsFlag
		if isSet(fs, "event") && !isSet(fs, "events") {
			evSpec = *eventFlag
		}
		var events []string
		if client == "opencode" && !isSet(fs, "events") && !isSet(fs, "event") {
			events = append([]string(nil), opencodeHookEvents...)
		} else {
			events, err = parseHookEvents(evSpec)
			if err != nil {
				fmt.Fprintln(a.stderr, err)
				return errUsage
			}
		}
		if err := validateHookEvents(client, events); err != nil {
			fmt.Fprintln(a.stderr, err)
			return errUsage
		}
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
		return a.setupHooks(client, executablePath(), cfg.DB, hookNamespace(fs, a.env), events, true, *project, *cfgPath, extra...)
	}
	if isSet(fs, "config") {
		fmt.Fprintln(a.stderr, "--config requires --write or --remove")
		return errUsage
	}
	event, err := normalizeHookEvent(*eventFlag)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return errUsage
	}
	if err := validateHookEvents(client, []string{event}); err != nil {
		fmt.Fprintln(a.stderr, err)
		return errUsage
	}

	payload := readHookPayload(a.stdin)
	env := a.env
	if dir := payload.dir(); dir != "" {
		env.Getwd = func() (string, error) { return dir, nil }
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	text, err := hookText(ctx, flags, env, event, *maxChars, *focus)
	if err != nil {
		fmt.Fprintln(a.stderr, "llm-memory hook:", err)
		return nil
	}
	return writeJSON(a.stdout, hookOutput(client, event, text))
}

func hookText(ctx context.Context, flags *config.Flags, env config.Env, event string, maxChars int, focus string) (string, error) {
	cfg, err := flags.Resolve(env)
	if err != nil {
		return "", err
	}
	st, err := store.Open(ctx, cfg.DB)
	if err != nil {
		return "", err
	}
	defer func() { _ = st.Close() }()

	if event == hookEventSessionStart {
		var searcher briefing.Searcher = st
		if cfg.Embed.Enabled() {
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			if eng, err := bootstrap.NewEngine(st, cfg, logger); err == nil && eng != nil {
				searcher = eng
			}
		}
		text, err := briefing.Build(ctx, st, briefing.Options{
			Namespace: cfg.Namespace,
			MaxChars:  maxChars,
			Focus:     focus,
			Searcher:  searcher,
		})
		if err != nil {
			return "", err
		}
		return hookPreamble + text, nil
	}

	ns, err := store.NormalizeNamespace(cfg.Namespace)
	if err != nil {
		ns = store.DefaultNamespace
	}
	tasks, _ := st.List(ctx, store.ListFilter{Namespace: ns, Kinds: []store.Kind{store.KindTaskState}, Limit: 5})
	return hookReminder(event, ns, tasks), nil
}

func hookReminder(event, ns string, tasks []store.Memory) string {
	var b strings.Builder
	if event == hookEventPreCompact {
		fmt.Fprintf(&b, "Context compaction imminent for llm-memory namespace %q.\n", ns)
		b.WriteString("- Persist any new decisions (with reason), preferences, or stable facts using `remember`.\n")
		b.WriteString("- Update current progress with `handoff` so the post-compaction session can resume cleanly.\n")
	} else {
		fmt.Fprintf(&b, "Before ending or switching tools (llm-memory namespace %q):\n", ns)
		b.WriteString("- If you made decisions, learned stable facts, or received lasting preferences, save them with `remember`.\n")
		b.WriteString("- If a multi-step task is ongoing or finished, leave a `handoff` (passing completed task_state IDs in `supersedes`).\n")
	}
	if len(tasks) > 0 {
		b.WriteString("- Active task_state memories in this namespace:\n")
		for _, t := range tasks {
			fmt.Fprintf(&b, "  - #%d: %s\n", t.ID, summary(t))
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// hookOutput builds the stdout JSON for each client and event.
func hookOutput(client, event, text string) any {
	if client == "opencode" {
		return map[string]any{
			"event":   event,
			"context": text,
		}
	}
	if client == "antigravity" {
		// Shape of the SessionStart example in the Antigravity hooks docs.
		return map[string]any{
			"injectSteps": []any{
				map[string]any{"systemMessage": map[string]any{"systemMessage": text}},
			},
		}
	}
	if event == hookEventStop {
		return map[string]any{
			"systemMessage": text,
		}
	}
	// Claude Code and Codex share the hookSpecificOutput format for SessionStart / PreCompact.
	return map[string]any{
		"hookSpecificOutput": map[string]any{
			"hookEventName":     event,
			"additionalContext": text,
		},
	}
}

// ---- setup --hooks ----------------------------------------------------------

func hookConfigPath(client string, project bool, env config.Env) (string, error) {
	if project {
		wd, err := resolveWd(env)
		if err != nil {
			return "", err
		}
		switch client {
		case "antigravity":
			return filepath.Join(wd, ".gemini", "hooks.json"), nil
		case "claude-code":
			return filepath.Join(wd, ".claude", "settings.json"), nil
		case "codex":
			return filepath.Join(wd, ".codex", "hooks.json"), nil
		case "opencode":
			return filepath.Join(wd, ".opencode", "plugins", "llm-memory.js"), nil
		default:
			return "", fmt.Errorf("unknown client %q", client)
		}
	}
	home, err := env.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	xdgConfig := env.Getenv("XDG_CONFIG_HOME")
	if xdgConfig == "" {
		xdgConfig = filepath.Join(home, ".config")
	}
	switch client {
	case "antigravity":
		return filepath.Join(home, ".gemini", "config", "hooks.json"), nil
	case "claude-code":
		return filepath.Join(home, ".claude", "settings.json"), nil
	case "codex":
		return filepath.Join(home, ".codex", "hooks.json"), nil
	case "opencode":
		return filepath.Join(xdgConfig, "opencode", "plugins", "llm-memory.js"), nil
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
func hookCommand(bin, client, event, db, namespace string, extra ...string) string {
	parts := []string{shellQuote(bin), "hook", client}
	if event != "" && event != hookEventSessionStart {
		parts = append(parts, "--event", event)
	}
	parts = append(parts, "--db", shellQuote(db), "--namespace", shellQuote(namespace))
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

// stripOurHooks removes all llm-memory hook handlers from root.
func stripOurHooks(root map[string]any, client string) error {
	if client == "antigravity" {
		delete(root, "llm-memory")
		return nil
	}
	rawHooks, ok := root["hooks"]
	if !ok || rawHooks == nil {
		return nil
	}
	hooks, ok := rawHooks.(map[string]any)
	if !ok {
		return errors.New(`"hooks" is not a JSON object`)
	}
	for ev, rawGroups := range hooks {
		if rawGroups == nil {
			continue
		}
		groups, ok := rawGroups.([]any)
		if !ok {
			return fmt.Errorf(`"hooks.%s" is not a JSON array`, ev)
		}
		var kept []any
		hadOur := false
		for _, g := range groups {
			gm, ok := g.(map[string]any)
			if !ok {
				kept = append(kept, g)
				continue
			}
			hs, ok := gm["hooks"].([]any)
			if !ok {
				kept = append(kept, g)
				continue
			}
			var rest []any
			for _, h := range hs {
				if hm, ok := h.(map[string]any); ok {
					if c, _ := hm["command"].(string); isOurHook(c) {
						hadOur = true
						continue
					}
				}
				rest = append(rest, h)
			}
			if len(rest) == 0 && len(hs) > 0 && hadOur {
				continue // group only held our previous handler
			}
			gm["hooks"] = rest
			kept = append(kept, g)
		}
		if len(kept) == 0 && hadOur {
			delete(hooks, ev)
		} else if kept != nil {
			hooks[ev] = kept
		}
	}
	return nil
}

// applyHooks adds (or replaces) our hook handlers in a decoded hook config.
// Claude Code and Codex use {"hooks":{"<Event>":[{"matcher":…,"hooks":[handler…]}]}};
// Antigravity uses {"llm-memory":{"<Event>":[handler…]}}.
func applyHooks(root map[string]any, client string, events []string, cmdForEvent func(event string) string) error {
	if err := stripOurHooks(root, client); err != nil {
		return err
	}
	if client == "antigravity" {
		entry := map[string]any{}
		for _, ev := range events {
			entry[ev] = []any{hookHandler(cmdForEvent(ev))}
		}
		root["llm-memory"] = entry
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
	for _, ev := range events {
		var groups []any
		if raw, ok := hooks[ev]; ok && raw != nil {
			g, ok := raw.([]any)
			if !ok {
				return fmt.Errorf(`"hooks.%s" is not a JSON array`, ev)
			}
			groups = g
		}
		// No matcher: run on startup, resume, clear and compact alike.
		groups = append(groups, map[string]any{"hooks": []any{hookHandler(cmdForEvent(ev))}})
		hooks[ev] = groups
	}
	root["hooks"] = hooks
	return nil
}

// patchJSONFile decodes path (missing or empty = {}), applies fn and writes
// it back, keeping every other key.
func patchJSONFile(path string, fn func(map[string]any) error) error {
	return patchJSONFileWithWarn(path, nil, fn)
}

func patchJSONFileWithWarn(path string, warn io.Writer, fn func(map[string]any) error) error {
	root := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) > 0 {
		hasComments, err := decodeJSONC(data, &root)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		if hasComments && warn != nil {
			fmt.Fprintf(warn, "note: JSONC comments in %s were not preserved\n", path)
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

func renderOpenCodePlugin(bin, db, namespace string, events []string, extra ...string) string {
	binJSON, _ := json.Marshal(bin)
	baseArgs := []string{"hook", "opencode", "--db", db, "--namespace", namespace}
	baseArgs = append(baseArgs, extra...)
	argsJSON, _ := json.Marshal(baseArgs)

	hasSessionStart := slices.Contains(events, hookEventSessionStart)
	hasPreCompact := slices.Contains(events, hookEventPreCompact)

	var b strings.Builder
	b.WriteString("// Generated by llm-memory. Do not edit by hand; rerun `llm-memory setup --hooks --write opencode` to update.\n")
	b.WriteString("import { Plugin } from \"@opencode/plugin\"\n")
	b.WriteString("import { spawn } from \"node:child_process\"\n\n")
	fmt.Fprintf(&b, "const BIN = %s\n", binJSON)
	fmt.Fprintf(&b, "const BASE_ARGS = %s\n", argsJSON)
	b.WriteString("const TIMEOUT_MS = 15000\n")
	b.WriteString("const MAX_CACHE = 200\n\n")
	b.WriteString(`function runHook(eventName, cwd) {
  return new Promise((resolve) => {
    let stdout = ""
    let done = false
    const finish = (val) => {
      if (done) return
      done = true
      clearTimeout(timer)
      resolve(val)
    }
    const child = spawn(BIN, [...BASE_ARGS, "--event", eventName], {
      stdio: ["pipe", "pipe", "ignore"],
    })
    const timer = setTimeout(() => {
      try { child.kill() } catch {}
      finish("")
    }, TIMEOUT_MS)
    child.on("error", () => finish(""))
    child.stdout.on("data", (chunk) => { stdout += chunk })
    child.on("close", (code) => {
      if (code !== 0 || !stdout.trim()) return finish("")
      try {
        const parsed = JSON.parse(stdout)
        finish(typeof parsed.context === "string" ? parsed.context : "")
      } catch {
        finish("")
      }
    })
    try {
      child.stdin.end(JSON.stringify({ cwd: cwd || process.cwd() }))
    } catch {
      finish("")
    }
  })
}

async function sessionDir(ctx, sessionID) {
  if (sessionID && ctx.session?.get) {
    try {
      const s = await ctx.session.get({ sessionID })
      if (s?.directory) return s.directory
    } catch {}
  }
  return ctx.location?.directory || process.cwd()
}

export default Plugin.define({
  id: "llm-memory",
  async setup(ctx) {
    const briefings = new Map()
    const pending = new Map()
`)
	if hasSessionStart {
		b.WriteString(`
    await ctx.session.hook("context", async (event) => {
      const sid = event.sessionID || ""
      let text = briefings.get(sid)
      if (text === undefined) {
        let req = pending.get(sid)
        if (!req) {
          req = sessionDir(ctx, sid).then((cwd) => runHook("SessionStart", cwd))
          pending.set(sid, req)
        }
        try {
          text = await req
        } finally {
          pending.delete(sid)
        }
        if (briefings.size >= MAX_CACHE) {
          const oldest = briefings.keys().next().value
          if (oldest !== undefined) briefings.delete(oldest)
        }
        briefings.set(sid, text)
      }
      if (text) {
        event.system.push({ type: "text", text })
      }
    })
`)
	}
	if hasPreCompact {
		b.WriteString(`
    await ctx.session.hook("compaction", async (event) => {
      const sid = event.sessionID || ""
      briefings.delete(sid)
      const cwd = await sessionDir(ctx, sid)
      const reminder = await runHook("PreCompact", cwd)
      if (reminder) {
        event.system.push({ type: "text", text: reminder })
      }
    })
`)
	} else if hasSessionStart {
		b.WriteString(`
    await ctx.session.hook("compaction", (event) => {
      briefings.delete(event.sessionID || "")
    })
`)
	}
	b.WriteString("  },\n})\n")
	return b.String()
}

// setupHooks prints or writes the hook configuration.
func (a app) setupHooks(client, bin, db, namespace string, events []string, write, project bool, cfgPath string, extra ...string) error {
	if !isHookClient(client) {
		return unknownHookClient(a.stderr, client)
	}
	if len(events) == 0 {
		if client == "opencode" {
			events = append([]string(nil), opencodeHookEvents...)
		} else {
			events = []string{hookEventSessionStart}
		}
	}
	if err := validateHookEvents(client, events); err != nil {
		fmt.Fprintln(a.stderr, err)
		return errUsage
	}
	target := cfgPath
	if target == "" {
		var err error
		if target, err = hookConfigPath(client, project, a.env); err != nil {
			return err
		}
	}
	if client == "opencode" {
		js := renderOpenCodePlugin(bin, db, namespace, events, extra...)
		if write {
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create directory for %s: %w", target, err)
			}
			mode := os.FileMode(0o644)
			if fi, err := os.Stat(target); err == nil {
				mode = fi.Mode().Perm()
			}
			if err := os.WriteFile(target, []byte(js), mode); err != nil {
				return fmt.Errorf("write %s: %w", target, err)
			}
			fmt.Fprintf(a.stdout, "Updated %s (%s %s hook)\n", target, client, strings.Join(events, ","))
		} else {
			fmt.Fprintf(a.stderr, "# Write to %s (or rerun with --write):\n", target)
			_, err := fmt.Fprint(a.stdout, js)
			return err
		}
		return nil
	}
	cmdForEvent := func(ev string) string {
		return hookCommand(bin, client, ev, db, namespace, extra...)
	}
	if write {
		if err := patchJSONFile(target, func(root map[string]any) error {
			return applyHooks(root, client, events, cmdForEvent)
		}); err != nil {
			return err
		}
		fmt.Fprintf(a.stdout, "Updated %s (%s %s hook)\n", target, client, strings.Join(events, ","))
	} else {
		fmt.Fprintf(a.stderr, "# Add to %s (or rerun with --write):\n", target)
		root := map[string]any{}
		if err := applyHooks(root, client, events, cmdForEvent); err != nil {
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

func (a app) removeHooks(client string, project bool, cfgPath string) error {
	target := cfgPath
	if target == "" {
		var err error
		if target, err = hookConfigPath(client, project, a.env); err != nil {
			return err
		}
	}
	if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(a.stdout, "No hook configuration found at %s\n", target)
		return nil
	}
	if client == "opencode" {
		data, err := os.ReadFile(target)
		if err != nil {
			return fmt.Errorf("read %s: %w", target, err)
		}
		if !strings.Contains(string(data), "llm-memory") {
			return fmt.Errorf("%s does not look like an llm-memory OpenCode plugin", target)
		}
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("remove %s: %w", target, err)
		}
		fmt.Fprintf(a.stdout, "Removed llm-memory hooks from %s (%s)\n", target, client)
		return nil
	}
	if err := patchJSONFile(target, func(root map[string]any) error {
		return stripOurHooks(root, client)
	}); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "Removed llm-memory hooks from %s (%s)\n", target, client)
	return nil
}
