package models

import (
	"encoding/json"

	"github.com/display-protocol/dp1-go/playlist"
)

// A signed mutation intent is the feed-local envelope that authorizes a mutation of an existing
// resource. DP-1 defines no such document, so this shape is ours.
//
// Why it exists: an owner's signatures live *inside* the document and are public via GET, so an observer
// can replay a previously published document — PUT it to roll a resource back to an older version, or
// POST it after a delete to resurrect it. The per-signature `ts` cannot bound that replay because it is
// not covered by the signature: `sig` is computed over the JCS form of the document with `signatures`
// stripped, so `ts` can be rewritten on a replayed body without invalidating anything. Only a value that
// sits *inside* a signed payload is trustworthy, which is why the intent carries its own `created` and is
// signed as its own little document.
//
// Because the freshness bound is wall-clock rather than per-feed bookkeeping, a stale intent is stale on
// every feed — which is what makes this hold for documents mirrored across feeds.
const (
	// IntentActionReplace authorizes PUT.
	IntentActionReplace = "replace"
	// IntentActionDelete authorizes DELETE.
	IntentActionDelete = "delete"
)

// Target types enumerate the resource kinds an intent may name; each maps to one route family and is
// checked against the route the request arrived on, so an intent signed for one kind cannot act on another.
const (
	IntentTargetPlaylist      = "playlist"
	IntentTargetPlaylistGroup = "playlist-group"
	IntentTargetChannel       = "channel"
)

// IntentTarget binds an intent to exactly one stored resource. Both id and slug are checked against the
// stored row, so a signature made for one resource cannot act on another.
type IntentTarget struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

// SignedIntent authorizes a mutation. The executor verifies the signatures over Raw (§7.1 digest,
// `signatures` stripped), requires a signer whose kid is an owner of the *stored* resource, and rejects a
// `created` outside the configured freshness window.
//
// No `binding` tags: the intent is decoded with encoding/json so the exact bytes the signatures cover
// survive in Raw; gin's binding validation would not run. Validation lives in the executor
// (verifyIntent), which re-checks every field.
type SignedIntent struct {
	Action string       `json:"action"`
	Target IntentTarget `json:"target"`
	// PayloadHash binds a replace intent to the exact document it authorizes (the DP-1 signing digest of
	// the document bytes), so a captured intent cannot be reused to install different content. Required
	// for IntentActionReplace; absent for IntentActionDelete, which has no accompanying document.
	PayloadHash string               `json:"payloadHash,omitempty"`
	Created     string               `json:"created"`
	Signatures  []playlist.Signature `json:"signatures"`

	// Raw is the intent bytes exactly as received; the signatures are verified over these.
	Raw json.RawMessage `json:"-"`
}

// SignedDeleteRequest is the DELETE body: a bare signed intent with action "delete".
type SignedDeleteRequest = SignedIntent

// SignedReplaceRequest is the PUT body. It pairs the full signed document with the intent that authorizes
// replacing this resource with it. Both members are kept as raw bytes: the document bytes are what get
// verified, co-signed and stored verbatim, and the authorization bytes are what the intent signature
// covers — re-encoding either would change what was signed.
type SignedReplaceRequest struct {
	Document      json.RawMessage `json:"document"`
	Authorization json.RawMessage `json:"authorization"`
}
