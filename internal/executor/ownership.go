package executor

// Ownership: who may replace or delete a stored document, derived from the document itself.
//
// DP-1 has no signed "owner" field in core. What it has is a signature chain whose entries carry a
// `role`, and (in the playlists/channels extensions) optional entity fields — `curators[]` and
// `publisher` — whose `key`s are identity claims. The feed needs a concrete owner set to authorize PUT
// and DELETE, so it derives one with a single rule applied to every resource kind:
//
//   - Declared owners win. When the signed document declares owners (`curators[].key`,
//     `publisher.key`), those keys are the owner set. They sit inside the signed bytes, so a relayer
//     cannot change who owns the document without breaking every signature.
//   - Otherwise the signature chain defines them. The owner set is the kids of the signatures carrying
//     the resource's owner role (`curator` for playlists and groups, `publisher` for channels). This is
//     what lets core-only documents through, as the spec allows.
//
// Authority then requires a signature that is declared (kid in the owner set), proven (cryptographically
// verified by the caller) AND acting as owner (role equals the owner role). The role check is what stops
// a licensor, institution or agent key that happens to appear in `curators[]` from being treated as
// the author: the spec assigns authorship to the `curator` role signature, not to any signature.
//
// Known limit, accepted deliberately: `role` is NOT covered by the signature (sig is over the JCS
// document with `signatures` stripped), so the role check is a consistency rule, not a cryptographic
// boundary — a relayer can relabel any signature entry without breaking it. What the signature does
// bind is the content, including the declared owners. Two consequences follow:
//   - For a document with no declared owners, a relayer can strip or relabel entries before the feed
//     first sees it and thereby reshuffle authority among the actual co-signers. Declaring owners fixes
//     the SET OF KEYS inside the signed bytes, so that particular move is closed.
//   - For any document, a relayer can relabel an owner key's non-owner-role signature to the owner role,
//     so a declared owner key that signed only as licensor can be made to authorize (create, replace,
//     delete, or consent). Declaring owners does not prevent this.
//
// Nobody who did not sign the content gains authority either way. What the role rule buys is spec
// conformance — players MUST verify a `curator`/`publisher`-role signature, which a kid-only feed did not
// guarantee — and detection of honest mislabeling by client tooling. It does not defend against a
// relayer and cannot until DP-1 covers `role` in the signed bytes. Documented in docs/api_design.md and
// pinned by TestIntegration_Ownership_RelabeledRoleIsNotDetected.
//
// Ownership of a stored resource may grow on replace but never shrink: adding an owner is monotone
// (nobody loses authority), removing one is an eviction primitive a co-owner could turn against the
// others. Every key entering the owner set on replace must itself sign the document in the owner role —
// that proves key possession and consent to being listed, and stops an owner from attributing the
// document to an arbitrary public key. Self-removal via the intent is the natural future extension and
// is not built.
//
// Interim assumption — create is deliberately less strict than replace about consent. A POST that
// declares curators [A, B] signed only by A is accepted, and B owns without having signed; the same
// document arriving as a PUT to add B is refused. The asymmetry is accepted because create is open and
// trusts the document's own claims (the spec calls entity keys identity claims), whereas a replace is the
// feed enforcing a change to an authorization state it already guards, and additions are permanent.
// Requiring every declared owner to sign on create would turn multi-curator publishing into an N-of-N
// ceremony before a document can exist at all. Revisit if attribution-without-consent on create turns
// out to matter in practice.
//
// Consequence of deriving owners from the stored bytes: when a document declares no owners, the incoming
// owner set on replace is exactly its owner-role signers, so every stored co-owner must re-sign every
// PUT (N-of-N), not just one. There is no side table remembering the set. Authors who want any-one-owner
// edits should declare `curators`/`publisher` — but the PUT that first declares them moves the resource
// from that N-of-N regime to any-one authorization, so it needs a signature from EVERY current owner
// (requireRegimeTransitionConsent); otherwise one co-owner could declare the set and then edit alone.

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/display-protocol/dp1-go/extension/identity"
	"github.com/display-protocol/dp1-go/playlist"
)

