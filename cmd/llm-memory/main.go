// Command llm-memory is a local MCP server storing a memory shared by
// several LLM clients in a SQLite file, plus a small admin CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Morditux/llm-memory/internal/bootstrap"
	"github.com/Morditux/llm-memory/internal/briefing"
	"github.com/Morditux/llm-memory/internal/config"
	"github.com/Morditux/llm-memory/internal/server"
	"github.com/Morditux/llm-memory/internal/store"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

// cliSource is the author recorded for writes made from the CLI.
const cliSource = "cli"

const usage = `llm-memory — shared memory for LLM clients (MCP server over stdio)

Usage:
  llm-memory [serve] [--listen ADDR] [--stateless] [--read-only] [flags]
                                             start the MCP server (stdio by default, or Streamable HTTP with --listen)
  llm-memory context [--namespace X] [--max-chars N] [--focus Q] [--json]
                                             print the briefing get_context would return
  llm-memory remember --kind K [--title T] [--tag T[,T]] [--importance N] [--supersedes ID[,ID]] [--expires-in D] [--json] <content|->
                                             create a memory from the command line
  llm-memory update [--kind K] [--title T] [--content C] [--tag T[,T]] [--importance N] [--namespace X] [--expires-in D] [--clear-expires] [--add-link K:ID] [--remove-link K:ID] [--json] <id> [content|-]
                                             modify an active memory (previous version stays in history)
  llm-memory handoff [--next S[,S]] [--questions Q[,Q]] [--supersedes ID[,ID]] [--description D] [--json] <summary|->
                                             write a handoff for the next assistant
  llm-memory forget [--reason R] [--json] <id>...
                                             archive one or more memories
  llm-memory list [--namespace X] [--kind K[,K]] [--tag T[,T]] [--archived] [--limit N] [--json]
                                             list memories, most recent first
  llm-memory show [--json] <id>              full content and history of a memory
  llm-memory search [--namespace X] [--kind K] [--tag T[,T]] [--archived] [--limit N] [--json] <query>
                                             full-text search (same engine as recall)
  llm-memory restore <id>                    un-archive a memory
  llm-memory purge --archived-before DATE [--yes]
                                             delete for good memories archived before DATE
  llm-memory export [--namespace X] [--history] [-o FILE]
                                             JSON export (stdout by default)
  llm-memory import [--dry-run] <FILE|->     import a JSON export; exact duplicates are skipped
  llm-memory stats [--json]                  counts by namespace, kind and source; file size
  llm-memory reindex [--all]                 compute missing embeddings (semantic search only)
  llm-memory namespace <list|rename|merge|archive> ...
                                             manage namespaces
  llm-memory gc                              archive expired (TTL) memories
  llm-memory doctor [--json]                 check database integrity, permissions and embeddings
  llm-memory backup [--keep N] [--verify] [--json] [<dest>]
                                             hot SQLite backup via VACUUM INTO
  llm-memory setup [--ollama] [--write] [--remove] [--config PATH] <client>
                                             print, write or remove MCP configuration for a client
  llm-memory setup --hooks [--events LIST] [--write] [--remove] [--config PATH] <antigravity|claude-code|codex>
                                             print, write or remove the client hooks (SessionStart, PreCompact, Stop)
  llm-memory hook [--event E] [--namespace X] [--max-chars N] [--focus Q] <antigravity|claude-code|codex>
                                             hook entry point (reads the client payload on stdin)
  llm-memory hook --write [--events LIST] [--config PATH] [--namespace X] [--max-chars N] [--focus Q] <client>
                                             install hook(s) in the client's configuration file
  llm-memory hook --remove [--config PATH] <client>
                                             remove llm-memory hooks from the client's configuration file
  llm-memory ui [--addr 127.0.0.1:8765]      start local web explorer
  llm-memory completion <bash|zsh|fish>      print shell completion script
  llm-memory version                         version and schema version

Common flags (all commands):
  --db PATH          SQLite file (env LLM_MEMORY_DB, default ~/.llm-memory/memory.db)
  --namespace NAME   default namespace, or "auto" (env LLM_MEMORY_NAMESPACE, default global)
  --source NAME      client name recorded as author (env LLM_MEMORY_SOURCE)
  --log-level LEVEL  debug|info|warn|error (env LLM_MEMORY_LOG_LEVEL, default info)

Optional semantic search (disabled unless both are set; used by serve, search, reindex):
  --embed-url URL          OpenAI-compatible endpoint, e.g. http://localhost:11434/v1/embeddings
                           (env LLM_MEMORY_EMBED_URL; API key: env LLM_MEMORY_EMBED_API_KEY only)
  --embed-model NAME       embeddings model (env LLM_MEMORY_EMBED_MODEL)
  --embed-min-score X      minimum cosine similarity, default 0.3 (env LLM_MEMORY_EMBED_MIN_SCORE)
  --embed-timeout D        request timeout, default 30s (env LLM_MEMORY_EMBED_TIMEOUT)

Flags may appear before or after positional arguments.
`

