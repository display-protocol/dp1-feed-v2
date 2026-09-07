package executor

// Signature-based authorization shared by the mutating executor methods. There is no API key: create is
// open (any document validly signed by its owner is accepted), while replace and delete are owner-bound —
// the request must carry a verifying signature from a key the *stored* document already names as an
// owner, acting in the owner role. How the owner set is derived, and how it may change on replace, lives
// in ownership.go; this file holds the identity checks and the signed mutation-intent.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/display-protocol/dp1-go/playlist"
	"github.com/google/uuid"

	"github.com/display-protocol/dp1-feed-v2/internal/fetcher"
	"github.com/display-protocol/dp1-feed-v2/internal/models"
)

var (
	// ErrSignaturesRequired is returned when a mutating request carries no signatures. The HTTP layer
	// rejects such requests earlier (RequireSignatures middleware); this guards direct executor callers.
	ErrSignaturesRequired = errors.New("signatures are required")
	// ErrIntentInvalid is returned when a signed mutation intent is malformed or does not match what it
	// claims to authorize: wrong action, wrong target type, id/slug that disagree with the stored row, or
	// (for a replace) a payloadHash that is not the digest of the submitted document.
	ErrIntentInvalid = errors.New("invalid mutation intent")
	// ErrSlugRequired is returned when a create omits slug. The client signs over the document's slug, so
	// the feed cannot derive or normalize one after signing without invalidating the signature.
	ErrSlugRequired = errors.New("slug is required")
	// ErrItemIDRequired is returned when a playlist item lacks a UUID id. The feed used to assign missing
	// ids, but that would rewrite the document after the client signed it; the client must supply them.
	ErrItemIDRequired = errors.New("every playlist item requires a UUID id")
	// ErrSignedDocumentMismatch is returned when a PUT's document id, slug, or created disagree with the
	// stored resource. Identity is immutable and is validated, never silently replaced: substituting stored
	// values would change bytes the client signed and orphan the signature.
	ErrSignedDocumentMismatch = errors.New("signed document does not match the stored resource")
	// ErrTooManyReferences is returned when a group or channel lists more playlist URIs than the feed will
	// resolve for one request. This is a fan-out bound rather than a schema rule: each unstored reference
	// becomes an outbound fetch, and creation is open, so the count has to be capped before resolution
	// begins. The client chose the list, so it is a 400.
	ErrTooManyReferences = errors.New("too many playlist references")
	// ErrResolvedTooLarge is returned when the playlists a group or channel references add up to more bytes
	// than one request may hold in memory. Distinct from ErrTooManyReferences: the count can be well within
	// the cap while the documents behind it are not, which is the combination that made the reference cap
	// alone an insufficient bound. The client chose the references, so it is a 400.
	ErrResolvedTooLarge = errors.New("referenced playlists are too large to resolve in one request")
	// ErrPlaylistURITooLong is returned when a group or channel reference exceeds maxPlaylistURILen. The
	// document sets the URI, so this is client input being wrong: 400.
	ErrPlaylistURITooLong = errors.New("playlist reference URI is too long")
	// ErrNoPlaylistReferences is returned when a group or channel document lists no playlists. The document
	// is the client's, so this is a correctable submission error (400) rather than an internal fault; it
	// previously fell through the error mapper as an unclassified 500.
	ErrNoPlaylistReferences = errors.New("group or channel references no playlists")
	// errMissingRawBody means the HTTP layer did not attach the request bytes to a submission (programming error).
	errMissingRawBody = errors.New("signed submission is missing the raw request body")
)

// signedIdentity is the resource identity a client-signed document asserts about itself. All three
// values are read from the document and used as row projections; none is ever written back into it.
type signedIdentity struct {
	id      uuid.UUID
	slug    string
	created time.Time
}

// newSignedIdentity validates the identity fields of a signed submission. slug is taken verbatim
// (no slugify): normalizing it would change the signed bytes.
func newSignedIdentity(idStr, createdStr *string, slug string, raw json.RawMessage) (signedIdentity, error) {
	if len(raw) == 0 {
		return signedIdentity{}, errMissingRawBody
	}
	id, err := parseUserProvidedID(idStr)
	if err != nil {
		return signedIdentity{}, err
	}
	created, err := parseUserProvidedCreated(createdStr)
	if err != nil {
		return signedIdentity{}, err
	}
	verbatim, err := requireSlug(slug)
	if err != nil {
		return signedIdentity{}, err
	}
	return signedIdentity{id: id, slug: verbatim, created: created}, nil
}

