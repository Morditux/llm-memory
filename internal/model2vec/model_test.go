package model2vec

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func writeSafetensorsFile(t *testing.T, path string, header map[string]any, payload []byte) {
	t.Helper()
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(hb)))
	data := make([]byte, 0, 8+len(hb)+len(payload))
	data = append(data, lenBuf[:]...)
	data = append(data, hb...)
	data = append(data, payload...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func f32Bytes(vals ...float32) []byte {
	b := make([]byte, 4*len(vals))
	for i, v := range vals {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(v))
	}
	return b
}

func approxEq(a, b float32) bool {
	return float32(math.Abs(float64(a-b))) <= 1e-5
}

func TestWordPieceModelAndInference(t *testing.T) {
	dir := t.TempDir()

	cfg := map[string]any{
		"model_type": "model2vec",
		"normalize":  true,
		"max_length": 4,
	}
	cfgBytes, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	tok := map[string]any{
		"added_tokens": []map[string]any{
			{"id": 0, "content": "[PAD]", "special": true},
			{"id": 1, "content": "[UNK]", "special": true},
		},
		"normalizer": map[string]any{
			"type":                 "BertNormalizer",
			"clean_text":           true,
			"handle_chinese_chars": true,
			"strip_accents":        nil,
			"lowercase":            true,
		},
		"pre_tokenizer": map[string]any{
			"type": "BertPreTokenizer",
		},
		"model": map[string]any{
			"type":                      "WordPiece",
			"unk_token":                 "[UNK]",
			"continuing_subword_prefix": "##",
			"max_input_chars_per_word":  100,
			"vocab": map[string]int{
				"[PAD]": 0,
				"[UNK]": 1,
				"hello": 2,
				"world": 3,
				"cafe":  4,
				"##s":   5,
			},
		},
	}
	tokBytes, _ := json.Marshal(tok)
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), tokBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// 6 rows x 2 dims
	// row 2 ("hello") = [3, 0]
	// row 3 ("world") = [0, 4]
	// row 4 ("cafe")  = [1, 1]
	// row 5 ("##s")   = [3, 1]
	payload := f32Bytes(
		0, 0,
		99, 99, // UNK must be ignored
		3, 0,
		0, 4,
		1, 1,
		3, 1,
	)
	header := map[string]any{
		"__metadata__": map[string]string{"format": "pt"},
		"embeddings": map[string]any{
			"dtype":        "F32",
			"shape":        []int{6, 2},
			"data_offsets": []int64{0, int64(len(payload))},
		},
	}
	writeSafetensorsFile(t, filepath.Join(dir, "model.safetensors"), header, payload)

	m, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if m.Dim() != 2 {
		t.Fatalf("Dim = %d, want 2", m.Dim())
	}

	// "CaféS!" -> normalized "cafes !" -> "cafe" (4) + "##s" (5) + "!" (UNK, filtered).
	// Mean = [(1+3)/2, (1+1)/2] = [2, 1], normalized = [2/sqrt(5), 1/sqrt(5)].
	// "hello world" -> mean [1.5, 2.0], norm = 2.5 -> [0.6, 0.8].
	// "unknownword" -> empty token list after UNK filter -> [0, 0].
	vecs := m.Encode([]string{"CaféS!", "hello world", "unknownword"})
	if len(vecs) != 3 {
		t.Fatalf("len(vecs) = %d", len(vecs))
	}

	invSqrt5 := float32(1.0 / math.Sqrt(5.0))
	if !approxEq(vecs[0][0], 2*invSqrt5) || !approxEq(vecs[0][1], 1*invSqrt5) {
		t.Errorf("vecs[0] = %v, want [%v %v]", vecs[0], 2*invSqrt5, invSqrt5)
	}
	if !approxEq(vecs[1][0], 0.6) || !approxEq(vecs[1][1], 0.8) {
		t.Errorf("vecs[1] = %v, want [0.6 0.8]", vecs[1])
	}
	if vecs[2][0] != 0 || vecs[2][1] != 0 {
		t.Errorf("vecs[2] = %v, want [0 0]", vecs[2])
	}
}

