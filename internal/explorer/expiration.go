package explorer

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Morditux/llm-memory/internal/store"
)

// ParseExpiration converts an expires_in duration ("90m", "7d") or an expires_at
// RFC 3339 timestamp into a UTC time. It returns nil when both are empty and a
// *store.ValidationError when both are set or either is malformed.
func ParseExpiration(expiresIn, expiresAt string) (*time.Time, error) {
	in := strings.TrimSpace(expiresIn)
	at := strings.TrimSpace(expiresAt)
	if in != "" && at != "" {
		return nil, &store.ValidationError{Field: "expires", Msg: "provide either expires_in or expires_at, not both"}
	}
	if in != "" {
		if strings.HasSuffix(in, "d") {
			days, err := strconv.Atoi(strings.TrimSuffix(in, "d"))
			if err != nil || days <= 0 {
				return nil, &store.ValidationError{Field: "expires_in", Msg: fmt.Sprintf("invalid duration %q", expiresIn)}
			}
			t := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour)
			return &t, nil
		}
		d, err := time.ParseDuration(in)
		if err != nil || d <= 0 {
			return nil, &store.ValidationError{Field: "expires_in", Msg: fmt.Sprintf("invalid duration %q", expiresIn)}
		}
		t := time.Now().UTC().Add(d)
		return &t, nil
	}
	if at != "" {
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return nil, &store.ValidationError{Field: "expires_at", Msg: fmt.Sprintf("invalid RFC 3339 timestamp %q", expiresAt)}
		}
		utc := t.UTC()
		return &utc, nil
	}
	return nil, nil
}
