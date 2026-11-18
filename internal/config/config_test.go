package config

import (
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func fakeEnv(vars map[string]string) Env {
	return Env{
		Getenv:      func(k string) string { return vars[k] },
		UserHomeDir: func() (string, error) { return "/home/test", nil },
	}
}

func resolve(t *testing.T, args []string, vars map[string]string) (Config, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := Register(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return f.Resolve(fakeEnv(vars))
}

func TestResolvePrecedence(t *testing.T) {
	env := map[string]string{
		EnvDB:        "/env/mem.db",
		EnvNamespace: "env-ns",
		EnvSource:    "env-src",
		EnvLogLevel:  "warn",
	}
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want Config
	}{
		{
			name: "defaults",
			want: Config{
				DB:        filepath.Join("/home/test", ".llm-memory", "memory.db"),
				Namespace: "global",
				Source:    "",
				LogLevel:  slog.LevelInfo,
			},
		},
		{
			name: "env over defaults",
			env:  env,
			want: Config{DB: "/env/mem.db", Namespace: "env-ns", Source: "env-src", LogLevel: slog.LevelWarn},
		},
		{
			name: "flags over env",
			args: []string{"--db", "/flag/mem.db", "--namespace", "flag-ns", "--source", "flag-src", "--log-level", "debug"},
			env:  env,
			want: Config{DB: "/flag/mem.db", Namespace: "flag-ns", Source: "flag-src", LogLevel: slog.LevelDebug},
		},
		{
			name: "partial flags",
			args: []string{"--source", "claude-code"},
			env:  map[string]string{EnvNamespace: "demo"},
			want: Config{
				DB:        filepath.Join("/home/test", ".llm-memory", "memory.db"),
				Namespace: "demo",
				Source:    "claude-code",
				LogLevel:  slog.LevelInfo,
			},
		},
		{
			name: "tilde expanded in flag",
			args: []string{"--db", "~/x/y.db"},
			want: Config{DB: filepath.Join("/home/test", "x", "y.db"), Namespace: "global", LogLevel: slog.LevelInfo},
		},
		{
			name: "tilde not expanded mid-path",
			args: []string{"--db", "/a/~/b.db"},
			want: Config{DB: "/a/~/b.db", Namespace: "global", LogLevel: slog.LevelInfo},
		},
		{
			name: "blank env ignored",
			env:  map[string]string{EnvNamespace: "   "},
			want: Config{
				DB:        filepath.Join("/home/test", ".llm-memory", "memory.db"),
				Namespace: "global",
				LogLevel:  slog.LevelInfo,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolve(t, tt.args, tt.env)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestResolveErrors(t *testing.T) {
	if _, err := resolve(t, []string{"--log-level", "loud"}, nil); err == nil {
		t.Error("expected error for invalid log level")
	}
	if _, err := resolve(t, []string{"--namespace", ""}, nil); err == nil {
		t.Error("expected error for explicitly empty namespace")
	}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	f := Register(fs)
	_ = fs.Parse(nil)
	_, err := f.Resolve(Env{
		Getenv:      func(string) string { return "" },
		UserHomeDir: func() (string, error) { return "", errors.New("no home") },
	})
	if err == nil {
		t.Error("expected error when home directory is unknown")
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"debug": slog.LevelDebug, "INFO": slog.LevelInfo, "": slog.LevelInfo,
		"warn": slog.LevelWarn, "warning": slog.LevelWarn, "error": slog.LevelError,
	} {
		got, err := ParseLevel(in)
		if err != nil || got != want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
}

func TestResolveEmbed(t *testing.T) {
	// Disabled by default.
	cfg, err := resolve(t, nil, nil)
	if err != nil || cfg.Embed.Enabled() || cfg.Embed != (Embed{}) {
		t.Fatalf("default embed = %+v, %v", cfg.Embed, err)
	}
	// Env, with API key from env only.
	cfg, err = resolve(t, nil, map[string]string{
		EnvEmbedURL: "http://localhost:11434/v1/embeddings", EnvEmbedModel: "nomic-embed-text",
		EnvEmbedAPIKey: " secret ", EnvEmbedMinScore: "0.42", EnvEmbedTimeout: "5s",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Embed{URL: "http://localhost:11434/v1/embeddings", Model: "nomic-embed-text", APIKey: "secret", MinScore: 0.42, Timeout: 5 * time.Second}
	if cfg.Embed != want || !cfg.Embed.Enabled() {
		t.Errorf("env embed = %+v", cfg.Embed)
	}
	// Flags over env.
	cfg, err = resolve(t, []string{"--embed-url", "https://api.example/v1/embeddings", "--embed-model", "m2", "--embed-min-score", "-1"},
		map[string]string{EnvEmbedURL: "http://x", EnvEmbedModel: "m1"})
	if err != nil || cfg.Embed.URL != "https://api.example/v1/embeddings" || cfg.Embed.Model != "m2" || cfg.Embed.MinScore != -1 {
		t.Errorf("flag embed = %+v, %v", cfg.Embed, err)
	}
	// Errors.
	for _, args := range [][]string{
		{"--embed-url", "http://x"},                                  // model missing
		{"--embed-model", "m"},                                       // URL missing
		{"--embed-provider", "http", "--embed-url", "http://x"},      // http requires model
		{"--embed-provider", "model2vec", "--embed-url", "http://x"}, // cannot combine
		{"--embed-provider", "bogus"},                                // unknown provider
		{"--embed-url", "http://x", "--embed-model", "m", "--embed-min-score", "2"},
		{"--embed-url", "http://x", "--embed-model", "m", "--embed-min-score", "abc"},
		{"--embed-url", "http://x", "--embed-model", "m", "--embed-timeout", "soon"},
		{"--embed-url", "http://x", "--embed-model", "m", "--embed-timeout", "-1s"},
	} {
		if _, err := resolve(t, args, nil); err == nil {
			t.Errorf("%v: expected error", args)
		}
	}

	// Model2Vec defaults.
	cfg, err = resolve(t, []string{"--embed-provider", "model2vec"}, nil)
	if err != nil {
		t.Fatalf("model2vec defaults: %v", err)
	}
	if !cfg.Embed.Enabled() || cfg.Embed.Provider != EmbedProviderModel2Vec || cfg.Embed.Model != DefaultModel2VecModel || cfg.Embed.CacheDir != "/home/test/.llm-memory/models" {
		t.Errorf("model2vec defaults = %+v", cfg.Embed)
	}

	// Model2Vec env + custom cache dir + model.
	cfg, err = resolve(t, nil, map[string]string{
		EnvEmbedProvider: "MODEL2VEC",
		EnvEmbedModel:    "minishlab/potion-multilingual-128M",
		EnvEmbedCacheDir: "~/custom-models",
	})
	if err != nil {
		t.Fatalf("model2vec env: %v", err)
	}
	if cfg.Embed.Provider != EmbedProviderModel2Vec || cfg.Embed.Model != "minishlab/potion-multilingual-128M" || cfg.Embed.CacheDir != "/home/test/custom-models" {
		t.Errorf("model2vec env = %+v", cfg.Embed)
	}
}

func TestDetectNamespace(t *testing.T) {
	root := t.TempDir()

	// 1. Plain directory without markers -> global.
	plain := filepath.Join(root, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := DetectNamespace(plain); got != DefaultNamespace {
		t.Errorf("plain = %q, want %q", got, DefaultNamespace)
	}

	// 2. Git repo fallback -> normalized repo directory name.
	repo := filepath.Join(root, "My_Cool.Repo")
	sub := filepath.Join(repo, "pkg", "sub")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := DetectNamespace(sub); got != "my-cool-repo" {
		t.Errorf("git repo = %q, want my-cool-repo", got)
	}

	// 3. .llm-memory.yml overrides .git.
	if err := os.WriteFile(filepath.Join(repo, ".llm-memory.yml"), []byte("# comment\nnamespace: \"Custom App\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DetectNamespace(sub); got != "custom-app" {
		t.Errorf(".llm-memory.yml = %q, want custom-app", got)
	}

	// 4. .llm-memory takes priority over .llm-memory.yml.
	if err := os.WriteFile(filepath.Join(sub, ".llm-memory"), []byte("# header\nSub Service\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DetectNamespace(sub); got != "sub-service" {
		t.Errorf(".llm-memory = %q, want sub-service", got)
	}

	// 5. Resolve with --namespace auto.
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := Register(fs)
	if err := fs.Parse([]string{"--namespace", "auto"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := f.Resolve(Env{
		Getenv:      func(string) string { return "" },
		UserHomeDir: func() (string, error) { return "/home/test", nil },
		Getwd:       func() (string, error) { return sub, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Namespace != "sub-service" {
		t.Errorf("Resolve(--namespace auto) = %q, want sub-service", cfg.Namespace)
	}
}