func TestUnigramAndMetaspace(t *testing.T) {
	dir := t.TempDir()

	cfgBytes := []byte(`{"model_type":"model2vec","normalize":false}`)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	tok := map[string]any{
		"normalizer": map[string]any{
			"type": "Sequence",
			"normalizers": []any{
				map[string]any{
					"type":    "Replace",
					"pattern": map[string]any{"Regex": `\s+`},
					"content": " ",
				},
				map[string]any{
					"type":        "Strip",
					"strip_left":  true,
					"strip_right": true,
				},
			},
		},
		"pre_tokenizer": map[string]any{
			"type":           "Metaspace",
			"replacement":    "▁",
			"prepend_scheme": "always",
			"split":          false,
		},
		"model": map[string]any{
			"type":          "Unigram",
			"unk_id":        0,
			"byte_fallback": true,
			"vocab": [][]any{
				{"[UNK]", -10.0},
				{"▁bonjour", -1.0},
				{"▁bon", -3.0},
				{"jour", -3.0},
				{"<0x7A>", -8.0}, // 'z'
			},
		},
	}
	tokBytes, _ := json.Marshal(tok)
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), tokBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	payload := f32Bytes(
		0, 0, // [UNK]
		10, 20, // ▁bonjour
		1, 1, // ▁bon
		3, 3, // jour
		4, 8, // <0x7A>
	)
	header := map[string]any{
		"embeddings": map[string]any{
			"dtype":        "F32",
			"shape":        []int{5, 2},
			"data_offsets": []int64{0, int64(len(payload))},
		},
	}
	writeSafetensorsFile(t, filepath.Join(dir, "model.safetensors"), header, payload)

	m, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	// "  bonjour " -> Strip -> "bonjour" -> Metaspace -> "▁bonjour" -> id 1 ([10, 20]).
	vecs := m.Encode([]string{"  bonjour ", "z"})
	if !approxEq(vecs[0][0], 10) || !approxEq(vecs[0][1], 20) {
		t.Errorf("vecs[0] = %v, want [10 20]", vecs[0])
	}
	// "z" -> "▁z" -> "▁" is unknown (filtered UNK), "z" falls back to <0x7A> (id 4: [4, 8]).
	if !approxEq(vecs[1][0], 4) || !approxEq(vecs[1][1], 8) {
		t.Errorf("vecs[1] = %v, want [4 8]", vecs[1])
	}
}

