package models

// PlaylistGroupCreateRequest is the JSON body for POST /api/v1/playlist-groups.
// Playlists is an ordered list of playlist URIs; the executor resolves each to a stored playlist row.
type PlaylistGroupCreateRequest struct {
	Title      string   `json:"title" binding:"required"`
	Slug       string   `json:"slug,omitempty"`
	Playlists  []string `json:"playlists" binding:"required"`
	Curator    string   `json:"curator,omitempty"`
	Summary    string   `json:"summary,omitempty"`
	CoverImage string   `json:"coverImage,omitempty"`
}

// PlaylistGroupReplaceRequest is the JSON body for PUT /api/v1/playlist-groups/{id}.
type PlaylistGroupReplaceRequest = PlaylistGroupCreateRequest

// PlaylistGroupUpdateRequest is the JSON body for PATCH /api/v1/playlist-groups/{id} (partial update).
// Only non-nil fields are updated; nil fields preserve existing values.
type PlaylistGroupUpdateRequest struct {
	Title      *string  `json:"title,omitempty"`
	Slug       *string  `json:"slug,omitempty"`
	Playlists  []string `json:"playlists,omitempty"`
	Curator    *string  `json:"curator,omitempty"`
	Summary    *string  `json:"summary,omitempty"`
	CoverImage *string  `json:"coverImage,omitempty"`
}
