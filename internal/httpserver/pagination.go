package httpserver

// List query parsing: ?limit= is clamped to API bounds before passing to the executor/store.

import (
	"fmt"
	"strconv"
	"strings"
)

// API list limits (public contract). Store enforces a higher ceiling separately.
const (
	DefaultListLimit = 100
	MinListLimit     = 1
	MaxListLimit     = 100
)

// NormalizeListLimit clamps n to [MinListLimit, MaxListLimit]; zero or negative uses DefaultListLimit.
func NormalizeListLimit(n int) int {
	if n <= 0 {
		return DefaultListLimit
	}
	if n > MaxListLimit {
		return MaxListLimit
	}
	if n < MinListLimit {
		return MinListLimit
	}
	return n
}

// ParseListLimitQuery parses the `limit` query parameter. Empty uses DefaultListLimit.
// Non-empty values must be decimal integers; the result is normalized to API bounds.
func ParseListLimitQuery(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultListLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("limit: %w", err)
	}
	return NormalizeListLimit(n), nil
}
