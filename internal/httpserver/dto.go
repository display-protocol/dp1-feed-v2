package httpserver

// Response DTOs aligned with OpenAPI (shared list envelope for playlists, groups, and channels).

// ListResponse is the JSON envelope for GET list endpoints: items, optional cursor, hasMore.
type ListResponse[T any] struct {
	Items   []T    `json:"items"`
	Cursor  string `json:"cursor,omitempty"`
	HasMore bool   `json:"hasMore"`
}

// NewListResponse sets HasMore from nextCursor (non-empty means another page exists).
func NewListResponse[T any](items []T, nextCursor string) ListResponse[T] {
	return ListResponse[T]{
		Items:   items,
		Cursor:  nextCursor,
		HasMore: nextCursor != "",
	}
}
