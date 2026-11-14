package semantic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewClientValidation(t *testing.T) {
	for _, c := range []ClientConfig{
		{URL: "", Model: "m"},
		{URL: "localhost:11434/v1/embeddings", Model: "m"},
		{URL: "ftp://host/x", Model: "m"},
		{URL: "http:///nohost", Model: "m"},
		{URL: "http://localhost/v1/embeddings", Model: "  "},
	} {
		if _, err := NewClient(c); err == nil {
			t.Errorf("%+v: expected error", c)
		}
	}
	c, err := NewClient(ClientConfig{URL: "https://api.example.com/v1/embeddings", Model: "m"})
	if err != nil || c.Model() != "m" || c.cfg.Timeout != DefaultTimeout || c.cfg.BatchSize != DefaultBatchSize {
		t.Errorf("defaults: %+v, %v", c, err)
	}
}

func TestClientRequestAndBatching(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Lock()
		bodies = append(bodies, body)
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		inputs := body["input"].([]any)
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		var data []item
		for i := len(inputs) - 1; i >= 0; i-- {
			data = append(data, item{i, []float32{float32(len(inputs[i].(string))), 1}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	c, err := NewClient(ClientConfig{URL: srv.URL, Model: "test-model", APIKey: "k3y", BatchSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{"a", "bb", "ccc", "dddd", "eeeee"}
	vecs, err := c.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vecs {
		if int(v[0]) != len(texts[i]) {
			t.Errorf("vector %d out of order: %v", i, v)
		}
	}
	if len(bodies) != 3 {
		t.Fatalf("%d requests, want 3 (batches of 2)", len(bodies))
	}
	if bodies[0]["model"] != "test-model" || len(bodies[2]["input"].([]any)) != 1 {
		t.Errorf("request bodies: %v", bodies)
	}
	if auth[0] != "Bearer k3y" {
		t.Errorf("Authorization = %q", auth[0])
	}

	// No API key: no Authorization header.
	auth = nil
	c2, _ := NewClient(ClientConfig{URL: srv.URL, Model: "m"})
	if _, err := c2.Embed(context.Background(), []string{"x"}); err != nil || auth[0] != "" {
		t.Errorf("without key: auth=%q err=%v", auth, err)
	}
}

func TestClientErrors(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"http 500": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "model not loaded", http.StatusInternalServerError)
		},
		"malformed": func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("{not json")) },
		"count":     func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":[]}`)) },
		"duplicate": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[1]}]}`))
		},
		"out of range": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"index":5,"embedding":[1]},{"index":0,"embedding":[1]}]}`))
		},
		"empty vector": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[]},{"index":1,"embedding":[1]}]}`))
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			c, _ := NewClient(ClientConfig{URL: srv.URL, Model: "m"})
			_, err := c.Embed(context.Background(), []string{"a", "b"})
			if err == nil {
				t.Fatal("expected error")
			}
			if name == "http 500" && !strings.Contains(err.Error(), "model not loaded") {
				t.Errorf("error should carry the API message: %v", err)
			}
		})
	}

	// Timeout.
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	c, _ := NewClient(ClientConfig{URL: slow.URL, Model: "m", Timeout: 50 * time.Millisecond})
	start := time.Now()
	if _, err := c.Embed(context.Background(), []string{"a"}); err == nil || time.Since(start) > time.Second {
		t.Errorf("timeout not enforced: %v after %v", err, time.Since(start))
	}

	// Unreachable.
	c, _ = NewClient(ClientConfig{URL: "http://127.0.0.1:1/v1/embeddings", Model: "m", Timeout: time.Second})
	if _, err := c.Embed(context.Background(), []string{"a"}); err == nil {
		t.Error("unreachable API: expected error")
	}
}
