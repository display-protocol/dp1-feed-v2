// Package models holds HTTP request shapes bound by Gin (JSON tags + binding) before executor + dp1-go validation.
package models

import (
	"encoding/json"
	"errors"

	"github.com/display-protocol/dp1-go/extension/identity"
	dp1playlists "github.com/display-protocol/dp1-go/extension/playlists"
	"github.com/display-protocol/dp1-go/playlist"
)

// PlaylistCreateRequest is the JSON body for POST /api/v1/playlists (aligned with OpenAPI PlaylistInput).
// Gin binds and validates required fields before the executor runs schema validation via dp1-go.
type PlaylistCreateRequest struct {
	DPVersion string                  `json:"dpVersion" binding:"required"`
	Title     string                  `json:"title" binding:"required"`
	Slug      string                  `json:"slug,omitempty"`
	Items     []playlist.PlaylistItem `json:"items" binding:"required"`

	Note     *dp1playlists.Note `json:"note,omitempty"`
	Curators []identity.Entity  `json:"curators,omitempty"`
	Summary  string             `json:"summary,omitempty"`
	// OpenAt is when this playlist's own subject became public (an exhibition's opening,
	// a collection's release) — a playlist-level fact for ordering/display, distinct from
	// any item's displayAt scheduling (dp1#55; see identity.Entity's own doc comment for
	// why this SDK needed updating for it).
	OpenAt       string                     `json:"openAt,omitempty"`
	CoverImage   string                     `json:"coverImage,omitempty"`
	Defaults     *playlist.Defaults         `json:"defaults,omitempty"`
	DynamicQuery *dp1playlists.DynamicQuery `json:"dynamicQuery,omitempty"`
	// Links is editorial content about the playlist (articles, events, news), dp1#55.
	Links []identity.Link `json:"links,omitempty"`

	// Visibility is the playlists extension's listing request (§3.7): "public" (the default when absent)
	// or "unlisted". Modeled here so strict decoding accepts it, with this feed's own type rather than
	// dp1-go's `*string` (see Visibility). The feed reads it from the stored body, never from this field.
	Visibility Visibility `json:"visibility,omitempty"`

	// Identity and authorization: the client supplies id, created and the signatures over the document,
	// at least one of which must be a curator-role signature from an owner (a Curators key when declared,
	// else any key). All three are part of the signed payload and are required (there is no API key).
	ID         *string              `json:"id,omitempty"`
	Created    *string              `json:"created,omitempty"`
	Signatures []playlist.Signature `json:"signatures,omitempty"`

	// Signature is the deprecated v1.0.x single signature. Accepted so a signed document carrying one is
	// not rejected as an unknown field; it is preserved verbatim in Raw.
	Signature string `json:"signature,omitempty"`

	// Raw is the request body exactly as received, set by the HTTP layer (never decoded from JSON).
	// The executor verifies, co-signs, and stores these bytes verbatim; the decoded fields above are
	// only read (identity projections, owner checks), never used to rebuild the document.
	Raw json.RawMessage `json:"-"`
}

// PlaylistReplaceRequest is the JSON body for PUT /api/v1/playlists/{id} (full replacement, same shape as create).
type PlaylistReplaceRequest = PlaylistCreateRequest

// errVisibilityNotString is the decode error for a visibility member that is not a JSON string.
var errVisibilityNotString = errors.New("visibility must be a string")

// Visibility is a playlist's `visibility` member as submitted: any JSON string, since the spec's
// vocabulary is open and the store fails closed on values it does not recognize (pg.listedPlaylistSQL).
//
// It is a type of its own, not a plain string, for one case: encoding/json decodes a JSON null into a
// string field as "" without error, so a plain field would accept `"visibility": null`. dp1-go's
// `Playlist.Visibility` is a `*string`, which reads null as absent, and only its playlists-extension
// schema types the member: with extensions enabled dp1-go refuses null too, but with them disabled this
// decoder is the only place null is refused before it is signed into storage. Numbers, booleans,
// objects and arrays were already refused by the string decode; null is the one this adds.
type Visibility string

// UnmarshalJSON accepts only a JSON string. encoding/json calls it for a present member, including null,
// and never for an absent one, so absence keeps meaning "public".
func (v *Visibility) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || data[0] != '"' {
		return errVisibilityNotString
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	*v = Visibility(s)
	return nil
}