var (
	// ErrNotResourceOwner is returned when no verifying signature comes from a stored owner acting in the
	// owner role, so the request may not replace or delete the resource.
	ErrNotResourceOwner = errors.New("request is not signed by an owner of the resource")
	// ErrOwnerRemoved is returned when a replace would drop a key from the stored owner set. Owners may be
	// added, never removed: removal would let one co-owner evict another.
	ErrOwnerRemoved = errors.New("owners cannot be removed from a resource")
	// ErrOwnerConsentRequired is returned when a replace adds an owner whose key did not sign the incoming
	// document in the owner role. Without that signature the feed would co-sign and serve an attribution
	// the named key never agreed to, and could not even tell the key exists.
	ErrOwnerConsentRequired = errors.New("a new owner must sign the document in the owner role")
)

// keySet is a set of signing-key identifiers (did:key / did:pkh kids).
type keySet map[string]struct{}

// entityKeySet collects the non-empty keys of a curator/publisher entity list.
func entityKeySet(entities []identity.Entity) keySet {
	set := make(keySet, len(entities))
	for _, e := range entities {
		if k := strings.TrimSpace(e.Key); k != "" {
			set[k] = struct{}{}
		}
	}
	return set
}

// publisherKeySet wraps a channel's optional publisher as a declared-owner set (empty when absent/blank).
func publisherKeySet(publisher *identity.Entity) keySet {
	set := make(keySet, 1)
	if publisher == nil {
		return set
	}
	if k := strings.TrimSpace(publisher.Key); k != "" {
		set[k] = struct{}{}
	}
	return set
}

// ownerSet derives a document's owner set. declared are the keys the signed document names as owners
// (empty when it names none); when non-empty they are the answer and the signature chain cannot widen
// them. Otherwise the owners are the kids of the signatures carrying ownerRole.
//
// The signatures are used as identity only; the caller is responsible for having verified them before
// the result is trusted for anything. sigs may include the feed's own entry (role "feed"), which never
// counts.
func ownerSet(declared keySet, ownerRole string, sigs []playlist.Signature) keySet {
	if len(declared) > 0 {
		return declared
	}
	set := make(keySet, len(sigs))
	for _, s := range sigs {
		if s.Role != ownerRole {
			continue
		}
		if k := strings.TrimSpace(s.Kid); k != "" {
			set[k] = struct{}{}
		}
	}
	return set
}

// requireOwnerSignature enforces authority: at least one signature must carry a kid in owners AND the
// owner role. Cryptographic verification of sigs is the caller's job (every mutating path verifies all
// signatures before or right after this check); this only decides who is acting.
//
// missing is the sentinel wrapped on failure so each path keeps its own contract: a create reports the
// document as not validly self-signed (400), a replace/delete reports the signer as not an owner (403).
// When an owner key did sign but under another role, the message says so — that is the case a client
// is most likely to hit by accident, and the fix (sign as curator/publisher) is not obvious from a bare
// "not an owner".
func requireOwnerSignature(owners keySet, ownerRole string, sigs []playlist.Signature, missing error) error {
	if len(owners) == 0 {
		// No owner key means nobody can authorize. On create this is the routine case of a document that
		// declares no owner and carries no owner-role signature, so the message says what to sign as. On
		// replace/delete it means the stored document has no owner (its owner signature predates role-aware
		// ownership and carries a non-owner role), and no request can ever authorize a mutation of it.
		return fmt.Errorf("%w: the resource has no owner (no declared owner key and no %q-role signature)", missing, ownerRole)
	}
	var wrongRole []string
	for _, s := range sigs {
		if _, ok := owners[strings.TrimSpace(s.Kid)]; !ok {
			continue
		}
		if s.Role == ownerRole {
			return nil
		}
		wrongRole = append(wrongRole, fmt.Sprintf("%s signed as %q", s.Kid, s.Role))
	}
	if len(wrongRole) > 0 {
		return fmt.Errorf("%w: an owner key signed with a non-owner role (%s); the %q role is required", missing, strings.Join(wrongRole, ", "), ownerRole)
	}
	return missing
}

