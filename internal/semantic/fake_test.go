package semantic

import (
	"context"
	"encoding/json"
	"errors"
	"hash/fnv"
	"net/http"
	"strings"
	"sync"
	"unicode"
)

// concepts gives the fake model a notion of meaning: words of the same
// concept share a dimension, so "SGBD" is close to "PostgreSQL" although
// they share no word.
var concepts = [][]string{
	{"postgresql", "mysql", "sgbd", "database", "base", "données", "donnees", "sql", "jsonb"},
	{"déploiement", "deploiement", "docker", "compose", "vps", "hetzner", "deploy", "hébergement", "hosting", "serveur"},
	{"réponses", "courtes", "concises", "concise", "style", "ton", "verbeux"},
	{"jwt", "authentification", "login", "token", "connexion", "eddsa"},
}

// otherDims spreads words outside any concept over hashed dimensions, so
// that two unrelated texts are (nearly) orthogonal.
const otherDims = 16

func fakeVector(text string) []float32 {
	v := make([]float32, len(concepts)+otherDims)
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}) {
		found := false
		for i, c := range concepts {
			for _, cw := range c {
				if w == cw {
					v[i]++
					found = true
				}
			}
		}
		if !found {
			h := fnv.New32a()
			_, _ = h.Write([]byte(w))
			v[len(concepts)+int(h.Sum32()%otherDims)] += 0.2
		}
	}
	return v
}

// fakeEmbedder is an in-process Embedder that counts calls and can fail.
type fakeEmbedder struct {
	mu     sync.Mutex
	model  string
	calls  int
	fail   error
	failOn func(texts []string) bool
	dims   int // when > 0, vectors are padded to this size
}

func (f *fakeEmbedder) Model() string { return f.model }

func (f *fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail != nil && (f.failOn == nil || f.failOn(texts)) {
		return nil, f.fail
	}
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := fakeVector(t)
		for len(v) < f.dims {
			v = append(v, 0)
		}
		out[i] = v
	}
	return out, nil
}

func (f *fakeEmbedder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

var errAPIDown = errors.New("connection refused")

// fakeAPI serves the OpenAI embeddings format with fakeVector.
func fakeAPI(requests *int, mu *sync.Mutex) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*requests++
		mu.Unlock()
		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		resp := struct {
			Data []item `json:"data"`
		}{}
		for i := len(req.Input) - 1; i >= 0; i-- { // reverse order: clients must use index
			resp.Data = append(resp.Data, item{i, fakeVector(req.Input[i])})
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}
