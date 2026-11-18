// Package bootstrap resolves configuration and builds the long-lived services
// (store, semantic engine) shared by every front-end binary.
package bootstrap

import (
	"context"
	"log/slog"

	"github.com/Morditux/llm-memory/internal/config"
	"github.com/Morditux/llm-memory/internal/model2vec"
	"github.com/Morditux/llm-memory/internal/semantic"
	"github.com/Morditux/llm-memory/internal/store"
)

// OpenStore resolves the common flags against env and opens the database.
func OpenStore(ctx context.Context, flags *config.Flags, env config.Env) (*store.Store, config.Config, error) {
	cfg, err := flags.Resolve(env)
	if err != nil {
		return nil, cfg, err
	}
	st, err := store.Open(ctx, cfg.DB)
	if err != nil {
		return nil, cfg, err
	}
	return st, cfg, nil
}

// NewEmbedder returns the configured semantic Embedder, or nil when semantic
// search is disabled.
func NewEmbedder(cfg config.Config, logger *slog.Logger) (semantic.Embedder, error) {
	if !cfg.Embed.Enabled() {
		return nil, nil
	}
	if cfg.Embed.Provider == config.EmbedProviderModel2Vec {
		return model2vec.NewEmbedder(model2vec.Options{
			Model:    cfg.Embed.Model,
			CacheDir: cfg.Embed.CacheDir,
			Logger:   logger,
		})
	}
	return semantic.NewClient(semantic.ClientConfig{
		URL:     cfg.Embed.URL,
		Model:   cfg.Embed.Model,
		APIKey:  cfg.Embed.APIKey,
		Timeout: cfg.Embed.Timeout,
	})
}

// NewEngine returns the semantic search engine, or nil when it is not
// configured (full-text search only, no network access).
func NewEngine(st *store.Store, cfg config.Config, logger *slog.Logger) (*semantic.Engine, error) {
	emb, err := NewEmbedder(cfg, logger)
	if err != nil || emb == nil {
		return nil, err
	}
	return semantic.New(st, emb, semantic.Options{MinScore: cfg.Embed.MinScore, Logger: logger}), nil
}
