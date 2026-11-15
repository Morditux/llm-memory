package store

import (
	"fmt"
	"regexp"
	"strings"
)

type secretRule struct {
	name string
	re   *regexp.Regexp
}

var secretRules = []secretRule{
	{name: "private key", re: regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)},
	{name: "OpenAI/Anthropic API key", re: regexp.MustCompile(`\bsk-(?:proj-|ant-)?[A-Za-z0-9_-]{20,}\b`)},
	{name: "GitHub token", re: regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{30,}\b`)},
	{name: "GitHub fine-grained PAT", re: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{30,}\b`)},
	{name: "GitLab PAT", re: regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`)},
	{name: "AWS access key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{name: "Slack token", re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
	{name: "Google API key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{name: "Stripe API key", re: regexp.MustCompile(`\b(?:sk|rk)_(?:live|test)_[0-9A-Za-z]{20,}\b`)},
	{name: "HuggingFace token", re: regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}\b`)},
	{name: "JWT", re: regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)},
}

var urlCredsRe = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://([^/\s:@]+):([^/\s@]+)@[^\s]+`)

func isPlaceholderSecret(pw string) bool {
	lower := strings.ToLower(strings.TrimSpace(pw))
	switch lower {
	case "", "password", "pass", "secret", "xxx", "***":
		return true
	}
	if strings.HasPrefix(lower, "<") && strings.HasSuffix(lower, ">") {
		return true
	}
	if strings.HasPrefix(lower, "$") {
		return true
	}
	if strings.Trim(lower, "*") == "" || strings.Trim(lower, "x") == "" {
		return true
	}
	return false
}

// checkNoSecrets rejects text that appears to contain credentials or tokens.
func checkNoSecrets(field, s string) error {
	if s == "" {
		return nil
	}
	for _, r := range secretRules {
		if r.re.MatchString(s) {
			return &ValidationError{
				Field: field,
				Msg:   fmt.Sprintf("appears to contain a secret (%s): never store secrets in llm-memory", r.name),
			}
		}
	}
	for _, m := range urlCredsRe.FindAllStringSubmatch(s, -1) {
		if len(m) >= 3 && !isPlaceholderSecret(m[2]) {
			return &ValidationError{
				Field: field,
				Msg:   "appears to contain a secret (URL credentials): never store secrets in llm-memory",
			}
		}
	}
	return nil
}
