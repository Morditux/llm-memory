package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Morditux/llm-memory/internal/config"
)

const (
	instructionStartMarker = "<!-- llm-memory:start -->"
	instructionEndMarker   = "<!-- llm-memory:end -->"
	instructionHeading     = "## Shared memory (llm-memory MCP server)"
)

const clientInstructionBlock = instructionStartMarker + "\n" +
	instructionHeading + "\n\n" +
	"You share a persistent memory with other AI assistants.\n\n" +
	"- At the start of every task, call `get_context` with the project namespace (call `list_namespaces` first if unsure, or use `global`) before answering.\n" +
	"- Before asking me for context, search for it with `recall`.\n" +
	"- Immediately store with `remember`: decisions made and their reason, stable project facts (project namespace), and my lasting cross-project preferences (`namespace: \"global\"`). Do not store transient details or the content of files already in the repository.\n" +
	"- If a memory becomes wrong or obsolete, fix it with `update_memory`, replace it (`remember` with `supersedes`), or archive it with `forget`.\n" +
	"- When I say I am switching tools or stopping, or at the end of a long task, call `handoff` with the state, next steps and open questions.\n" +
	"- Never store secrets (passwords, API keys, tokens).\n" +
	instructionEndMarker

func instructionFilePath(client string, project bool, env config.Env) (string, bool, error) {
	if project {
		wd, err := resolveWd(env)
		if err != nil {
			return "", false, err
		}
		switch client {
		case "antigravity":
			return filepath.Join(wd, "GEMINI.md"), true, nil
		case "claude-code":
			return filepath.Join(wd, "CLAUDE.md"), true, nil
		case "codex", "opencode":
			return filepath.Join(wd, "AGENTS.md"), true, nil
		default:
			return "", false, nil
		}
	}

	home, err := env.UserHomeDir()
	if err != nil {
		return "", false, fmt.Errorf("resolve home directory: %w", err)
	}
	xdgConfig := env.Getenv("XDG_CONFIG_HOME")
	if xdgConfig == "" {
		xdgConfig = filepath.Join(home, ".config")
	}
	switch client {
	case "antigravity":
		return filepath.Join(home, ".gemini", "GEMINI.md"), true, nil
	case "claude-code":
		return filepath.Join(home, ".claude", "CLAUDE.md"), true, nil
	case "codex":
		return filepath.Join(home, ".codex", "AGENTS.md"), true, nil
	case "opencode":
		return filepath.Join(xdgConfig, "opencode", "AGENTS.md"), true, nil
	default:
		return "", false, nil
	}
}

func upsertInstructionBlock(path string) error {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}

	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	var updated string

	startIdx := strings.Index(content, instructionStartMarker)
	endIdx := strings.Index(content, instructionEndMarker)
	switch {
	case startIdx >= 0 && endIdx > startIdx:
		updated = content[:startIdx] + clientInstructionBlock + content[endIdx+len(instructionEndMarker):]
	case replaceLegacyInstructionHeading(content) != "":
		updated = replaceLegacyInstructionHeading(content)
	case strings.TrimSpace(content) != "":
		updated = strings.TrimRight(content, "\n") + "\n\n" + clientInstructionBlock + "\n"
	default:
		updated = clientInstructionBlock + "\n"
	}

	updated = strings.TrimRight(updated, "\n") + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(updated), mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func replaceLegacyInstructionHeading(content string) string {
	lines := strings.Split(content, "\n")
	startLine := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == instructionHeading {
			startLine = i
			break
		}
	}
	if startLine < 0 {
		return ""
	}
	endLine := len(lines)
	for i := startLine + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			endLine = i
			break
		}
	}

	var out []string
	out = append(out, lines[:startLine]...)
	out = append(out, clientInstructionBlock)
	if endLine < len(lines) {
		// Ensure a blank line between our block and the next heading.
		out = append(out, "")
		out = append(out, lines[endLine:]...)
	}
	return strings.Join(out, "\n")
}

func removeInstructionBlock(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}

	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	startIdx := strings.Index(content, instructionStartMarker)
	endIdx := strings.Index(content, instructionEndMarker)
	if startIdx < 0 || endIdx <= startIdx {
		return false, nil
	}

	before := strings.TrimRight(content[:startIdx], " \t\n")
	after := strings.TrimLeft(content[endIdx+len(instructionEndMarker):], " \t\n")

	var updated string
	switch {
	case before == "" && after == "":
		// Delete empty instruction files so e.g. an empty AGENTS.md does not shadow CLAUDE.md.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, fmt.Errorf("remove %s: %w", path, err)
		}
		return true, nil
	case before == "":
		updated = after + "\n"
	case after == "":
		updated = before + "\n"
	default:
		updated = before + "\n\n" + after + "\n"
	}

	if err := os.WriteFile(path, []byte(updated), mode); err != nil {
		return false, fmt.Errorf("write %s: %w", path, err)
	}
	return true, nil
}
