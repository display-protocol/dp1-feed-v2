package executor

// Signature-based authorization shared by the mutating executor methods. There is no API key: create is
// open (any validly self-signed document is accepted), while replace and delete are owner-bound — the
// request must carry a verifying signature from a key the *stored* document already names as its owner,
// and the owner set may not change on replace.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/display-protocol/dp1-go/extension/identity"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/google/uuid"

	"github.com/display-protocol/dp1-feed-v2/internal/models"
)

var (
	// ErrSignaturesRequired is returned when a mutating request carries no signatures. The HTTP layer
	// rejects such requests earlier (RequireSignatures middleware); this guards direct executor callers.
	ErrSignaturesRequired = errors.New("signatures are required")
	// ErrNotResourceOwner is returned when a signature verifies but its key is not an owner
	// (curator/publisher) of the stored resource, so it may not replace or delete it.
	ErrNotResourceOwner = errors.New("request is not signed by an owner of the resource")
	// ErrOwnerImmutable is returned when a replace would change the stored resource's owner set. Ownership
	// is fixed at creation: the owner may change anything else, but not who the owner is.
	ErrOwnerImmutable = errors.New("resource owner is immutable and cannot be changed")
	// ErrDeleteRequestInvalid is returned when a signed delete-intent is malformed or does not match the
	// resource it targets (wrong action, wrong target type, or id/slug that disagree with the stored row).
	ErrDeleteRequestInvalid = errors.New("invalid delete request")
)

// requireSignatures rejects a mutating request that carries no signatures.
func requireSignatures(sigs []playlist.Signature) error {
	if len(sigs) == 0 {
		return ErrSignaturesRequired
	}
	return nil
}

// entityKeySet collects the non-empty keys of a curator/publisher entity list.
func entityKeySet(entities []identity.Entity) map[string]struct{} {
	set := make(map[string]struct{}, len(entities))
	for _, e := range entities {
		if k := strings.TrimSpace(e.Key); k != "" {
			set[k] = struct{}{}
		}
	}
	return set
}

// stringOwnerKeySet wraps a single-key owner (playlist-group curator) as a key set, empty when blank.
func stringOwnerKeySet(key string) map[string]struct{} {
	set := make(map[string]struct{}, 1)
	if k := strings.TrimSpace(key); k != "" {
		set[k] = struct{}{}
	}
	return set
}

// requireStoredOwnerSignature enforces owner authority: at least one of the request's signatures must
// carry a kid that the stored resource names as an owner. Cryptographic verification of those signatures
// is done separately (verify*Signatures / VerifySignatures); this only checks authority.
func requireStoredOwnerSignature(ownerKeys map[string]struct{}, sigs []playlist.Signature) error {
	if len(ownerKeys) == 0 {
		// No owner key means nobody can authorize a mutation. This should not happen for documents created
		// through the signature path (create requires a valid owner signature), but guard it explicitly.
		return ErrNotResourceOwner
	}
	for _, s := range sigs {
		if _, ok := ownerKeys[s.Kid]; ok {
			return nil
		}
	}
	return ErrNotResourceOwner
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

// requireImmutableEntityOwner enforces that a replace keeps the same owner key set (playlist curators,
// channel publisher). Comparison is by key only; names/urls may change.
func requireImmutableEntityOwner(stored, incoming map[string]struct{}) error {
	if !sameKeySet(stored, incoming) {
		return ErrOwnerImmutable
	}
	return nil
}

// requireImmutableStringOwner enforces that a replace keeps the same single-key owner (playlist-group
// curator string).
func requireImmutableStringOwner(stored, incoming string) error {
	if strings.TrimSpace(stored) != strings.TrimSpace(incoming) {
		return ErrOwnerImmutable
	}
	return nil
}

func sameKeySet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// verifyDeleteIntent authorizes a DELETE. It checks the envelope (action + target identity), enforces the
// freshness window on "created" (replay bound), verifies the signatures cryptographically over the intent
// bytes, and finally requires a verifying signature from a stored owner key. ownerKeys are the stored
// resource's owner kids; storedID/storedSlug/targetType pin the intent to this exact resource.
func (e *impl) verifyDeleteIntent(req *models.SignedDeleteRequest, storedID uuid.UUID, storedSlug, targetType string, ownerKeys map[string]struct{}) error {
	if req == nil || len(req.Raw) == 0 {
		return fmt.Errorf("%w: missing signed body", ErrDeleteRequestInvalid)
	}
	if req.Action != models.DeleteAction {
		return fmt.Errorf("%w: action must be %q", ErrDeleteRequestInvalid, models.DeleteAction)
	}
	if req.Target.Type != targetType {
		return fmt.Errorf("%w: target type %q does not match %q", ErrDeleteRequestInvalid, req.Target.Type, targetType)
	}
	if strings.TrimSpace(req.Target.ID) != storedID.String() {
		return fmt.Errorf("%w: target id %q does not match stored id %q", ErrDeleteRequestInvalid, req.Target.ID, storedID)
	}
	if req.Target.Slug != storedSlug {
		return fmt.Errorf("%w: target slug %q does not match stored slug %q", ErrDeleteRequestInvalid, req.Target.Slug, storedSlug)
	}
	if err := e.requireFreshDeleteTimestamp(req.Created); err != nil {
		return err
	}
	if err := requireSignatures(req.Signatures); err != nil {
		return err
	}

	ok, failed, err := e.dp1.VerifySignatures(req.Raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSignatureVerificationFailed, err)
	}
	if !ok {
		failedKids := make([]string, 0, len(failed))
		for _, sig := range failed {
			failedKids = append(failedKids, sig.Kid)
		}
		return fmt.Errorf("%w: failed signatures: %v", ErrSignatureVerificationFailed, failedKids)
	}
	return requireStoredOwnerSignature(ownerKeys, req.Signatures)
}

// requireFreshDeleteTimestamp parses the intent's "created" and rejects it when it sits outside
// ±deleteSkew of now. Bounding it in both directions caps replay of a captured delete after the same id
// is re-created, and rejects clocks that are implausibly far ahead.
func (e *impl) requireFreshDeleteTimestamp(created string) error {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(created))
	if err != nil {
		return fmt.Errorf("%w: created must be RFC3339: %w", ErrInvalidTimestamp, err)
	}
	skew := e.deleteSkew
	if skew <= 0 {
		skew = defaultDeleteSkew
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

// IsForbiddenError reports whether err is an ownership-authorization failure (maps to 403 forbidden):
// either the signer is not an owner, or a replace tried to change the owner.
func IsForbiddenError(err error) bool {
	return err != nil && (errors.Is(err, ErrNotResourceOwner) || errors.Is(err, ErrOwnerImmutable))
}

// IsDeleteRequestError reports whether err is a malformed/mismatched delete-intent (maps to 400 bad_request).
func IsDeleteRequestError(err error) bool {
	return err != nil && errors.Is(err, ErrDeleteRequestInvalid)
}