// requireDeclarationRetained enforces that a document which declares its owners keeps declaring them:
// once `curators`/`publisher` is present in the stored document, a replacement may not omit it. Dropping
// the declaration would move the document from the signed-owner regime to the label-derived one (see the
// package comment), and for a channel it would let a single declared publisher silently become
// publisher-less and then multi-owner. Which keys the declaration must contain is requireOwnersRetained's
// job; this only guards the presence of the declaration.
func requireDeclarationRetained(storedDeclared, incomingDeclared keySet) error {
	if len(storedDeclared) > 0 && len(incomingDeclared) == 0 {
		return fmt.Errorf("%w: the stored document declares its owners, so the replacement must declare them too", ErrOwnerRemoved)
	}
	return nil
}

// requireOwnersRetained enforces the no-eviction half of the replace rule: every stored owner must still
// be in the incoming owner set. Runs before signature verification because it needs none.
func requireOwnersRetained(stored, incoming keySet) error {
	var removed []string
	for k := range stored {
		if _, ok := incoming[k]; !ok {
			removed = append(removed, k)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	sort.Strings(removed)
	return fmt.Errorf("%w: %s", ErrOwnerRemoved, strings.Join(removed, ", "))
}

// requireRegimeTransitionConsent guards the one owner-set change that would otherwise let a co-owner
// strip another's veto. A document with no declared owners is authorized N-of-N: its owner set is derived
// from the owner-role signers, so requireOwnersRetained forces every stored owner to sign every replace
// (see the package comment). A document that declares its owners is authorized any-one-of-N. Turning the
// first into the second (stored undeclared, incoming declared) drops every other owner's required
// signature, so the transition must carry an owner-role signature from EVERY stored owner, proving the
// whole current owner set consents to the weaker regime. Without this a single co-owner could declare the
// existing owner set and thereafter edit alone.
//
// New owners added in the same replace are requireNewOwnerConsent's job; this covers the keys that were
// already owners. A non-transition — declared->declared, undeclared->undeclared, or the forbidden
// declared->undeclared (see requireDeclarationRetained) — is a no-op here. storedOwners is the derived
// stored owner set; sigs must already have been cryptographically verified.
func requireRegimeTransitionConsent(storedDeclared, incomingDeclared, storedOwners keySet, ownerRole string, sigs []playlist.Signature) error {
	if len(storedDeclared) > 0 || len(incomingDeclared) == 0 {
		return nil
	}
	signedAsOwner := ownerSet(nil, ownerRole, sigs)
	var missing []string
	for k := range storedOwners {
		if _, ok := signedAsOwner[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("%w: declaring owners on a resource that had none changes it from unanimous to any-one authorization, so every current owner must sign as %q; missing: %s", ErrOwnerConsentRequired, ownerRole, strings.Join(missing, ", "))
}

// requireNewOwnerConsent enforces the consent half of the replace rule: every key in incoming that is
// not in stored must have signed the incoming document in the owner role. sigs must already have been
// cryptographically verified; presence alone is what is checked here.
func requireNewOwnerConsent(stored, incoming keySet, ownerRole string, sigs []playlist.Signature) error {
	signedAsOwner := ownerSet(nil, ownerRole, sigs)
	var unsigned []string
	for k := range incoming {
		if _, was := stored[k]; was {
			continue
		}
		if _, ok := signedAsOwner[k]; !ok {
			unsigned = append(unsigned, k)
		}
	}
	if len(unsigned) == 0 {
		return nil
	}
	sort.Strings(unsigned)
	return fmt.Errorf("%w: %s", ErrOwnerConsentRequired, strings.Join(unsigned, ", "))
}

// IsForbiddenError reports whether err is an ownership-authorization failure (maps to 403 forbidden):
// the signer is not an owner acting as owner, a replace tried to remove an owner, or it added one
// without that key's consent signature.
func IsForbiddenError(err error) bool {
	return err != nil && (errors.Is(err, ErrNotResourceOwner) ||
		errors.Is(err, ErrOwnerRemoved) ||
		errors.Is(err, ErrOwnerConsentRequired))
}
