package model2vec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ModelConfig represents the subset of config.json used at inference time.
type ModelConfig struct {
	ModelType string `json:"model_type"`
	Normalize bool   `json:"normalize"`
	MaxLength *int   `json:"max_length"`
	HiddenDim int    `json:"hidden_dim"`
}

// StaticModel holds an in-memory Model2Vec static embedding table and tokenizer.
type StaticModel struct {
	tok       *tokenizer
	emb       []float32
	rows      int
	dim       int
	weights   []float32
	mapping   []int
	normalize bool
	maxLength int
}

// LoadDir loads a Model2Vec model from a directory containing config.json,
// tokenizer.json, and model.safetensors.
func LoadDir(dir string) (*StaticModel, error) {
	var mcfg ModelConfig
	maxLen := defaultMaxLength
	cfgBytes, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err == nil {
		if err := json.Unmarshal(cfgBytes, &mcfg); err != nil {
			return nil, fmt.Errorf("parse config.json: %w", err)
		}
		if mcfg.MaxLength != nil {
			maxLen = *mcfg.MaxLength
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read config.json: %w", err)
	}

	tok, err := loadTokenizer(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		return nil, err
	}

	st, err := loadSafetensors(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		return nil, err
	}

	if len(st.Mapping) > 0 {
		for i, row := range st.Mapping {
			if row < 0 || row >= st.Rows {
				return nil, fmt.Errorf("mapping[%d] = %d out of range [0,%d)", i, row, st.Rows)
			}
		}
	}

	return &StaticModel{
		tok:       tok,
		emb:       st.Embeddings,
		rows:      st.Rows,
		dim:       st.Dim,
		weights:   st.Weights,
		mapping:   st.Mapping,
		normalize: mcfg.Normalize,
		maxLength: maxLen,
	}, nil
}

// Dim returns the embedding dimension.
func (m *StaticModel) Dim() int { return m.dim }

// Encode converts each input string into a vector of length Dim().
func (m *StaticModel) Encode(texts []string) [][]float32 {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = m.encodeOne(text)
	}
	return out
}

func (m *StaticModel) encodeOne(text string) []float32 {
	vec := make([]float32, m.dim)
	ids := m.tok.Encode(text, m.maxLength)
	if len(ids) == 0 {
		return vec
	}

	used := 0
	for _, id := range ids {
		if id < 0 {
			continue
		}
		rowIdx := id
		if len(m.mapping) > 0 {
			if id >= len(m.mapping) {
				continue
			}
			rowIdx = m.mapping[id]
		}
		if rowIdx < 0 || rowIdx >= m.rows {
			continue
		}

		w := float32(1)
		if len(m.weights) > 0 {
			if id >= len(m.weights) {
				continue
			}
			w = m.weights[id]
		}

		base := rowIdx * m.dim
		row := m.emb[base : base+m.dim]
		for d := range m.dim {
			vec[d] += row[d] * w
		}
		used++
	}
	if used == 0 {
		return vec
	}

	inv := float32(1) / float32(used)
	for d := range m.dim {
		vec[d] *= inv
	}

	if m.normalize {
		var sumSq float64
		for _, v := range vec {
			f := float64(v)
			sumSq += f * f
		}
		norm := math.Sqrt(sumSq) + 1e-32
		invNorm := float32(1.0 / norm)
		for d := range m.dim {
			vec[d] *= invNorm
		}
	}
	return vec
}

// Options configures a lazy-loading Model2Vec Embedder.
type Options struct {
	Model    string       // HuggingFace repo ID ("minishlab/potion-base-8M") or local directory path
	CacheDir string       // cache root for downloaded models
	BaseURL  string       // optional HuggingFace base URL (default https://huggingface.co)
	HTTP     *http.Client // optional HTTP client for downloads
	Logger   *slog.Logger // nil = discard
}

// Embedder implements semantic.Embedder using a local or cached Model2Vec model.
// Model files are loaded lazily on the first Embed call so MCP server startup
// stays instantaneous.
type Embedder struct {
	opts Options
	log  *slog.Logger

	mu    sync.Mutex
	model *StaticModel
	dir   string
}

// NewEmbedder validates opts and returns an Embedder.
func NewEmbedder(opts Options) (*Embedder, error) {
	opts.Model = strings.TrimSpace(opts.Model)
	if opts.Model == "" {
		return nil, errors.New("model2vec model must not be empty")
	}
	opts.CacheDir = strings.TrimSpace(opts.CacheDir)
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Embedder{opts: opts, log: log}, nil
}

// Model returns the configured model identifier (repo ID or path).
func (e *Embedder) Model() string { return e.opts.Model }

// Embed returns one vector per input text.
func (e *Embedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m, err := e.ensureLoaded(ctx)
	if err != nil {
		return nil, err
	}
	return m.Encode(texts), nil
}

// CheckLocal reports the resolved directory and whether all required model
// files already exist on disk (without triggering a download).
func (e *Embedder) CheckLocal() (dir string, ready bool, err error) {
	return resolveLocalModelDir(e.opts.Model, e.opts.CacheDir)
}

func (e *Embedder) ensureLoaded(ctx context.Context) (*StaticModel, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.model != nil {
		return e.model, nil
	}

	dir, err := ensureModelFiles(ctx, e.opts, e.log)
	if err != nil {
		return nil, err
	}
	m, err := LoadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("load model2vec %q: %w", e.opts.Model, err)
	}
	e.dir = dir
	e.model = m
	e.log.Info("model2vec loaded", "model", e.opts.Model, "dir", dir, "dim", m.Dim(), "vocab", m.rows)
	return m, nil
}
