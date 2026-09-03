// Package models holds HTTP request shapes bound by Gin (JSON tags + binding) before executor + dp1-go validation.
package models

import (
	"encoding/json"

	"github.com/display-protocol/dp1-go/extension/identity"
	dp1playlists "github.com/display-protocol/dp1-go/extension/playlists"
	"github.com/display-protocol/dp1-go/playlist"
)

// DefaultDPVersion is the operator default when the client omits dpVersion (DP-1 v1.1.0+).
const DefaultDPVersion = "1.1.0"

// PlaylistCreateRequest is the JSON body for POST /api/v1/playlists (aligned with OpenAPI PlaylistInput).
// Gin binds and validates required fields before the executor runs schema validation via dp1-go.
type PlaylistCreateRequest struct {
	DPVersion string                  `json:"dpVersion" binding:"required"`
	Title     string                  `json:"title" binding:"required"`
	Slug      string                  `json:"slug,omitempty"`
	Items     []playlist.PlaylistItem `json:"items" binding:"required"`

	Note         *dp1playlists.Note         `json:"note,omitempty"`
	Curators     []identity.Entity          `json:"curators,omitempty"`
	Summary      string                     `json:"summary,omitempty"`
	CoverImage   string                     `json:"coverImage,omitempty"`
	Defaults     *playlist.Defaults         `json:"defaults,omitempty"`
	DynamicQuery *dp1playlists.DynamicQuery `json:"dynamicQuery,omitempty"`

	// Trusted model fields: user-provided id, created timestamp, and curator signatures.
	// When signatures are present and valid, API key authentication is bypassed.
	ID         *string              `json:"id,omitempty"`
	Created    *string              `json:"created,omitempty"`
	Signatures []playlist.Signature `json:"signatures,omitempty"`
	// Signature is the deprecated v1.0.x single signature. Accepted so a signed document carrying one is
	// not rejected as an unknown field; it is preserved verbatim on the signed path and ignored on the API-key path.
	Signature string `json:"signature,omitempty"`

	// Raw is the request body exactly as received, set by the HTTP layer (never decoded from JSON).
	// On the signed-document path the executor verifies, co-signs, and stores these bytes verbatim;
	// the decoded fields above are only read, never used to rebuild the document.
	Raw json.RawMessage `json:"-"`
}

// PlaylistReplaceRequest is the JSON body for PUT /api/v1/playlists/{id} (full replacement, same shape as create).
type PlaylistReplaceRequest = PlaylistCreateRequest

// PlaylistUpdateRequest is the JSON body for PATCH /api/v1/playlists/{id} (partial update with optional fields).
// Only non-nil fields are updated; nil fields preserve existing values.
// PATCH is API-key only: a merged document cannot carry a client signature, so there is no signatures field.
type PlaylistUpdateRequest struct {
	DPVersion *string                 `json:"dpVersion,omitempty"`
	Title     *string                 `json:"title,omitempty"`
	Slug      *string                 `json:"slug,omitempty"`
	Items     []playlist.PlaylistItem `json:"items,omitempty"`

	Note         *dp1playlists.Note         `json:"note,omitempty"`
	Curators     []identity.Entity          `json:"curators,omitempty"`
	Summary      *string                    `json:"summary,omitempty"`
	CoverImage   *string                    `json:"coverImage,omitempty"`
	Defaults     *playlist.Defaults         `json:"defaults,omitempty"`
	DynamicQuery *dp1playlists.DynamicQuery `json:"dynamicQuery,omitempty"`
}
