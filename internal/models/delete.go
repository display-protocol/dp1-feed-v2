package models

import (
	"encoding/json"

	"github.com/display-protocol/dp1-go/playlist"
)

// DeleteAction is the only accepted value of SignedDeleteRequest.Action. It is part of the signed
// payload so a captured signature cannot be repurposed for a different operation.
const DeleteAction = "delete"

// DeleteTargetType enumerates the resource kinds a delete-intent may target; each maps to one route
// family and is checked against the route the request arrived on.
const (
	DeleteTargetPlaylist      = "playlist"
	DeleteTargetPlaylistGroup = "playlist-group"
	DeleteTargetChannel       = "channel"
)

// DeleteTarget binds a delete-intent to exactly one stored resource. Both id and slug are checked
// against the stored row so a signature made for one resource cannot delete another.
//
// No `binding` tags: the delete handler decodes with json.Unmarshal (to capture the exact bytes the
// signatures cover in Raw), which does not run gin's binding validation. The executor
// (verifyDeleteIntent) re-checks every field, so validation lives there, not in struct tags.
type DeleteTarget struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

// SignedDeleteRequest is the JSON body of a DELETE. DP-1 defines no delete document, so this is a
// feed-local envelope: a small object signed by an owner (curator/publisher) of the target resource.
// The executor verifies the signatures over these bytes (§7.1 digest, signatures stripped), requires a
// signer whose kid is an owner of the stored resource, and rejects a "created" outside the configured
// freshness window (replay bound). Raw carries the exact received bytes for verification and is never
// part of the wire schema.
type SignedDeleteRequest struct {
	Action     string               `json:"action"`
	Target     DeleteTarget         `json:"target"`
	Created    string               `json:"created"`
	Signatures []playlist.Signature `json:"signatures"`

	Raw json.RawMessage `json:"-"`
}
