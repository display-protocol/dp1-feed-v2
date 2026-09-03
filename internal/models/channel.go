package models

import (
	"encoding/json"

	"github.com/display-protocol/dp1-go/extension/identity"
	"github.com/display-protocol/dp1-go/playlist"
)

// DefaultChannelVersion is used when POST /channels omits version (semver).

const DefaultChannelVersion = "1.0.0"

// ChannelCreateRequest is the JSON body for POST /api/v1/channels (extensions).
// Playlists is an ordered list of playlist URIs, resolved the same way as for playlist-groups.
// Slug is optional; when omitted, whitespace-only, or un-slugifiable, the executor derives a unique slug from title (same pattern as playlist-groups). If the title is also un-slugifiable, the executor uses a "channel-" prefix with a short id suffix.
type ChannelCreateRequest struct {
	Title      string            `json:"title" binding:"required"`
	Slug       string            `json:"slug,omitempty"`
	Version    string            `json:"version,omitempty"`
	Playlists  []string          `json:"playlists" binding:"required"`
	Curators   []identity.Entity `json:"curators,omitempty"`
	Publisher  *identity.Entity  `json:"publisher,omitempty"`
	Summary    string            `json:"summary,omitempty"`
	CoverImage string            `json:"coverImage,omitempty"`

	// Trusted model fields: user-provided id, created timestamp, and publisher signatures.
	// When signatures are present and valid, API key authentication is bypassed.
	ID         *string              `json:"id,omitempty"`
	Created    *string              `json:"created,omitempty"`
	Signatures []playlist.Signature `json:"signatures,omitempty"`
	// Signature is the deprecated v1.0.x single signature; see PlaylistCreateRequest.Signature.
	Signature string `json:"signature,omitempty"`

	// Raw is the request body exactly as received; see PlaylistCreateRequest.Raw.
	Raw json.RawMessage `json:"-"`
}

// ChannelReplaceRequest is the JSON body for PUT /api/v1/channels/{id}.
type ChannelReplaceRequest = ChannelCreateRequest

// ChannelUpdateRequest is the JSON body for PATCH /api/v1/channels/{id} (partial update).
// Only non-nil fields are updated; nil fields preserve existing values.
// PATCH is API-key only: a merged document cannot carry a client signature, so there is no signatures field.
type ChannelUpdateRequest struct {
	Title      *string           `json:"title,omitempty"`
	Slug       *string           `json:"slug,omitempty"`
	Version    *string           `json:"version,omitempty"`
	Playlists  []string          `json:"playlists,omitempty"`
	Curators   []identity.Entity `json:"curators,omitempty"`
	Publisher  *identity.Entity  `json:"publisher,omitempty"`
	Summary    *string           `json:"summary,omitempty"`
	CoverImage *string           `json:"coverImage,omitempty"`
}
