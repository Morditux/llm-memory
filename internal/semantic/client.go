// Package semantic adds optional meaning-based search on top of the FTS5
// engine: embeddings are computed by an external API (OpenAI-compatible
// /v1/embeddings: OpenAI, Ollama, LM Studio, vLLM…), stored by
// internal/store, and fused with full-text ranks (Reciprocal Rank Fusion).
// It never touches SQL directly and does not depend on the MCP SDK.
package semantic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Defaults.
const (
	DefaultTimeout   = 30 * time.Second
	DefaultBatchSize = 32
	maxErrorBody     = 512
)

// Embedder turns texts into vectors.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
}

// ClientConfig configures the HTTP client.
type ClientConfig struct {
	URL       string // full endpoint, e.g. http://localhost:11434/v1/embeddings
	Model     string
	APIKey    string        // sent as "Authorization: Bearer"; optional
	Timeout   time.Duration // per request; default 30s
	BatchSize int           // texts per request; default 32
	HTTP      *http.Client  // optional, for tests
}

// Client calls an OpenAI-compatible embeddings endpoint.
type Client struct {
	cfg  ClientConfig
	http *http.Client
}

// NewClient validates cfg.
func NewClient(cfg ClientConfig) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("embeddings URL %q must be an absolute http(s) URL, e.g. http://localhost:11434/v1/embeddings", cfg.URL)
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, errors.New("embeddings model must not be empty")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = DefaultBatchSize
	}
	h := cfg.HTTP
	if h == nil {
		h = &http.Client{Timeout: cfg.Timeout}
	}
	return &Client{cfg: cfg, http: h}, nil
}

// Model returns the configured model name.
func (c *Client) Model() string { return c.cfg.Model }

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

// Embed returns one vector per text, in order, batching requests.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += c.cfg.BatchSize {
		end := min(start+c.cfg.BatchSize, len(texts))
		vecs, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: c.cfg.Model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("encode embeddings request: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("embeddings request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embeddings API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return nil, fmt.Errorf("embeddings API: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var er embedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("embeddings API: invalid response: %w", err)
	}
	if len(er.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings API: %d vectors for %d texts", len(er.Data), len(texts))
	}
	out := make([][]float32, len(texts))
	for _, d := range er.Data {
		if d.Index < 0 || d.Index >= len(texts) || out[d.Index] != nil {
			return nil, fmt.Errorf("embeddings API: invalid or duplicate index %d", d.Index)
		}
		if len(d.Embedding) == 0 {
			return nil, fmt.Errorf("embeddings API: empty vector at index %d", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}
