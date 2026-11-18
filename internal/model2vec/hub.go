package model2vec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultHubBaseURL = "https://huggingface.co"

var requiredModelFiles = []string{"config.json", "tokenizer.json", "model.safetensors"}

func resolveLocalModelDir(model, cacheDir string) (dir string, ready bool, err error) {
	if isLocalPathSpec(model) {
		info, statErr := os.Stat(model)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return model, false, fmt.Errorf("model2vec directory %q does not exist", model)
			}
			return model, false, fmt.Errorf("stat model2vec directory %q: %w", model, statErr)
		}
		if !info.IsDir() {
			return model, false, fmt.Errorf("model2vec path %q is not a directory", model)
		}
		return model, hasAllModelFiles(model), nil
	}

	// If a plain relative directory with that name exists locally, prefer it.
	if info, statErr := os.Stat(model); statErr == nil && info.IsDir() {
		return model, hasAllModelFiles(model), nil
	}

	owner, repo, err := parseRepoID(model)
	if err != nil {
		return "", false, err
	}
	if cacheDir == "" {
		return "", false, errors.New("model2vec cache directory must not be empty")
	}
	dir = filepath.Join(cacheDir, owner, repo)
	return dir, hasAllModelFiles(dir), nil
}

func isLocalPathSpec(model string) bool {
	if filepath.IsAbs(model) || strings.HasPrefix(model, "./") || strings.HasPrefix(model, "../") ||
		strings.HasPrefix(model, `.\`) || strings.HasPrefix(model, `..\`) {
		return true
	}
	// A bare directory without "/" or with >1 slashes cannot be an owner/repo HuggingFace ID.
	return strings.Count(model, "/") != 1
}

func parseRepoID(model string) (owner, repo string, err error) {
	parts := strings.Split(model, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" ||
		parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." ||
		strings.ContainsAny(model, `\`) {
		return "", "", fmt.Errorf("invalid model2vec model %q (expected owner/repo or an existing local directory)", model)
	}
	return parts[0], parts[1], nil
}

func hasAllModelFiles(dir string) bool {
	for _, name := range requiredModelFiles {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.IsDir() {
			return false
		}
	}
	return true
}

func ensureModelFiles(ctx context.Context, opts Options, log *slog.Logger) (string, error) {
	dir, ready, err := resolveLocalModelDir(opts.Model, opts.CacheDir)
	if err != nil {
		return "", err
	}
	if ready {
		return dir, nil
	}
	if isLocalPathSpec(opts.Model) {
		return "", fmt.Errorf("local model2vec directory %q is missing one of %v", dir, requiredModelFiles)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create model cache dir %q: %w", dir, err)
	}

	baseURL := strings.TrimRight(opts.BaseURL, "/")
	if baseURL == "" {
		if envURL := strings.TrimSpace(os.Getenv("HF_ENDPOINT")); envURL != "" {
			baseURL = strings.TrimRight(envURL, "/")
		} else {
			baseURL = defaultHubBaseURL
		}
	}

	client := opts.HTTP
	if client == nil {
		client = &http.Client{
			Transport: &http.Transport{
				ResponseHeaderTimeout: 30 * time.Second,
			},
		}
	}

	for _, name := range requiredModelFiles {
		dst := filepath.Join(dir, name)
		if info, statErr := os.Stat(dst); statErr == nil && !info.IsDir() {
			continue
		}
		fileURL := fmt.Sprintf("%s/%s/resolve/main/%s", baseURL, opts.Model, name)
		log.Info("downloading model2vec file", "model", opts.Model, "file", name, "url", fileURL)
		if err := downloadFile(ctx, client, fileURL, dst); err != nil {
			return "", fmt.Errorf("download %s for %s: %w", name, opts.Model, err)
		}
	}
	return dir, nil
}

func downloadFile(ctx context.Context, client *http.Client, fileURL, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fileURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}
