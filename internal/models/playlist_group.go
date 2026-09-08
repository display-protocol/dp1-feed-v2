package models

import (
	"encoding/json"

	"github.com/display-protocol/dp1-go/playlist"
)

// PlaylistGroupCreateRequest is the JSON body for POST /api/v1/playlist-groups.
// Playlists is an ordered list of playlist URIs; the executor resolves each to a stored playlist row.
// Curator is a display name (DP-1 core), not a key: the group's owners are its curator-role signers.
type PlaylistGroupCreateRequest struct {
	Title      string   `json:"title" binding:"required"`
	Slug       string   `json:"slug,omitempty"`
	Playlists  []string `json:"playlists" binding:"required"`
	Curator    string   `json:"curator,omitempty"`
	Summary    string   `json:"summary,omitempty"`
	CoverImage string   `json:"coverImage,omitempty"`

	// Identity and authorization: the client supplies id, created and the signatures over the document,
	// at least one of which must carry the curator role. All three are part of the signed payload and are
	// required (there is no API key).
	ID         *string              `json:"id,omitempty"`
	Created    *string              `json:"created,omitempty"`
	Signatures []playlist.Signature `json:"signatures,omitempty"`

	// Signature is the deprecated v1.0.x single signature; see PlaylistCreateRequest.Signature.
	Signature string `json:"signature,omitempty"`

	// Raw is the request body exactly as received; see PlaylistCreateRequest.Raw.
	Raw json.RawMessage `json:"-"`
}

// PlaylistGroupReplaceRequest is the JSON body for PUT /api/v1/playlist-groups/{id}.
type PlaylistGroupReplaceRequest = PlaylistGroupCreateRequest
