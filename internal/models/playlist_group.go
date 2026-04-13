package models

import "github.com/display-protocol/dp1-go/playlist"

// PlaylistGroupCreateRequest is the JSON body for POST /api/v1/playlist-groups.
// Playlists is an ordered list of playlist URIs; the executor resolves each to a stored playlist row.
type PlaylistGroupCreateRequest struct {
	Title      string   `json:"title" binding:"required"`
	Slug       string   `json:"slug,omitempty"`
	Playlists  []string `json:"playlists" binding:"required"`
	Curator    string   `json:"curator,omitempty"`
	Summary    string   `json:"summary,omitempty"`
	CoverImage string   `json:"coverImage,omitempty"`

	// Trusted model fields: user-provided id, created timestamp, and curator signatures.
	// When signatures are present and valid, API key authentication is bypassed.
	ID         *string              `json:"id,omitempty"`
	Created    *string              `json:"created,omitempty"`
	Signatures []playlist.Signature `json:"signatures,omitempty"`
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