func TestSafetensorsDTypesAndQuantization(t *testing.T) {
	dir := t.TempDir()
	// Test BF16, F16, I8, and mapping/weights.
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"normalize":false}`), 0o644); err != nil {
		t.Fatal(err)
	}
	tokBytes := []byte(`{
		"normalizer":{"type":"BertNormalizer","lowercase":true},
		"pre_tokenizer":{"type":"BertPreTokenizer"},
		"model":{"type":"WordPiece","unk_token":"[UNK]","vocab":{"[UNK]":0,"a":1,"b":2}}
	}`)
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), tokBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	// embeddings: 2 rows x 2 cols as BF16: row0 = [2.0, 4.0], row1 = [6.0, 8.0]
	// float32 2.0 = 0x40000000 -> BF16 0x4000
	// float32 4.0 = 0x40800000 -> BF16 0x4080
	// float32 6.0 = 0x40C00000 -> BF16 0x40C0
	// float32 8.0 = 0x41000000 -> BF16 0x4100
	embBuf := make([]byte, 8)
	binary.LittleEndian.PutUint16(embBuf[0:], 0x4000)
	binary.LittleEndian.PutUint16(embBuf[2:], 0x4080)
	binary.LittleEndian.PutUint16(embBuf[4:], 0x40C0)
	binary.LittleEndian.PutUint16(embBuf[6:], 0x4100)

	// weights: [0, 0.5, 2.0] as F32
	wBuf := f32Bytes(0, 0.5, 2.0)
	// mapping: [0, 0, 1] as I64 -> token 1 ("a") maps to row 0, token 2 ("b") maps to row 1
	mBuf := make([]byte, 24)
	binary.LittleEndian.PutUint64(mBuf[0:], 0)
	binary.LittleEndian.PutUint64(mBuf[8:], 0)
	binary.LittleEndian.PutUint64(mBuf[16:], 1)

	payload := append(append(embBuf, wBuf...), mBuf...)
	header := map[string]any{
		"embeddings": map[string]any{
			"dtype":        "BF16",
			"shape":        []int{2, 2},
			"data_offsets": []int64{0, 8},
		},
		"weights": map[string]any{
			"dtype":        "F32",
			"shape":        []int{3},
			"data_offsets": []int64{8, 20},
		},
		"mapping": map[string]any{
			"dtype":        "I64",
			"shape":        []int{3},
			"data_offsets": []int64{20, 44},
		},
	}
	writeSafetensorsFile(t, filepath.Join(dir, "model.safetensors"), header, payload)

	m, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	// "a b" -> token 1 (row 0 * 0.5 = [1, 2]) + token 2 (row 1 * 2.0 = [12, 16]) -> mean = [6.5, 9.0].
	vecs := m.Encode([]string{"a b"})
	if !approxEq(vecs[0][0], 6.5) || !approxEq(vecs[0][1], 9.0) {
		t.Errorf("vecs[0] = %v, want [6.5 9.0]", vecs[0])
	}

	// Verify float16ToFloat32 on known constants: 0x3C00 = 1.0, 0xC000 = -2.0, 0x0000 = 0.0.
	if got := float16ToFloat32(0x3C00); got != 1.0 {
		t.Errorf("f16(1.0) = %v", got)
	}
	if got := float16ToFloat32(0xC000); got != -2.0 {
		t.Errorf("f16(-2.0) = %v", got)
	}
	if got := float16ToFloat32(0x0000); got != 0.0 {
		t.Errorf("f16(0.0) = %v", got)
	}
}

func TestEmbedderHubDownloadAndCache(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "config.json"), []byte(`{"normalize":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "tokenizer.json"), []byte(`{
		"normalizer":{"type":"BertNormalizer","lowercase":true},
		"pre_tokenizer":{"type":"BertPreTokenizer"},
		"model":{"type":"WordPiece","unk_token":"[UNK]","vocab":{"[UNK]":0,"go":1}}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := f32Bytes(0, 0, 3, 4)
	writeSafetensorsFile(t, filepath.Join(srcDir, "model.safetensors"), map[string]any{
		"embeddings": map[string]any{
			"dtype":        "F32",
			"shape":        []int{2, 2},
			"data_offsets": []int64{0, int64(len(payload))},
		},
	}, payload)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/minishlab/test-model/resolve/main/config.json":
			http.ServeFile(w, r, filepath.Join(srcDir, "config.json"))
		case "/minishlab/test-model/resolve/main/tokenizer.json":
			http.ServeFile(w, r, filepath.Join(srcDir, "tokenizer.json"))
		case "/minishlab/test-model/resolve/main/model.safetensors":
			http.ServeFile(w, r, filepath.Join(srcDir, "model.safetensors"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	emb, err := NewEmbedder(Options{
		Model:    "minishlab/test-model",
		CacheDir: cacheDir,
		BaseURL:  srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if emb.Model() != "minishlab/test-model" {
		t.Fatalf("Model() = %q", emb.Model())
	}
	if _, ready, err := emb.CheckLocal(); err != nil || ready {
		t.Fatalf("expected not cached yet, got ready=%v err=%v", ready, err)
	}

	vecs, err := emb.Embed(context.Background(), []string{"go"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !approxEq(vecs[0][0], 0.6) || !approxEq(vecs[0][1], 0.8) {
		t.Errorf("vecs[0] = %v, want [0.6 0.8]", vecs[0])
	}
	if hits.Load() != 3 {
		t.Errorf("server hits = %d, want 3", hits.Load())
	}
	if _, ready, err := emb.CheckLocal(); err != nil || !ready {
		t.Fatalf("expected cached now, got ready=%v err=%v", ready, err)
	}

	// Second Embedder pointing at the same cache works completely offline.
	srv.Close()
	emb2, err := NewEmbedder(Options{
		Model:    "minishlab/test-model",
		CacheDir: cacheDir,
		BaseURL:  srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	vecs2, err := emb2.Embed(context.Background(), []string{"go"})
	if err != nil {
		t.Fatalf("offline cached Embed: %v", err)
	}
	if !approxEq(vecs2[0][0], 0.6) || !approxEq(vecs2[0][1], 0.8) {
		t.Errorf("vecs2[0] = %v, want [0.6 0.8]", vecs2[0])
	}
}

func TestEmbedderEnsure(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "config.json"), []byte(`{"normalize":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "tokenizer.json"), []byte(`{
		"normalizer":{"type":"BertNormalizer","lowercase":true},
		"pre_tokenizer":{"type":"BertPreTokenizer"},
		"model":{"type":"WordPiece","unk_token":"[UNK]","vocab":{"[UNK]":0,"go":1}}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := f32Bytes(0, 0, 3, 4)
	writeSafetensorsFile(t, filepath.Join(srcDir, "model.safetensors"), map[string]any{
		"embeddings": map[string]any{
			"dtype":        "F32",
			"shape":        []int{2, 2},
			"data_offsets": []int64{0, int64(len(payload))},
		},
	}, payload)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/minishlab/ensure-model/resolve/main/config.json":
			http.ServeFile(w, r, filepath.Join(srcDir, "config.json"))
		case "/minishlab/ensure-model/resolve/main/tokenizer.json":
			http.ServeFile(w, r, filepath.Join(srcDir, "tokenizer.json"))
		case "/minishlab/ensure-model/resolve/main/model.safetensors":
			http.ServeFile(w, r, filepath.Join(srcDir, "model.safetensors"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cacheDir := t.TempDir()
	emb, err := NewEmbedder(Options{
		Model:    "minishlab/ensure-model",
		CacheDir: cacheDir,
		BaseURL:  srv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}

	dir, downloaded, err := emb.Ensure(context.Background())
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	if !downloaded {
		t.Error("expected downloaded=true on first Ensure")
	}
	if hits.Load() != 3 {
		t.Errorf("server hits = %d, want 3", hits.Load())
	}
	wantDir := filepath.Join(cacheDir, "minishlab", "ensure-model")
	if dir != wantDir {
		t.Errorf("dir = %q, want %q", dir, wantDir)
	}
	if _, ready, err := emb.CheckLocal(); err != nil || !ready {
		t.Fatalf("expected ready=true after Ensure, got ready=%v err=%v", ready, err)
	}

	// Second Ensure works completely offline and reports downloaded=false.
	srv.Close()
	dir2, downloaded2, err := emb.Ensure(context.Background())
	if err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	if downloaded2 {
		t.Error("expected downloaded=false when already cached")
	}
	if dir2 != wantDir {
		t.Errorf("dir2 = %q, want %q", dir2, wantDir)
	}

	// Incomplete local directory returns an error.
	incompleteDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(incompleteDir, "config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	localEmb, err := NewEmbedder(Options{Model: incompleteDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := localEmb.Ensure(context.Background()); err == nil {
		t.Error("expected error on incomplete local model directory")
	}
}

func TestPrecompiledNormalizerInvalidBase64(t *testing.T) {
	if _, err := parsePrecompiledNormalizer("%%%"); err == nil {
		t.Error("expected error on invalid base64")
	}
	// Valid empty trie header (trie_size=0).
	empty := base64.StdEncoding.EncodeToString([]byte{0, 0, 0, 0})
	p, err := parsePrecompiledNormalizer(empty)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := p.Normalize("abc"); got != "abc" {
		t.Errorf("got %q, want abc", got)
	}
}
