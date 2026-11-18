// Package config resolves runtime parameters with the precedence
// flag > environment variable > default.
package config

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Environment variable names.
const (
	EnvDB        = "LLM_MEMORY_DB"
	EnvNamespace = "LLM_MEMORY_NAMESPACE"
	EnvSource    = "LLM_MEMORY_SOURCE"
	EnvLogLevel  = "LLM_MEMORY_LOG_LEVEL"

	// Semantic search (optional, phase 6). The API key is read from the
	// environment only, never from a flag (flags are visible in ps).
	EnvEmbedProvider = "LLM_MEMORY_EMBED_PROVIDER"
	EnvEmbedURL      = "LLM_MEMORY_EMBED_URL"
	EnvEmbedModel    = "LLM_MEMORY_EMBED_MODEL"
	EnvEmbedAPIKey   = "LLM_MEMORY_EMBED_API_KEY"
	EnvEmbedCacheDir = "LLM_MEMORY_EMBED_CACHE_DIR"
	EnvEmbedMinScore = "LLM_MEMORY_EMBED_MIN_SCORE"
	EnvEmbedTimeout  = "LLM_MEMORY_EMBED_TIMEOUT"
)

// Defaults.
const (
	DefaultNamespace       = "global"
	AutoNamespace          = "auto"
	DefaultLogLevel        = "info"
	defaultDBRel           = ".llm-memory/memory.db"
	EmbedProviderHTTP      = "http"
	EmbedProviderModel2Vec = "model2vec"
	DefaultModel2VecModel  = "minishlab/potion-base-8M"
	defaultModelCacheRel   = ".llm-memory/models"
)

// Config holds the resolved parameters.
type Config struct {
	// DB is the absolute (or caller-relative) path of the SQLite file, with ~ expanded.
	DB string
	// Namespace is the default namespace used when a tool call omits it.
	Namespace string
	// Source is the client name forced on the command line or environment.
	// Empty means "use clientInfo.name from the MCP session".
	Source string
	// LogLevel is the slog level for stderr logs.
	LogLevel slog.Level
	// Embed configures optional semantic search; disabled when neither URL nor Model2Vec provider is set.
	Embed Embed
}

// Embed configures the embeddings backend used by semantic search.
type Embed struct {
	Provider string // "", "http", or "model2vec"
	URL      string
	Model    string
	APIKey   string
	CacheDir string        // local cache directory for model2vec models
	MinScore float64       // 0 = engine default
	Timeout  time.Duration // 0 = client default
}

// Enabled reports whether semantic search is configured.
func (e Embed) Enabled() bool { return e.URL != "" || e.Provider == EmbedProviderModel2Vec }

// Flags holds the raw flag values registered on a FlagSet.
type Flags struct {
	fs            *flag.FlagSet
	db            string
	namespace     string
	source        string
	logLevel      string
	embedProvider string
	embedURL      string
	embedModel    string
	embedCacheDir string
	embedMin      string
	embedTO       string
}

// Register declares the common flags (--db, --namespace, --source, --log-level) on fs.
func Register(fs *flag.FlagSet) *Flags {
	f := &Flags{fs: fs}
	fs.StringVar(&f.db, "db", "", "SQLite database file (env "+EnvDB+", default ~/"+defaultDBRel+")")
	fs.StringVar(&f.namespace, "namespace", "", "default namespace (env "+EnvNamespace+", default "+DefaultNamespace+")")
	fs.StringVar(&f.source, "source", "", "client name recorded as author (env "+EnvSource+", default: MCP clientInfo.name)")
	fs.StringVar(&f.logLevel, "log-level", "", "log level: debug, info, warn, error (env "+EnvLogLevel+", default "+DefaultLogLevel+")")
	fs.StringVar(&f.embedProvider, "embed-provider", "", "embeddings backend: http or model2vec (env "+EnvEmbedProvider+")")
	fs.StringVar(&f.embedURL, "embed-url", "", "OpenAI-compatible embeddings endpoint enabling semantic search (env "+EnvEmbedURL+", default: disabled)")
	fs.StringVar(&f.embedModel, "embed-model", "", "embeddings model or Model2Vec repo/path (env "+EnvEmbedModel+"; API key: env "+EnvEmbedAPIKey+")")
	fs.StringVar(&f.embedCacheDir, "embed-cache-dir", "", "cache directory for Model2Vec models (env "+EnvEmbedCacheDir+", default ~/"+defaultModelCacheRel+")")
	fs.StringVar(&f.embedMin, "embed-min-score", "", "minimum cosine similarity of a semantic match (env "+EnvEmbedMinScore+", default 0.3)")
	fs.StringVar(&f.embedTO, "embed-timeout", "", "timeout of an embeddings request (env "+EnvEmbedTimeout+", default 30s)")
	return f
}

// Env abstracts environment lookups so tests do not touch the real process.
type Env struct {
	Getenv      func(string) string
	UserHomeDir func() (string, error)
	Getwd       func() (string, error)
}