// mustMatchStored enforces that a PUT replaces the resource it targets: the document's id and slug must
// equal the stored row's, and created must denote the same instant as the stored document's (compared as
// times, since the two may be formatted differently). Changing identity means a new document.
func (si signedIdentity) mustMatchStored(rowID uuid.UUID, rowSlug, storedCreated string) error {
	if si.id != rowID {
		return fmt.Errorf("%w: id %q does not match stored id %q", ErrSignedDocumentMismatch, si.id, rowID)
	}
	if si.slug != rowSlug {
		return fmt.Errorf("%w: slug %q does not match stored slug %q", ErrSignedDocumentMismatch, si.slug, rowSlug)
	}
	stored, err := parseDocumentCreated(storedCreated)
	if err != nil {
		return err
	}
	if !si.created.Equal(stored) {
		return fmt.Errorf("%w: created %q does not match stored created %q", ErrSignedDocumentMismatch, si.created.Format(time.RFC3339Nano), storedCreated)
	}
	return nil
}

// requireSlug returns the client-provided slug verbatim (only whitespace-emptiness is rejected). It is
// deliberately not slugified: normalizing after the client has signed would change the signed bytes.
func requireSlug(slug string) (string, error) {
	if strings.TrimSpace(slug) == "" {
		return "", ErrSlugRequired
	}
	return slug, nil
}

// requireItemIDs rejects any playlist item without a parseable UUID id, so the feed never has to mint one
// after signing (which would orphan the client's signature).
func requireItemIDs(items []playlist.PlaylistItem) error {
	for i := range items {
		if _, err := uuid.Parse(strings.TrimSpace(items[i].ID)); err != nil {
			return fmt.Errorf("%w: items[%d]", ErrItemIDRequired, i)
		}
	}
	return nil
}

// requireSignatures rejects a mutating request that carries no signatures.
func requireSignatures(sigs []playlist.Signature) error {
	if len(sigs) == 0 {
		return ErrSignaturesRequired
	}
	return nil
}

// signatureFailure wraps a set of signature entries that failed cryptographic verification as
// ErrSignatureVerificationFailed, listing their kids for diagnostics.
func signatureFailure(failed []playlist.Signature) error {
	kids := make([]string, 0, len(failed))
	for _, s := range failed {
		kids = append(kids, s.Kid)
	}
	return fmt.Errorf("%w: failed signatures: %v", ErrSignatureVerificationFailed, kids)
}

// verifyIntent authorizes a mutation of an existing resource against a signed intent.
//
// The intent is a document in its own right, so its `created` sits inside the bytes the signature covers
// and cannot be rewritten on a replayed request — unlike the per-signature `ts`, which is not part of the
// signing digest. That is what bounds replay here, and because the bound is wall-clock rather than
// per-feed state, a stale intent is stale on every feed that mirrors the document.
//
// wantAction/targetType/storedID/storedSlug pin the intent to this exact operation and resource.
// docRaw is the document the intent authorizes (replace) or nil (delete): when non-nil the intent must
// name that document's signing digest in payloadHash, so a captured intent cannot install other content.
// owners are the *stored* resource's owner kids and ownerRole the role they must sign with.
//
// Checks run cheapest-first, and every failure mode is distinguishable by the caller: envelope mismatch
// (400), stale/absent freshness (400), signature failure (400), non-owner signer (403).
func (e *impl) verifyIntent(
	intent *models.SignedIntent,
	wantAction, targetType string,
	storedID uuid.UUID,
	storedSlug string,
	owners keySet,
	ownerRole string,
	docRaw json.RawMessage,
) error {
	if intent == nil || len(intent.Raw) == 0 {
		return fmt.Errorf("%w: missing signed authorization", ErrIntentInvalid)
	}
	if intent.Action != wantAction {
		return fmt.Errorf("%w: action must be %q", ErrIntentInvalid, wantAction)
	}
	if intent.Target.Type != targetType {
		return fmt.Errorf("%w: target type %q does not match %q", ErrIntentInvalid, intent.Target.Type, targetType)
	}
	if strings.TrimSpace(intent.Target.ID) != storedID.String() {
		return fmt.Errorf("%w: target id %q does not match stored id %q", ErrIntentInvalid, intent.Target.ID, storedID)
	}
	if intent.Target.Slug != storedSlug {
		return fmt.Errorf("%w: target slug %q does not match stored slug %q", ErrIntentInvalid, intent.Target.Slug, storedSlug)
	}
	if err := e.requireIntentBindsDocument(intent, docRaw); err != nil {
		return err
	}
	if err := e.requireFreshIntentTimestamp(intent.Created); err != nil {
		return err
	}
	if err := requireSignatures(intent.Signatures); err != nil {
		return err
	}

	ok, failed, err := e.dp1.VerifySignatures(intent.Raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSignatureVerificationFailed, err)
	}
	if !ok {
		return signatureFailure(failed)
	}
	return requireOwnerSignature(owners, ownerRole, intent.Signatures, ErrNotResourceOwner)
}

