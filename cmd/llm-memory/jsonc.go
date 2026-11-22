package main

import (
	"bytes"
	"encoding/json"
	"strings"
)

// stripJSONC removes // line comments, /* ... */ block comments, and trailing
// commas outside JSON string literals, plus an optional leading UTF-8 BOM.
func stripJSONC(data []byte) (clean []byte, hasComments bool) {
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})

	// Pass 1: strip comments outside strings while preserving newlines.
	noComments := make([]byte, 0, len(data))
	inString := false
	escaped := false
	for i := 0; i < len(data); i++ {
		b := data[i]
		if inString {
			noComments = append(noComments, b)
			switch {
			case escaped:
				escaped = false
			case b == '\\':
				escaped = true
			case b == '"':
				inString = false
			}
			continue
		}
		if b == '"' {
			inString = true
			noComments = append(noComments, b)
			continue
		}
		if b == '/' && i+1 < len(data) {
			if data[i+1] == '/' {
				hasComments = true
				i += 2
				for i < len(data) && data[i] != '\n' {
					i++
				}
				if i < len(data) && data[i] == '\n' {
					noComments = append(noComments, '\n')
				}
				continue
			}
			if data[i+1] == '*' {
				hasComments = true
				i += 2
				for i < len(data) {
					if data[i] == '*' && i+1 < len(data) && data[i+1] == '/' {
						i++
						break
					}
					if data[i] == '\n' {
						noComments = append(noComments, '\n')
					}
					i++
				}
				continue
			}
		}
		noComments = append(noComments, b)
	}

	// Pass 2: strip trailing commas before } or ] outside strings.
	clean = make([]byte, 0, len(noComments))
	inString = false
	escaped = false
	for i := 0; i < len(noComments); i++ {
		b := noComments[i]
		if inString {
			clean = append(clean, b)
			switch {
			case escaped:
				escaped = false
			case b == '\\':
				escaped = true
			case b == '"':
				inString = false
			}
			continue
		}
		if b == '"' {
			inString = true
			clean = append(clean, b)
			continue
		}
		if b == ',' {
			j := i + 1
			for j < len(noComments) && (noComments[j] == ' ' || noComments[j] == '\t' || noComments[j] == '\r' || noComments[j] == '\n') {
				j++
			}
			if j < len(noComments) && (noComments[j] == '}' || noComments[j] == ']') {
				continue
			}
		}
		clean = append(clean, b)
	}
	return clean, hasComments
}

// decodeJSONC strips JSONC comments and trailing commas and unmarshals into v.
func decodeJSONC(data []byte, v any) (hasComments bool, err error) {
	clean, hasComments := stripJSONC(data)
	if len(strings.TrimSpace(string(clean))) == 0 {
		return hasComments, nil
	}
	if err := json.Unmarshal(clean, v); err != nil {
		return hasComments, err
	}
	return hasComments, nil
}
