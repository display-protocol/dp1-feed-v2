// Package utils holds small shared helpers (JSON encode/decode for DB and similar).
package utils

import (
	"encoding/json"
	"fmt"
)

// DecodeJSONB unmarshals Postgres jsonb octets into T.
// label appears in errors (e.g. "playlist body", "playlist item").
func DecodeJSONB[T any](raw []byte, label string) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, fmt.Errorf("decode %s: %w", label, err)
	}
	return v, nil
}

// EncodeJSONB marshals v for PostgreSQL jsonb query parameters.
func EncodeJSONB(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode jsonb: %w", err)
	}
	return b, nil
}