var (
	// errUsage signals a command-line error already reported to the user.
	errUsage = errors.New("usage error")
	// errHelp signals that help was printed; the command must not run.
	errHelp = errors.New("help requested")
)

// app carries the process I/O, so commands are testable.
type app struct {
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	env    config.Env
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], app{
		stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
		env: config.Env{Getenv: os.Getenv, UserHomeDir: os.UserHomeDir},
	})
	stop()
	switch {
	case err == nil, errors.Is(err, errHelp):
	case errors.Is(err, errUsage):
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "llm-memory:", err)
		os.Exit(1)
	}
}

// run dispatches the sub-command. The serve path writes the MCP protocol on
// stdout and nothing else.
func run(ctx context.Context, args []string, a app) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	commands := map[string]func(context.Context, []string) error{
		"serve":      a.cmdServe,
		"context":    a.cmdContext,
		"hook":       a.cmdHook,
		"remember":   a.cmdRemember,
		"update":     a.cmdUpdate,
		"forget":     a.cmdForget,
		"handoff":    a.cmdHandoff,
		"list":       a.cmdList,
		"show":       a.cmdShow,
		"search":     a.cmdSearch,
		"restore":    a.cmdRestore,
		"purge":      a.cmdPurge,
		"export":     a.cmdExport,
		"import":     a.cmdImport,
		"stats":      a.cmdStats,
		"reindex":    a.cmdReindex,
		"namespace":  a.cmdNamespace,
		"gc":         a.cmdGC,
		"doctor":     a.cmdDoctor,
		"backup":     a.cmdBackup,
		"setup":      a.cmdSetup,
		"ui":         a.cmdUI,
		"completion": a.cmdCompletion,
		"version":    a.cmdVersion,
	}
	if cmd == "help" {
		fmt.Fprint(a.stdout, usage)
		return nil
	}
	f, ok := commands[cmd]
	if !ok {
		fmt.Fprintf(a.stderr, "unknown command %q\n\n%s", cmd, usage)
		return errUsage
	}
	return f(ctx, args)
}

func (a app) newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	fs.Usage = func() {
		fmt.Fprintf(a.stderr, "Usage of llm-memory %s:\n", name)
		fs.PrintDefaults()
	}
	return fs
}

// parse parses flags interspersed with positional arguments and checks the
// number of positional arguments (min..max, max < 0 = unlimited).
func parse(fs *flag.FlagSet, args []string, minPos, maxPos int) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, errHelp
			}
			return nil, errUsage
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		if args[0] == "--" {
			pos = append(pos, args[1:]...)
			break
		}
		pos, args = append(pos, args[0]), args[1:]
	}
	if len(pos) < minPos || (maxPos >= 0 && len(pos) > maxPos) {
		switch {
		case maxPos == 0:
			fmt.Fprintf(fs.Output(), "unexpected arguments: %v\n", pos)
		case minPos == maxPos:
			fmt.Fprintf(fs.Output(), "%s expects %d argument(s), got %d\n", fs.Name(), minPos, len(pos))
		default:
			fmt.Fprintf(fs.Output(), "%s expects at least %d argument(s)\n", fs.Name(), minPos)
		}
		return nil, errUsage
	}
	return pos, nil
}

