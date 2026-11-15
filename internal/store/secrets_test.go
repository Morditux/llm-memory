package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCheckNoSecrets(t *testing.T) {
	bad := map[string]string{
		"private key":        "-----BEGIN RSA " + "PRIVATE KEY-----\nMIIEowIBAAK...",
		"openssh key":        "-----BEGIN OPENSSH " + "PRIVATE KEY-----\nb3BlbnNzaC...",
		"openai key":         "Use sk-proj-" + strings.Repeat("a", 28) + " for tests",
		"anthropic key":      "key: sk-ant-" + strings.Repeat("b", 28),
		"github pat":         "token ghp_" + strings.Repeat("c", 36),
		"github fg pat":      "github_pat_" + strings.Repeat("d", 36),
		"gitlab pat":         "glpat-" + strings.Repeat("e", 22),
		"aws key":            "AWS_ACCESS_KEY_ID=AKIA" + strings.Repeat("A", 16),
		"slack token":        "xoxb-" + strings.Repeat("1", 12) + "-abcdef",
		"google api key":     "AIza" + strings.Repeat("g", 35),
		"stripe key":         "sk_live_" + strings.Repeat("h", 24),
		"huggingface":        "hf_" + strings.Repeat("i", 34),
		"jwt":                "Authorization: Bearer eyJ" + strings.Repeat("j", 16) + ".eyJ" + strings.Repeat("k", 16) + "." + strings.Repeat("l", 16),
		"url with password":  "postgres://admin:s3cr3tP4ssw0rd@db.example.com:5432/prod",
		"https url password": "https://deploy:myRealToken99@git.example.com/repo.git",
	}
	for name, text := range bad {
		err := checkNoSecrets("content", text)
		var ve *ValidationError
		if !errors.As(err, &ve) || ve.Field != "content" {
			t.Errorf("%s: expected ValidationError, got %v", name, err)
		}
	}

	good := []string{
		"JWT tokens signed with EdDSA, key rotation every 90 days.",
		"Ollama runs on http://localhost:11434/v1/embeddings.",
		"Never store secrets in the repository.",
		"Example DSN: postgres://user:password@localhost:5432/app",
		"Placeholder URL: https://user:<password>@example.com/repo",
		"Env template: postgres://app:$DB_PASS@localhost/db",
		"Mask example: https://user:***@example.com and user:pass@ placeholder",
		"Public key ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI... for deployment.",
		"Use sk-foo for a short test prefix.",
	}
	for _, text := range good {
		if err := checkNoSecrets("content", text); err != nil {
			t.Errorf("false positive on %q: %v", text, err)
		}
	}
}

func TestStoreRejectsSecretsOnCreateAndUpdate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.Create(ctx, NewMemory{
		Kind:    KindFact,
		Content: "Token: ghp_" + strings.Repeat("c", 36),
	})
	var ve *ValidationError
	if !errors.As(err, &ve) || !strings.Contains(ve.Msg, "secret") {
		t.Fatalf("Create with secret content = %v, want ValidationError", err)
	}

	_, err = s.Create(ctx, NewMemory{
		Kind:    KindFact,
		Title:   "Key sk-proj-" + strings.Repeat("a", 28),
		Content: "API key title",
	})
	if !errors.As(err, &ve) || ve.Field != "title" {
		t.Fatalf("Create with secret title = %v, want ValidationError on title", err)
	}

	m, err := s.Create(ctx, NewMemory{Kind: KindFact, Content: "Safe initial content"})
	if err != nil {
		t.Fatal(err)
	}
	bad := "AKIA" + strings.Repeat("A", 16)
	if _, err := s.Update(ctx, m.ID, Patch{Content: &bad}, "test"); !errors.As(err, &ve) {
		t.Fatalf("Update with secret = %v, want ValidationError", err)
	}
}
