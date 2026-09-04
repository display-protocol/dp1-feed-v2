package models

import (
	"encoding/json"

	"github.com/display-protocol/dp1-go/extension/identity"
	"github.com/display-protocol/dp1-go/playlist"
)

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

	// Identity and authorization: the client supplies id, created and the publisher signatures over the
	// document. All three are part of the signed payload and are required (there is no API key).
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