// isSet reports whether a flag was given explicitly on the command line.
func isSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// openStore resolves the common flags and opens the database.
func (a app) openStore(ctx context.Context, flags *config.Flags) (*store.Store, config.Config, error) {
	return bootstrap.OpenStore(ctx, flags, a.env)
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func (a app) cmdServe(ctx context.Context, args []string) error {
	fs := a.newFlagSet("serve")
	flags := config.Register(fs)
	listen := fs.String("listen", "", "serve Streamable HTTP MCP on this address instead of stdio (e.g. 127.0.0.1:8766)")
	stateless := fs.Bool("stateless", false, "disable MCP session tracking in Streamable HTTP mode")
	readOnly := fs.Bool("read-only", false, "expose only read-only tools (get_context, recall, get_memory, list_namespaces)")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	st, cfg, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	logger := slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	engine, err := bootstrap.NewEngine(st, cfg, logger)
	if err != nil {
		return err
	}
	opts := server.Options{
		Version:          version,
		DefaultNamespace: cfg.Namespace,
		Source:           cfg.Source,
		Logger:           logger,
		ReadOnly:         *readOnly,
	}
	if engine != nil {
		opts.Searcher = engine
		logger.Info("semantic search enabled", "url", cfg.Embed.URL, "settings", engine.Describe())
	}
	if strings.TrimSpace(*listen) != "" {
		handler, err := server.NewHTTPHandler(st, opts, &mcp.StreamableHTTPOptions{
			Stateless: *stateless,
			Logger:    logger,
		})
		if err != nil {
			return err
		}
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			return fmt.Errorf("listen %s: %w", *listen, err)
		}
		if !isLoopbackListenAddr(ln.Addr().String()) {
			logger.Warn("Streamable HTTP MCP server has no authentication; bind to loopback (127.0.0.1) or place behind an authenticating reverse proxy", "addr", ln.Addr().String())
		}
		httpSrv := &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			<-ctx.Done()
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = httpSrv.Shutdown(shutCtx)
		}()
		logger.Info("starting", "transport", "streamable-http", "addr", ln.Addr().String(),
			"stateless", *stateless, "read_only", *readOnly, "version", version,
			"db", st.Path(), "namespace", cfg.Namespace, "source", cfg.Source)
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve http: %w", err)
		}
		logger.Info("stopped")
		return nil
	}

	srv, err := server.New(st, opts)
	if err != nil {
		return err
	}
	var transport mcp.Transport = &mcp.StdioTransport{}
	if a.stdin != os.Stdin { // tests drive serve through pipes
		transport = &mcp.IOTransport{Reader: io.NopCloser(a.stdin), Writer: nopWriteCloser{a.stdout}}
	}
	logger.Info("starting", "transport", "stdio", "read_only", *readOnly, "version", version, "db", st.Path(), "namespace", cfg.Namespace, "source", cfg.Source)
	err = srv.Run(ctx, transport)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) {
		return fmt.Errorf("serve: %w", err)
	}
	logger.Info("stopped")
	return nil
}

func (a app) cmdVersion(_ context.Context, args []string) error {
	fs := a.newFlagSet("version")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	fmt.Fprintf(a.stdout, "llm-memory %s (schema %d, %s, %s/%s)\n",
		version, store.LatestSchemaVersion(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return nil
}

func (a app) cmdContext(ctx context.Context, args []string) error {
	fs := a.newFlagSet("context")
	flags := config.Register(fs)
	maxChars := fs.Int("max-chars", briefing.DefaultMaxChars, "character budget of the briefing")
	focus := fs.String("focus", "", "optional topic or query to prioritize relevant memories")
	asJSON := fs.Bool("json", false, "output as JSON")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	st, cfg, err := a.openStore(ctx, flags)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	var searcher briefing.Searcher = st
	if cfg.Embed.Enabled() {
		logger := slog.New(slog.NewTextHandler(a.stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
		if eng, err := bootstrap.NewEngine(st, cfg, logger); err != nil {
			return err
		} else if eng != nil {
			searcher = eng
		}
	}
	text, err := briefing.Build(ctx, st, briefing.Options{Namespace: cfg.Namespace, MaxChars: *maxChars, Focus: *focus, Searcher: searcher})
	if err != nil {
		return err
	}
	if *asJSON {
		ns, _ := store.NormalizeNamespace(cfg.Namespace)
		return writeJSON(a.stdout, struct {
			Namespace string `json:"namespace"`
			Briefing  string `json:"briefing"`
		}{Namespace: ns, Briefing: text})
	}
	fmt.Fprint(a.stdout, text)
	return nil
}

func isLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
