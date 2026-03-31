package store

import (
	"errors"
	"fmt"
)

// ErrListLimitExceeded is returned when a list query uses a limit above StoreMaxListLimit.
var ErrListLimitExceeded = errors.New("list limit exceeds store maximum")

// Store-side list limits: default when the caller passes zero; hard cap above typical API max to catch bugs or abuse.
const (
	StoreDefaultListLimit = 100
	StoreMaxListLimit     = 512
)

// ResolveListLimit returns StoreDefaultListLimit for n <= 0, or n when n <= StoreMaxListLimit.
// Callers typically pass limits already clamped by the HTTP layer (smaller API max); the store cap catches abuse or bugs.
func ResolveListLimit(n int) (int, error) {
	if n <= 0 {
		return StoreDefaultListLimit, nil
	}
	if n > StoreMaxListLimit {
		return 0, fmt.Errorf("%w: %d > %d", ErrListLimitExceeded, n, StoreMaxListLimit)
	}
	return n, nil
}
