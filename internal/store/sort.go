package store

import (
	"fmt"
	"strings"
)

// SortOrder is the allowed sort direction for list endpoints (e.g. playlists by created_at).
// ParseSortOrder reads ?sort=; SQLOrder and TupleAfterCursorOp supply ORDER BY and keyset cursor SQL fragments.
type SortOrder string

const (
	// SortAsc orders from oldest to newest by created_at.
	SortAsc SortOrder = "asc"
	// SortDesc orders from newest to oldest by created_at.
	SortDesc SortOrder = "desc"
)

// ParseSortOrder parses the `sort` query parameter into a SortOrder. Empty string defaults to SortAsc.
func ParseSortOrder(s string) (SortOrder, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", string(SortAsc):
		return SortAsc, nil
	case string(SortDesc):
		return SortDesc, nil
	default:
		return "", fmt.Errorf("sort must be %q or %q", SortAsc, SortDesc)
	}
}

// SQLOrder returns "ASC" or "DESC" for ORDER BY clauses.
func (o SortOrder) SQLOrder() string {
	if o == SortDesc {
		return "DESC"
	}
	return "ASC"
}

// TupleAfterCursorOp is the SQL comparison for (created_at, id) vs the decoded cursor tuple so the next page
// continues strictly after the last row in sort order (ASC uses '>', DESC uses '<').
func (o SortOrder) TupleAfterCursorOp() string {
	if o == SortDesc {
		return "<"
	}
	return ">"
}