// Resolve applies flag > env > default. It must be called after fs.Parse.
func (f *Flags) Resolve(env Env) (Config, error) {
	set := map[string]bool{}
	f.fs.Visit(func(fl *flag.Flag) { set[fl.Name] = true })

	pick := func(name, flagVal, envName, def string) string {
		if set[name] {
			return flagVal
		}
		if v := strings.TrimSpace(env.Getenv(envName)); v != "" {
			return v
		}
		return def
	}

	var cfg Config

	db := pick("db", f.db, EnvDB, "")
	if db == "" {
		db = "~/" + defaultDBRel
	}
	p, err := expandHome(db, env.UserHomeDir)
	if err != nil {
		return Config{}, err
	}
	cfg.DB = p

	cfg.Namespace = pick("namespace", f.namespace, EnvNamespace, DefaultNamespace)
	if strings.TrimSpace(cfg.Namespace) == "" {
		return Config{}, errors.New("namespace must not be empty")
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Namespace), AutoNamespace) {
		getwd := env.Getwd
		if getwd == nil {
			getwd = os.Getwd
		}
		if wd, werr := getwd(); werr == nil && wd != "" {
			cfg.Namespace = DetectNamespace(wd)
		} else {
			cfg.Namespace = DefaultNamespace
		}
	}
	cfg.Source = strings.TrimSpace(pick("source", f.source, EnvSource, ""))

	lvl, err := ParseLevel(pick("log-level", f.logLevel, EnvLogLevel, DefaultLogLevel))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = lvl

	if cfg.Embed, err = f.resolveEmbed(pick, env); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (f *Flags) resolveEmbed(pick func(name, flagVal, envName, def string) string, env Env) (Embed, error) {
	provider := strings.ToLower(strings.TrimSpace(pick("embed-provider", f.embedProvider, EnvEmbedProvider, "")))
	e := Embed{
		Provider: provider,
		URL:      strings.TrimSpace(pick("embed-url", f.embedURL, EnvEmbedURL, "")),
		Model:    strings.TrimSpace(pick("embed-model", f.embedModel, EnvEmbedModel, "")),
		APIKey:   strings.TrimSpace(env.Getenv(EnvEmbedAPIKey)),
	}
	cacheDir := strings.TrimSpace(pick("embed-cache-dir", f.embedCacheDir, EnvEmbedCacheDir, ""))

	switch provider {
	case "", EmbedProviderHTTP:
		if provider == EmbedProviderHTTP && (e.URL == "" || e.Model == "") {
			return Embed{}, errors.New("semantic search with provider \"http\" needs both --embed-url and --embed-model (or " + EnvEmbedURL + " and " + EnvEmbedModel + ")")
		}
		if (e.URL == "") != (e.Model == "") {
			return Embed{}, errors.New("semantic search needs both --embed-url and --embed-model (or " + EnvEmbedURL + " and " + EnvEmbedModel + ")")
		}
		if cacheDir != "" {
			var err error
			if e.CacheDir, err = expandHome(cacheDir, env.UserHomeDir); err != nil {
				return Embed{}, err
			}
		}
	case EmbedProviderModel2Vec:
		if e.URL != "" {
			return Embed{}, errors.New("--embed-url cannot be combined with --embed-provider model2vec")
		}
		if e.Model == "" {
			e.Model = DefaultModel2VecModel
		} else {
			var err error
			if e.Model, err = expandHome(e.Model, env.UserHomeDir); err != nil {
				return Embed{}, err
			}
		}
		if cacheDir == "" {
			cacheDir = "~/" + defaultModelCacheRel
		}
		var err error
		if e.CacheDir, err = expandHome(cacheDir, env.UserHomeDir); err != nil {
			return Embed{}, err
		}
	default:
		return Embed{}, fmt.Errorf("invalid embeddings provider %q (expected http or model2vec)", provider)
	}

	if v := pick("embed-min-score", f.embedMin, EnvEmbedMinScore, ""); v != "" {
		x, err := strconv.ParseFloat(v, 64)
		if err != nil || x < -1 || x > 1 {
			return Embed{}, fmt.Errorf("invalid embeddings min score %q (expected a number between -1 and 1)", v)
		}
		e.MinScore = x
	}
	if v := pick("embed-timeout", f.embedTO, EnvEmbedTimeout, ""); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Embed{}, fmt.Errorf("invalid embeddings timeout %q (expected a duration such as 30s)", v)
		}
		e.Timeout = d
	}
	return e, nil
}

// ParseLevel converts a textual level to slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q (expected debug, info, warn or error)", s)
	}
}

// expandHome replaces a leading "~" (alone or followed by a separator) with
// the user's home directory. MCP clients do not go through a shell.
func expandHome(p string, home func() (string, error)) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") && !strings.HasPrefix(p, `~\`) {
		return p, nil
	}
	h, err := home()
	if err != nil {
		return "", fmt.Errorf("expand ~ in %q: %w", p, err)
	}
	if p == "~" {
		return h, nil
	}
	return filepath.Join(h, p[2:]), nil
}

// DetectNamespace walks up from dir looking for a .llm-memory or .llm-memory.yml
// marker file, then a .git root whose directory name becomes the namespace.
// Falls back to DefaultNamespace when nothing matches.
func DetectNamespace(dir string) string {
	cur := filepath.Clean(dir)
	for {
		if ns := readNamespaceFile(filepath.Join(cur, ".llm-memory")); ns != "" {
			return ns
		}
		if ns := readNamespaceYAML(filepath.Join(cur, ".llm-memory.yml")); ns != "" {
			return ns
		}
		if _, err := os.Stat(filepath.Join(cur, ".git")); err == nil {
			if ns := sanitizeNamespace(filepath.Base(cur)); ns != "" {
				return ns
			}
			return DefaultNamespace
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return DefaultNamespace
}

func readNamespaceFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return sanitizeNamespace(line)
	}
	return ""
}

func readNamespaceYAML(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if after, ok := strings.CutPrefix(line, "namespace:"); ok {
			val := strings.TrimSpace(after)
			val = strings.Trim(val, `"'`)
			return sanitizeNamespace(val)
		}
	}
	return ""
}

func sanitizeNamespace(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isAlnum {
			b.WriteRune(r)
			prevDash = false
		} else if !prevDash && b.Len() > 0 {
			b.WriteByte('-')
			prevDash = true
		}
		if b.Len() >= 64 {
			break
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		return ""
	}
	return out
}