// requireIntentBindsDocument ties a replace intent to the exact document it authorizes. Without it a
// captured intent for one document would authorize installing any other content the owner ever signed.
// A delete intent has no document, and must not claim to bind one.
func (e *impl) requireIntentBindsDocument(intent *models.SignedIntent, docRaw json.RawMessage) error {
	if len(docRaw) == 0 {
		// Reject the member's PRESENCE, not just a non-empty value. The route's delete schema sets
		// additionalProperties:false and does not list payloadHash, so any occurrence is off-contract — but
		// null, "" and "   " all decode to the empty string, so a value check alone silently accepts three
		// spellings the spec forbids. Presence is read from the raw body because that is the only place
		// the distinction between "absent" and "explicitly null" survives decoding.
		if intentDeclaresPayloadHash(intent) {
			return fmt.Errorf("%w: payloadHash is not accepted for this action", ErrIntentInvalid)
		}
		return nil
	}
	want, err := e.dp1.PayloadHash(docRaw)
	if err != nil {
		return fmt.Errorf("%w: cannot hash the submitted document: %w", ErrIntentInvalid, err)
	}
	if !strings.EqualFold(strings.TrimSpace(intent.PayloadHash), want) {
		return fmt.Errorf("%w: payloadHash %q does not match the submitted document (%s)", ErrIntentInvalid, intent.PayloadHash, want)
	}
	return nil
}

// intentDeclaresPayloadHash reports whether the intent body carried a payloadHash member at all,
// regardless of its value.
//
// Raw is the authority when present; the decoded string cannot distinguish an absent member from an
// explicit null. Callers that build a SignedIntent in-process (tests, internal paths) leave Raw empty, so
// the decoded value is the fallback. A body that fails to re-decode here is treated as declaring the
// member: this runs only on the reject path, and refusing an unparseable intent is the safe direction.
func intentDeclaresPayloadHash(intent *models.SignedIntent) bool {
	if len(intent.Raw) == 0 {
		return strings.TrimSpace(intent.PayloadHash) != ""
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(intent.Raw, &members); err != nil {
		return true
	}
	_, declared := members["payloadHash"]
	return declared
}

// requireFreshIntentTimestamp parses the intent's "created" and rejects it when it sits outside
// ±intentSkew of now. Bounding it in both directions caps replay of a captured intent and rejects clocks
// that are implausibly far ahead. The same window governs replace and delete.
func (e *impl) requireFreshIntentTimestamp(created string) error {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(created))
	if err != nil {
		return fmt.Errorf("%w: created must be RFC3339: %w", ErrInvalidTimestamp, err)
	}
	skew := e.intentSkew
	if skew <= 0 {
		skew = defaultIntentSkew
	}
	now := time.Now()
	if t.Before(now.Add(-skew)) || t.After(now.Add(skew)) {
		return fmt.Errorf("%w: created is outside the allowed %s window", ErrInvalidTimestamp, skew)
	}
	return nil
}

// IsSignaturesRequiredError reports whether err is ErrSignaturesRequired (maps to 401 unauthorized).
func IsSignaturesRequiredError(err error) bool {
	return err != nil && errors.Is(err, ErrSignaturesRequired)
}

// IsIntentError reports whether err is a malformed/mismatched signed mutation intent (400 bad_request).
func IsIntentError(err error) bool {
	return err != nil && errors.Is(err, ErrIntentInvalid)
}

// IsBlockedFetchDestinationError reports whether err is a refused playlist-fetch destination. The
// requester chose the URL, so this is their input being wrong rather than an internal fault: 400.
func IsBlockedFetchDestinationError(err error) bool {
	return err != nil && errors.Is(err, fetcher.ErrBlockedDestination)
}

// IsInvalidSubmissionError reports whether err is a client-correctable defect in a signed create/replace
// submission (missing slug, an item without a UUID id, too many or oversized references, an undeclared
// document with more than one owner-role signature, or a PUT whose document identity does not match the
// stored resource). Maps to 400.
func IsInvalidSubmissionError(err error) bool {
	return err != nil && (errors.Is(err, ErrAmbiguousOwner) ||
		errors.Is(err, ErrGroupCuratorMismatch) ||
		errors.Is(err, ErrSlugRequired) ||
		errors.Is(err, ErrItemIDRequired) ||
		errors.Is(err, ErrTooManyReferences) ||
		errors.Is(err, ErrResolvedTooLarge) ||
		errors.Is(err, ErrPlaylistURITooLong) ||
		errors.Is(err, ErrNoPlaylistReferences) ||
		errors.Is(err, ErrSignedDocumentMismatch))
}
