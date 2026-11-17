package store

import (
	"context"
	"crypto/rand"
	"fmt"
	"regexp"
)

var uuidRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// newUUID returns a random (version 4) UUID in lowercase canonical form.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate uuid: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// validUUID reports whether s is a lowercase v4 UUID as produced by this package.
func validUUID(s string) bool { return uuidRE.MatchString(s) }

// InstanceID returns the identifier of this database, generated when the
// database was created or migrated to schema version 5. It is stable for the
// life of the file.
func (s *Store) InstanceID(ctx context.Context) (string, error) {
	var id string
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'instance_id'").Scan(&id); err != nil {
		return "", fmt.Errorf("read instance id: %w", err)
	}
	return id, nil
}
