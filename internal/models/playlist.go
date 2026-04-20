// Package models holds HTTP request shapes bound by Gin (JSON tags + binding) before executor + dp1-go validation.
package models

import (
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
}

// PlaylistReplaceRequest is the JSON body for PUT /api/v1/playlists/{id} (full replacement, same shape as create).
type PlaylistReplaceRequest = PlaylistCreateRequest

// PlaylistUpdateRequest is the JSON body for PATCH /api/v1/playlists/{id} (partial update with optional fields).
// Only non-nil fields are updated; nil fields preserve existing values.
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

	// Trusted model: when non-empty, same semantics as create — verify curator signatures then feed co-signs.
	Signatures []playlist.Signature `json:"signatures,omitempty"`
}
