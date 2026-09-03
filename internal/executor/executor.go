// Package executor contains feed business logic: validation, signing, persistence, and transactional
// ingest of referenced playlists when creating or updating playlist-groups and channels.
// Playlist URI resolution (local API vs HTTP fetch, ordering) lives in ingest_resolve.go.
package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	dp1 "github.com/display-protocol/dp1-go"
	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/extension/identity"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/playlistgroup"
	"github.com/display-protocol/dp1-go/sign"
	"github.com/google/uuid"

	"github.com/display-protocol/dp1-feed-v2/internal/dp1svc"
	"github.com/display-protocol/dp1-feed-v2/internal/fetcher"
	"github.com/display-protocol/dp1-feed-v2/internal/models"
	"github.com/display-protocol/dp1-feed-v2/internal/notification"
	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// Executor is the feed business logic surface used by HTTP handlers (mock in tests via this interface).
//
// Gomock: generated type mocks.MockExecutor in internal/mocks/executor_mock.go.
// Regenerate all mocks from repository root: go generate ./...
// (directives in internal/mocks/doc.go; uses go tool mockgen from go.mod tools.)
//
// Document methods return store records: Raw is the persisted JSON and is what the HTTP layer writes to
// the wire, verbatim; Body is the decoded view for callers that need fields. Records returned by
// Create/Replace carry ID, Slug, Raw and Body only (timestamps are populated by reads).
//
// The feed never rebuilds a submitted document. DP-1 §7.1 binds every signature to the JCS form of the
// entire document, so the executor verifies the client's signatures over req.Raw, appends its own feed
// signature to those same bytes, validates, and stores them unchanged — it never derives a slug, mints
// item ids, re-formats created, injects a version default, or strips a legacy "signature".
//
// Authorization model (there is no API key): every mutating method is authorized by cryptographic
// signatures, never a shared secret.
//   - Create is open: any client may submit a document validly self-signed by its own declared
//     curator/publisher. The signer becomes the resource's owner.
//   - Replace and Delete are owner-bound: the request must carry a verifying signature whose kid is an
//     owner (curator/publisher) of the *stored* resource, and the owner set is immutable — a Replace
//     may not change curators/publisher. Any edit re-derives the document bytes, so the owner re-signs
//     and the feed co-signs.
//
// See internal/executor/signed_auth.go for the shared owner checks and the signed delete-intent.
type Executor interface {
	// CreatePlaylist verifies the client's curator signatures, feed co-signs, validates, and stores a new playlist.
	CreatePlaylist(ctx context.Context, req *models.PlaylistCreateRequest) (*store.PlaylistRecord, error)
	// GetPlaylist returns the stored playlist document for id or slug (HTTP layer JSON-encodes the response).
	GetPlaylist(ctx context.Context, idOrSlug string) (*store.PlaylistRecord, error)
	// ListPlaylists returns one page of playlist bodies and an optional next cursor (optional channel or playlist-group filter; id or slug).
	ListPlaylists(ctx context.Context, limit int, cursor string, sort store.SortOrder, channelFilter, playlistGroupFilter string) ([]store.PlaylistRecord, string, error)
	// ReplacePlaylist performs a full PUT (owner-bound, owner immutable): verify owner signature, feed co-sign, validate, update.
	ReplacePlaylist(ctx context.Context, idOrSlug string, req *models.PlaylistReplaceRequest, intent *models.SignedIntent) (*store.PlaylistRecord, error)
	// DeletePlaylist verifies the signed delete-intent against the stored owner keys, then removes the playlist row.
	DeletePlaylist(ctx context.Context, idOrSlug string, req *models.SignedDeleteRequest) error

	// ListPlaylistItems returns one page of stored playlist items from the item index (OpenAPI GET /playlist-items).
	ListPlaylistItems(ctx context.Context, limit int, cursor string, sort store.SortOrder, channelFilter, playlistGroupFilter string) ([]playlist.PlaylistItem, string, error)
	// GetPlaylistItem returns a single indexed playlist item by UUID (OpenAPI GET /playlist-items/{id}).
	GetPlaylistItem(ctx context.Context, itemID uuid.UUID) (*playlist.PlaylistItem, error)

	// CreatePlaylistGroup resolves each playlist URI (parallel fetch or local GET), then signs the group and commits group + upserted playlists + membership in one transaction.
	CreatePlaylistGroup(ctx context.Context, req *models.PlaylistGroupCreateRequest) (*store.PlaylistGroupRecord, error)
	// GetPlaylistGroup returns the stored playlist-group document for id or slug (HTTP layer JSON-encodes).
	GetPlaylistGroup(ctx context.Context, idOrSlug string) (*store.PlaylistGroupRecord, error)
	// ListPlaylistGroups returns one page of playlist-group bodies.
	ListPlaylistGroups(ctx context.Context, limit int, cursor string, sort store.SortOrder) ([]store.PlaylistGroupRecord, string, error)
	// ReplacePlaylistGroup re-resolves playlist URIs, verifies the owner signature, re-signs, and commits updates in one transaction.
	ReplacePlaylistGroup(ctx context.Context, idOrSlug string, req *models.PlaylistGroupReplaceRequest, intent *models.SignedIntent) (*store.PlaylistGroupRecord, error)
	// DeletePlaylistGroup verifies the signed delete-intent against the stored curator, then removes the playlist-group row (membership CASCADE).
	DeletePlaylistGroup(ctx context.Context, idOrSlug string, req *models.SignedDeleteRequest) error

	// CreateChannel resolves playlist URIs, signs the channel document, and commits channel + playlists + membership in one transaction (requires extensions).
	CreateChannel(ctx context.Context, req *models.ChannelCreateRequest) (*store.ChannelRecord, error)
	// GetChannel returns the stored channel document for id or slug (HTTP layer JSON-encodes).
	GetChannel(ctx context.Context, idOrSlug string) (*store.ChannelRecord, error)
	// ListChannels returns one page of channel bodies.
	ListChannels(ctx context.Context, limit int, cursor string, sort store.SortOrder) ([]store.ChannelRecord, string, error)
	// ReplaceChannel re-resolves playlist URIs, verifies the owner (publisher) signature, re-signs, and commits updates in one transaction.
	ReplaceChannel(ctx context.Context, idOrSlug string, req *models.ChannelReplaceRequest, intent *models.SignedIntent) (*store.ChannelRecord, error)
	// DeleteChannel verifies the signed delete-intent against the stored publisher, then removes the channel row (membership CASCADE).
	DeleteChannel(ctx context.Context, idOrSlug string, req *models.SignedDeleteRequest) error

	// GetChannelRegistry returns the curated channel registry as ordered publisher items.
	GetChannelRegistry(ctx context.Context) ([]store.RegistryPublisher, []store.RegistryPublisherChannel, error)

	// APIInfo returns deployment metadata for GET /api/v1.
	APIInfo(version string) map[string]any
}

// impl is the concrete Executor: coordinates store, dp1-go validation/signing, optional HTTP fetch, and publicBaseURL for local playlist URLs.
type impl struct {
	store              store.Store
	dp1                dp1svc.ValidatorSigner
	extensionsEnabled  bool
	fetch              fetcher.Fetcher
	publicBase         string
	notificationClient notification.Client
	intentSkew         time.Duration
}

// Option configures optional executor side-effect boundaries.
type Option func(*impl)

// WithNotificationClient registers the client notified after successful channel mutations.
func WithNotificationClient(client notification.Client) Option {
	return func(e *impl) {
		e.notificationClient = client
	}
}

// WithIntentClockSkew sets the signed delete-intent freshness window. A non-positive value leaves the
// executor default (defaultIntentSkew) in place.
func WithIntentClockSkew(d time.Duration) Option {
	return func(e *impl) {
		if d > 0 {
			e.intentSkew = d
		}
	}
}

// New constructs an Executor. If extensionsEnabled is true, playlist validation and channel APIs use registry/extension rules.
// fetch may be nil; external playlist URLs in groups/channels then fail unless they match publicBaseURL as local /api/v1/playlists/{idOrSlug}.
func New(st store.Store, dp dp1svc.ValidatorSigner, extensionsEnabled bool, fetch fetcher.Fetcher, publicBaseURL string, options ...Option) Executor {
	e := &impl{
		store:             st,
		dp1:               dp,
		extensionsEnabled: extensionsEnabled,
		fetch:             fetch,
		publicBase:        strings.TrimSpace(publicBaseURL),
		intentSkew:        defaultIntentSkew,
	}
	for _, option := range options {
		option(e)
	}
	return e
}

// defaultIntentSkew is the signed delete-intent freshness window when WithIntentClockSkew is not set.
// Kept small to bound replay of a captured delete after the same id is re-created.
const defaultIntentSkew = 5 * time.Minute

func (e *impl) runChannelMutation(ctx context.Context, mutate func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.notificationClient == nil {
		return mutate(ctx)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return fmt.Errorf("notified channel mutation requires a request deadline")
	}

	// Once final persistence begins, a client disconnect must not create an
	// ambiguous committed-without-notification outcome. Preserve request values
	// while keeping the one deadline established at request entry.
	base := context.WithoutCancel(ctx)
	mutationCtx, cancel := context.WithDeadline(base, deadline)
	defer cancel()
	return mutate(mutationCtx)
}

func (e *impl) notifyChannel(ctx context.Context, eventType notification.EventType, id uuid.UUID) {
	if e.notificationClient == nil {
		return
	}
	// Persistence has already committed, so delivery must not disappear merely
	// because the caller disconnected. Keep the request-scoped deadline so
	// delivery consumes only the remaining end-to-end budget.
	deliveryCtx := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		deliveryCtx, cancel = context.WithDeadline(deliveryCtx, deadline)
		defer cancel()
	}
	_ = e.notificationClient.Notify(deliveryCtx, notification.Event{
		Type: eventType,
		Time: time.Now().UTC(),
		Channel: notification.ChannelRef{
			URL: strings.TrimRight(e.publicBase, "/") + "/api/v1/channels/" + id.String(),
		},
	})
}

// ErrExtensionsDisabled is returned for channel APIs when the deployment has extensions disabled.
var ErrExtensionsDisabled = errors.New("extensions disabled")

// Trusted model errors: returned when client signature verification fails.
var (
	// ErrInvalidTimestamp is returned when user-provided created timestamp is in the future.
	ErrInvalidTimestamp = errors.New("invalid timestamp: cannot be in the future")
	// ErrInvalidID is returned when user-provided id is not a valid UUID.
	ErrInvalidID = errors.New("invalid id: must be a valid UUID")
	// ErrSignatureVerificationFailed is returned when signature cryptographic verification fails.
	ErrSignatureVerificationFailed = errors.New("signature verification failed")
	// ErrNoValidCuratorSignature is returned when playlist/group has no signature matching curators[].
	ErrNoValidCuratorSignature = errors.New("no valid curator signature found")
	// ErrNoValidPublisherSignature is returned when channel has no signature matching publisher.
	ErrNoValidPublisherSignature = errors.New("no valid publisher signature found")
)

// CreatePlaylist verifies the client's curator signatures over the received bytes, appends the feed
// signature to those same bytes, validates, and stores them verbatim.
//
// Create is open: any client may create a document validly self-signed by a key it declares in
// curators[]. id, created, slug and signatures[] are required and are stored exactly as submitted.
func (e *impl) CreatePlaylist(ctx context.Context, req *models.PlaylistCreateRequest) (*store.PlaylistRecord, error) {
	if err := requireSignatures(req.Signatures); err != nil {
		return nil, err
	}
	si, err := newSignedIdentity(req.ID, req.Created, req.Slug, req.Raw)
	if err != nil {
		return nil, err
	}
	if err := requireItemIDs(req.Items); err != nil {
		return nil, err
	}
	if err := e.verifyPlaylistCuratorSignatures(req.Raw, req.Signatures, req.Curators); err != nil {
		return nil, fmt.Errorf("curator signature verification: %w", err)
	}

	signed, pl, err := e.signAndValidatePlaylist(req.Raw, si.created)
	if err != nil {
		return nil, err
	}
	if err := e.store.CreatePlaylist(ctx, si.id, si.slug, signed); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return &store.PlaylistRecord{ID: si.id, Slug: si.slug, Raw: signed, Body: *pl}, nil
}

// signAndValidatePlaylist appends the feed signature to raw and validates the result (core or
// core+extension). Only the "signatures" array changes; every other byte stays the client's.
func (e *impl) signAndValidatePlaylist(raw []byte, ts time.Time) ([]byte, *playlist.Playlist, error) {
	signed, err := e.dp1.SignPlaylist(raw, ts)
	if err != nil {
		return nil, nil, fmt.Errorf("feed sign: %w", err)
	}
	pl, err := e.parseValidatedPlaylist(signed)
	if err != nil {
		return nil, nil, fmt.Errorf("post-sign validation: %w", err)
	}
	if pl == nil {
		return nil, nil, fmt.Errorf("post-sign validation: nil playlist")
	}
	return signed, pl, nil
}

// parseValidatedPlaylist runs dp1-go ParseAndValidate for core or core+extension, returning the typed playlist.
func (e *impl) parseValidatedPlaylist(raw []byte) (*playlist.Playlist, error) {
	if e.extensionsEnabled {
		return e.dp1.ValidatePlaylistWithExtension(raw)
	}
	return e.dp1.ValidatePlaylist(raw)
}

// parseDocumentCreated parses JSON "created" from a stored DP-1 document body (RFC3339 / RFC3339Nano).
func parseDocumentCreated(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse document created: %w", err)
	}
	return t, nil
}

// GetPlaylist returns the stored playlist record for id or slug (Raw is served verbatim).
func (e *impl) GetPlaylist(ctx context.Context, idOrSlug string) (*store.PlaylistRecord, error) {
	return e.store.GetPlaylist(ctx, idOrSlug)
}

// ListPlaylists returns one page of stored playlist records.
func (e *impl) ListPlaylists(ctx context.Context, limit int, cursor string, sort store.SortOrder, channelFilter, playlistGroupFilter string) ([]store.PlaylistRecord, string, error) {
	if !e.extensionsEnabled && strings.TrimSpace(channelFilter) != "" {
		return nil, "", ErrExtensionsDisabled
	}
	return e.store.ListPlaylists(ctx, &store.ListPlaylistsParams{
		Limit:               limit,
		Cursor:              cursor,
		Sort:                sort,
		ChannelFilter:       channelFilter,
		PlaylistGroupFilter: playlistGroupFilter,
	})
}

// ReplacePlaylist replaces a playlist by id or slug with the client's signed document, stored verbatim.
//
// Owner-bound, identity- and owner-immutable: signatures[] is required; the document's id, slug and
// created must EQUAL the stored row's (validated by mustMatchStored, never substituted — substituting
// would change bytes the client signed); the curator (owner) set may not change; every item must carry a
// UUID id; all signatures must verify over the submitted bytes; and at least one must be a stored owner.
func (e *impl) ReplacePlaylist(ctx context.Context, idOrSlug string, req *models.PlaylistReplaceRequest, intent *models.SignedIntent) (*store.PlaylistRecord, error) {
	if err := requireSignatures(req.Signatures); err != nil {
		return nil, err
	}
	if err := requireItemIDs(req.Items); err != nil {
		return nil, err
	}

	// 1) Get the existing playlist row and its owner (curator) key set.
	rec, err := e.store.GetPlaylist(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	si, err := newSignedIdentity(req.ID, req.Created, req.Slug, req.Raw)
	if err != nil {
		return nil, err
	}
	if err := si.mustMatchStored(rec.ID, rec.Slug, rec.Body.Created); err != nil {
		return nil, err
	}
	ownerKeys := entityKeySet(rec.Body.Curators)
	if err := requireImmutableEntityOwner(ownerKeys, entityKeySet(req.Curators)); err != nil {
		return nil, err
	}

	// 2) Authorize over the SUBMITTED bytes: every signature must cryptographically verify (400), and at
	// least one must come from a stored owner key (403).
	ok, failed, err := e.dp1.VerifyPlaylistSignatures(req.Raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSignatureVerificationFailed, err)
	}
	if !ok {
		return nil, signatureFailure(failed)
	}
	if err := requireStoredOwnerSignature(ownerKeys, req.Signatures); err != nil {
		return nil, err
	}
	// The document proves the owner authored this content; the intent proves the owner is asking for it to
	// replace THIS resource NOW. Without it the document's own (public) signatures would authorize replaying
	// an older version to roll the resource back.
	if err := e.verifyIntent(intent, models.IntentActionReplace, models.IntentTargetPlaylist, rec.ID, rec.Slug, ownerKeys, req.Raw); err != nil {
		return nil, err
	}

	// 3) Feed co-signs the same bytes, validate, persist by stable UUID (never the caller-supplied slug:
	// authorization was established for rec.ID). The store rebuilds playlist_item_index from items[].
	signed, pl, err := e.signAndValidatePlaylist(req.Raw, time.Now())
	if err != nil {
		return nil, err
	}
	// Write by stable UUID and conditional on the updated_at observed when this request was authorized:
	// the ownership decision above was made about that exact row generation, so if anything committed in
	// between (including a delete and re-create under the same client-chosen id) the write must fail
	// rather than apply to a different document.
	if err := e.store.UpdatePlaylist(ctx, rec.ID.String(), signed, rec.UpdatedAt); err != nil {
		return nil, err
	}
	return &store.PlaylistRecord{ID: rec.ID, Slug: rec.Slug, Raw: signed, Body: *pl}, nil
}

// DeletePlaylist authorizes a signed delete-intent against the stored playlist's curator (owner) keys,
// then removes the playlist row. The intent must name this exact resource and carry a fresh, verifying
// owner signature (see verifyIntent).
func (e *impl) DeletePlaylist(ctx context.Context, idOrSlug string, req *models.SignedDeleteRequest) error {
	rec, err := e.store.GetPlaylist(ctx, idOrSlug)
	if err != nil {
		return err
	}
	if err := e.verifyIntent(req, models.IntentActionDelete, models.IntentTargetPlaylist, rec.ID, rec.Slug, entityKeySet(rec.Body.Curators), nil); err != nil {
		return err
	}
	// Delete by stable UUID, not the caller-supplied slug, and conditional on the updated_at this
	// authorization was made against: a slug reused after load cannot redirect the delete, and a row
	// re-created under the same id after load is a different document, so the delete fails instead.
	return e.store.DeletePlaylist(ctx, rec.ID.String(), rec.UpdatedAt)
}

// ListPlaylistItems returns stored playlist items from playlist_item_index with optional channel or playlist-group scope.
func (e *impl) ListPlaylistItems(ctx context.Context, limit int, cursor string, sort store.SortOrder, channelFilter, playlistGroupFilter string) ([]playlist.PlaylistItem, string, error) {
	if !e.extensionsEnabled && channelFilter != "" {
		return nil, "", ErrExtensionsDisabled
	}

	recs, nextCur, err := e.store.ListPlaylistItems(ctx, &store.ListPlaylistItemsParams{
		Limit:               limit,
		Cursor:              cursor,
		Sort:                sort,
		ChannelFilter:       channelFilter,
		PlaylistGroupFilter: playlistGroupFilter,
	})
	if err != nil {
		return nil, "", err
	}

	out := make([]playlist.PlaylistItem, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Item)
	}
	return out, nextCur, nil
}

// GetPlaylistItem returns one item from the index by item id.
func (e *impl) GetPlaylistItem(ctx context.Context, itemID uuid.UUID) (*playlist.PlaylistItem, error) {
	rec, err := e.store.GetPlaylistItem(ctx, itemID)
	if err != nil {
		return nil, err
	}
	return &rec.Item, nil
}

// CreatePlaylistGroup verifies the client's curator signature over the received bytes, resolves playlist
// URIs (parallel fetch or local GET), feed co-signs the same bytes, validates, and commits upserted
// playlists, the group row, and membership in one transaction.
func (e *impl) CreatePlaylistGroup(ctx context.Context, req *models.PlaylistGroupCreateRequest) (*store.PlaylistGroupRecord, error) {
	uris := req.Playlists

	// Authorize BEFORE resolving playlist URIs: resolution can make outbound HTTP fetches, so an
	// unauthorized request must be rejected first (RequireSignatures only proves the array is non-empty).
	if err := requireSignatures(req.Signatures); err != nil {
		return nil, err
	}
	si, err := newSignedIdentity(req.ID, req.Created, req.Slug, req.Raw)
	if err != nil {
		return nil, err
	}
	if err := e.verifyPlaylistGroupCuratorSignatures(req.Raw, req.Signatures, req.Curator); err != nil {
		return nil, fmt.Errorf("curator signature verification: %w", err)
	}

	// Resolve every URI to stored playlist rows (parallel), preserving order for membership and FK targets.
	ingested, err := e.resolvePlaylistURIs(ctx, uris)
	if err != nil {
		return nil, err
	}

	signed, group, err := e.signAndValidatePlaylistGroup(req.Raw, si.created)
	if err != nil {
		return nil, err
	}
	if err := e.store.CreatePlaylistGroup(ctx, &store.PlaylistGroupInput{
		ID:        si.id,
		Slug:      si.slug,
		Raw:       signed,
		Playlists: ingested,
	}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return &store.PlaylistGroupRecord{ID: si.id, Slug: si.slug, Raw: signed, Body: *group}, nil
}

// signAndValidatePlaylistGroup appends the feed signature and validates (the playlist-group schema
// requires signatures, so unlike core playlists there is no pre-sign schema pass).
func (e *impl) signAndValidatePlaylistGroup(raw []byte, ts time.Time) ([]byte, *playlistgroup.Group, error) {
	signed, err := e.dp1.SignPlaylistGroup(raw, ts)
	if err != nil {
		return nil, nil, fmt.Errorf("feed sign: %w", err)
	}
	group, err := e.dp1.ValidatePlaylistGroup(signed)
	if err != nil {
		return nil, nil, fmt.Errorf("post-sign validation: %w", err)
	}
	if group == nil {
		return nil, nil, fmt.Errorf("post-sign validation: nil playlist-group")
	}
	return signed, group, nil
}

// GetPlaylistGroup returns the stored playlist-group record for id or slug (Raw is served verbatim).
func (e *impl) GetPlaylistGroup(ctx context.Context, idOrSlug string) (*store.PlaylistGroupRecord, error) {
	return e.store.GetPlaylistGroup(ctx, idOrSlug)
}

// ListPlaylistGroups returns one page of stored playlist-group records.
func (e *impl) ListPlaylistGroups(ctx context.Context, limit int, cursor string, sort store.SortOrder) ([]store.PlaylistGroupRecord, string, error) {
	return e.store.ListPlaylistGroups(ctx, &store.ListPlaylistsParams{
		Limit:  limit,
		Cursor: cursor,
		Sort:   sort,
	})
}

// ReplacePlaylistGroup replaces a group with the client's signed document, stored verbatim, and
// re-resolves membership. Owner-bound, identity- and owner-immutable (see ReplacePlaylist).
func (e *impl) ReplacePlaylistGroup(ctx context.Context, idOrSlug string, req *models.PlaylistGroupReplaceRequest, intent *models.SignedIntent) (*store.PlaylistGroupRecord, error) {
	if err := requireSignatures(req.Signatures); err != nil {
		return nil, err
	}

	// 1. Get the existing playlist-group row and its owner (curator).
	rec, err := e.store.GetPlaylistGroup(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	si, err := newSignedIdentity(req.ID, req.Created, req.Slug, req.Raw)
	if err != nil {
		return nil, err
	}
	if err := si.mustMatchStored(rec.ID, rec.Slug, rec.Body.Created); err != nil {
		return nil, err
	}
	if err := requireImmutableStringOwner(rec.Body.Curator, req.Curator); err != nil {
		return nil, err
	}
	ownerKeys := stringOwnerKeySet(rec.Body.Curator)
	uris := req.Playlists

	// 2. Authorize over the SUBMITTED bytes BEFORE resolving playlist URIs (resolution can fetch remote
	// URLs): crypto-verify all signatures (400), then require a stored-owner signature (403).
	ok, failed, err := e.dp1.VerifyPlaylistGroupSignatures(req.Raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSignatureVerificationFailed, err)
	}
	if !ok {
		return nil, signatureFailure(failed)
	}
	if err := requireStoredOwnerSignature(ownerKeys, req.Signatures); err != nil {
		return nil, err
	}
	// The document proves the owner authored this content; the intent proves the owner is asking for it to
	// replace THIS resource NOW. Without it the document's own (public) signatures would authorize replaying
	// an older version to roll the resource back.
	if err := e.verifyIntent(intent, models.IntentActionReplace, models.IntentTargetPlaylistGroup, rec.ID, rec.Slug, ownerKeys, req.Raw); err != nil {
		return nil, err
	}

	// 3. Fresh fetch/lookup for every URI; membership rows are replaced in the same store transaction.
	ingested, err := e.resolvePlaylistURIs(ctx, uris)
	if err != nil {
		return nil, err
	}

	// 4. Feed co-signs the same bytes, validate, persist by stable UUID (see ReplacePlaylist).
	signed, group, err := e.signAndValidatePlaylistGroup(req.Raw, time.Now())
	if err != nil {
		return nil, err
	}
	// Conditional on the updated_at read at authorization. This matters most here: remote playlist-URI
	// resolution ran between that read and this write, so the window is widest for groups and channels.
	if err := e.store.UpdatePlaylistGroup(ctx, rec.ID.String(), &store.PlaylistGroupInput{
		Raw:       signed,
		Playlists: ingested,
	}, rec.UpdatedAt); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return &store.PlaylistGroupRecord{ID: rec.ID, Slug: rec.Slug, Raw: signed, Body: *group}, nil
}

// DeletePlaylistGroup authorizes a signed delete-intent against the stored group's curator (owner), then
// removes the playlist-group row (membership CASCADE).
func (e *impl) DeletePlaylistGroup(ctx context.Context, idOrSlug string, req *models.SignedDeleteRequest) error {
	rec, err := e.store.GetPlaylistGroup(ctx, idOrSlug)
	if err != nil {
		return err
	}
	if err := e.verifyIntent(req, models.IntentActionDelete, models.IntentTargetPlaylistGroup, rec.ID, rec.Slug, stringOwnerKeySet(rec.Body.Curator), nil); err != nil {
		return err
	}
	// Delete by stable UUID and conditional on the authorized updated_at (see DeletePlaylist).
	return e.store.DeletePlaylistGroup(ctx, rec.ID.String(), rec.UpdatedAt)
}

// CreateChannel verifies the client's publisher signature over the received bytes, resolves playlist
// URIs, feed co-signs the same bytes, validates, and commits channel + playlists + membership in one
// transaction (requires extensions).
func (e *impl) CreateChannel(ctx context.Context, req *models.ChannelCreateRequest) (*store.ChannelRecord, error) {
	if !e.extensionsEnabled {
		return nil, ErrExtensionsDisabled
	}
	uris := req.Playlists

	// Authorize BEFORE resolving playlist URIs (resolution can fetch remote URLs); see CreatePlaylistGroup.
	if err := requireSignatures(req.Signatures); err != nil {
		return nil, err
	}
	si, err := newSignedIdentity(req.ID, req.Created, req.Slug, req.Raw)
	if err != nil {
		return nil, err
	}
	if err := requirePublisherKey(publisherKey(req.Publisher)); err != nil {
		return nil, err
	}
	if err := e.verifyChannelPublisherSignatures(req.Raw, req.Signatures, req.Publisher); err != nil {
		return nil, fmt.Errorf("publisher signature verification: %w", err)
	}

	// Resolve every URI to stored playlist rows (parallel), preserving order for membership and FK targets.
	ingested, err := e.resolvePlaylistURIs(ctx, uris)
	if err != nil {
		return nil, err
	}

	signed, ch, err := e.signAndValidateChannel(req.Raw, si.created)
	if err != nil {
		return nil, err
	}
	if err := e.runChannelMutation(ctx, func(mutationCtx context.Context) error {
		return e.store.CreateChannel(mutationCtx, &store.ChannelInput{
			ID:        si.id,
			Slug:      si.slug,
			Raw:       signed,
			Playlists: ingested,
		})
	}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	e.notifyChannel(ctx, notification.ChannelAdded, si.id)
	return &store.ChannelRecord{ID: si.id, Slug: si.slug, Raw: signed, Body: *ch}, nil
}

// signAndValidateChannel appends the feed signature and validates (channels schema requires signatures).
func (e *impl) signAndValidateChannel(raw []byte, ts time.Time) ([]byte, *channels.Channel, error) {
	signed, err := e.dp1.SignChannel(raw, ts)
	if err != nil {
		return nil, nil, fmt.Errorf("feed sign: %w", err)
	}
	ch, err := e.dp1.ValidateChannel(signed)
	if err != nil {
		return nil, nil, fmt.Errorf("post-sign validation: %w", err)
	}
	if ch == nil {
		return nil, nil, fmt.Errorf("post-sign validation: nil channel")
	}
	return signed, ch, nil
}

// GetChannel returns the stored channel record for id or slug (Raw is served verbatim).
func (e *impl) GetChannel(ctx context.Context, idOrSlug string) (*store.ChannelRecord, error) {
	if !e.extensionsEnabled {
		return nil, ErrExtensionsDisabled
	}
	return e.store.GetChannel(ctx, idOrSlug)
}

// ListChannels returns one page of stored channel records.
func (e *impl) ListChannels(ctx context.Context, limit int, cursor string, sort store.SortOrder) ([]store.ChannelRecord, string, error) {
	if !e.extensionsEnabled {
		return nil, "", ErrExtensionsDisabled
	}
	return e.store.ListChannels(ctx, &store.ListPlaylistsParams{
		Limit:  limit,
		Cursor: cursor,
		Sort:   sort,
	})
}

// ReplaceChannel replaces a channel with the client's signed document, stored verbatim, and re-resolves
// membership. Owner-bound, identity- and owner-immutable: the publisher (owner) may not change; channel
// curators[] may. See ReplacePlaylist.
func (e *impl) ReplaceChannel(ctx context.Context, idOrSlug string, req *models.ChannelReplaceRequest, intent *models.SignedIntent) (*store.ChannelRecord, error) {
	if !e.extensionsEnabled {
		return nil, ErrExtensionsDisabled
	}
	if err := requireSignatures(req.Signatures); err != nil {
		return nil, err
	}

	// 1. Get the existing channel row and its owner (publisher).
	rec, err := e.store.GetChannel(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	si, err := newSignedIdentity(req.ID, req.Created, req.Slug, req.Raw)
	if err != nil {
		return nil, err
	}
	if err := si.mustMatchStored(rec.ID, rec.Slug, rec.Body.Created); err != nil {
		return nil, err
	}
	if err := requirePublisherKey(publisherKey(req.Publisher)); err != nil {
		return nil, err
	}
	if err := requireImmutableStringOwner(publisherKey(rec.Body.Publisher), publisherKey(req.Publisher)); err != nil {
		return nil, err
	}
	ownerKeys := stringOwnerKeySet(publisherKey(rec.Body.Publisher))
	uris := req.Playlists

	// 2. Authorize over the SUBMITTED bytes BEFORE resolving playlist URIs: crypto-verify all signatures
	// (400), then require a stored-owner (publisher) signature (403).
	ok, failed, err := e.dp1.VerifyChannelSignatures(req.Raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSignatureVerificationFailed, err)
	}
	if !ok {
		return nil, signatureFailure(failed)
	}
	if err := requireStoredOwnerSignature(ownerKeys, req.Signatures); err != nil {
		return nil, err
	}
	// The document proves the owner authored this content; the intent proves the owner is asking for it to
	// replace THIS resource NOW. Without it the document's own (public) signatures would authorize replaying
	// an older version to roll the resource back.
	if err := e.verifyIntent(intent, models.IntentActionReplace, models.IntentTargetChannel, rec.ID, rec.Slug, ownerKeys, req.Raw); err != nil {
		return nil, err
	}

	// 3. Fresh fetch/lookup for every URI (only after authorization).
	ingested, err := e.resolvePlaylistURIs(ctx, uris)
	if err != nil {
		return nil, err
	}

	// 4. Feed co-signs the same bytes, validate, persist by stable UUID.
	signed, ch, err := e.signAndValidateChannel(req.Raw, time.Now())
	if err != nil {
		return nil, err
	}
	// Conditional on the updated_at read at authorization; playlist-URI resolution ran in between
	// (see ReplacePlaylistGroup).
	if err := e.runChannelMutation(ctx, func(mutationCtx context.Context) error {
		return e.store.UpdateChannel(mutationCtx, rec.ID.String(), &store.ChannelInput{
			Raw:       signed,
			Playlists: ingested,
		}, rec.UpdatedAt)
	}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	e.notifyChannel(ctx, notification.ChannelUpdated, rec.ID)
	return &store.ChannelRecord{ID: rec.ID, Slug: rec.Slug, Raw: signed, Body: *ch}, nil
}

// DeleteChannel authorizes a signed delete-intent against the stored channel's publisher (owner), then
// removes the channel row (membership CASCADE) and notifies clients.
func (e *impl) DeleteChannel(ctx context.Context, idOrSlug string, req *models.SignedDeleteRequest) error {
	if !e.extensionsEnabled {
		return ErrExtensionsDisabled
	}
	rec, err := e.store.GetChannel(ctx, idOrSlug)
	if err != nil {
		return err
	}
	if err := e.verifyIntent(req, models.IntentActionDelete, models.IntentTargetChannel, rec.ID, rec.Slug, stringOwnerKeySet(publisherKey(rec.Body.Publisher)), nil); err != nil {
		return err
	}
	// Delete by stable UUID and conditional on the authorized updated_at (see DeletePlaylist).
	if err := e.runChannelMutation(ctx, func(mutationCtx context.Context) error {
		return e.store.DeleteChannel(mutationCtx, rec.ID.String(), rec.UpdatedAt)
	}); err != nil {
		return err
	}
	e.notifyChannel(ctx, notification.ChannelDeleted, rec.ID)
	return nil
}

// GetChannelRegistry returns the curated channel registry (publishers + channels in order).
// The registry is read-only over the API: there is no signed document to authorize a full replace, so
// the write endpoint was removed with the API key. Seed it out-of-band (migration/tooling).
func (e *impl) GetChannelRegistry(ctx context.Context) ([]store.RegistryPublisher, []store.RegistryPublisherChannel, error) {
	return e.store.GetChannelRegistry(ctx)
}

// APIInfo returns static deployment metadata for GET /api/v1.
func (e *impl) APIInfo(version string) map[string]any {
	return map[string]any{
		"name":              "DP-1 Feed Operator API",
		"version":           version,
		"description":       "REST API for DP-1 playlists, playlist-groups, and channels",
		"specification":     "DP-1 v1.1.0+",
		"openapi":           "3.1.0",
		"deployment":        "self-hosted",
		"runtime":           "go",
		"extensionsEnabled": e.extensionsEnabled,
		"endpoints": map[string]string{
			"playlists":      "/api/v1/playlists",
			"playlistGroups": "/api/v1/playlist-groups",
			"channels":       "/api/v1/channels",
			"playlistItems":  "/api/v1/playlist-items",
			"registry":       "/api/v1/registry/channels",
			"health":         "/api/v1/health",
		},
		"documentation": "https://github.com/display-protocol/dp1",
	}
}

// IsExtensionsDisabled reports whether err is ErrExtensionsDisabled.
func IsExtensionsDisabled(err error) bool {
	return errors.Is(err, ErrExtensionsDisabled)
}

// IsDP1SignError reports whether err is a DP-1 signature-layer failure from dp1-go/sign
// (also re-exported on the root dp1 package as ErrSigInvalid, ErrUnsupportedAlg, ErrNoSignatures).
// Wrapped errors from fmt.Errorf("…: %w", err) remain detectable via errors.Is.
func IsDP1SignError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, sign.ErrSigInvalid) ||
		errors.Is(err, sign.ErrUnsupportedAlg) ||
		errors.Is(err, sign.ErrNoSignatures)
}

// IsDP1ValidationError reports whether err is a DP-1 JSON Schema validation failure from dp1-go.
func IsDP1ValidationError(err error) bool {
	return err != nil && errors.Is(err, dp1.ErrValidation)
}

// IsSignatureVerificationError reports whether err is a trusted model signature verification failure.
func IsSignatureVerificationError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrSignatureVerificationFailed) ||
		errors.Is(err, ErrNoValidCuratorSignature) ||
		errors.Is(err, ErrNoValidPublisherSignature)
}

// IsInvalidTimestampError reports whether err is a trusted model timestamp validation failure.
func IsInvalidTimestampError(err error) bool {
	return err != nil && errors.Is(err, ErrInvalidTimestamp)
}

// IsInvalidIDError reports whether err is a trusted model id validation failure.
func IsInvalidIDError(err error) bool {
	return err != nil && errors.Is(err, ErrInvalidID)
}

// parseUserProvidedID validates user-provided id is a valid UUID.
func parseUserProvidedID(idStr *string) (uuid.UUID, error) {
	if idStr == nil || *idStr == "" {
		return uuid.UUID{}, fmt.Errorf("%w: id is required for signature-based authentication", ErrInvalidID)
	}
	id, err := uuid.Parse(*idStr)
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("%w: %w", ErrInvalidID, err)
	}
	return id, nil
}

// parseUserProvidedCreated validates user-provided created timestamp is RFC3339 and not in the future.
func parseUserProvidedCreated(createdStr *string) (time.Time, error) {
	if createdStr == nil || *createdStr == "" {
		return time.Time{}, fmt.Errorf("%w: created is required for signature-based authentication", ErrInvalidTimestamp)
	}
	t, err := time.Parse(time.RFC3339, *createdStr)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: must be RFC3339 format: %w", ErrInvalidTimestamp, err)
	}
	if t.After(time.Now()) {
		return time.Time{}, ErrInvalidTimestamp
	}
	return t, nil
}

// publisherKey returns the channel publisher's key, or "" when there is no publisher. It is the channel
// owner identity used for replace/delete authorization.
func publisherKey(publisher *identity.Entity) string {
	if publisher == nil {
		return ""
	}
	return strings.TrimSpace(publisher.Key)
}

// verifyPlaylistCuratorSignatures verifies that at least one signature in sigs matches a curator key.
// Returns ErrNoValidCuratorSignature if no matching curator signature is found, or ErrSignatureVerificationFailed
// if signature cryptographic verification fails.
func (e *impl) verifyPlaylistCuratorSignatures(raw []byte, sigs []playlist.Signature, curators []identity.Entity) error {
	// First, verify all signatures cryptographically
	ok, failed, err := e.dp1.VerifyPlaylistSignatures(raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSignatureVerificationFailed, err)
	}
	if !ok {
		// Build detailed error message showing which signatures failed
		var failedKids []string
		for _, sig := range failed {
			failedKids = append(failedKids, sig.Kid)
		}
		return fmt.Errorf("%w: failed signatures: %v", ErrSignatureVerificationFailed, failedKids)
	}

	// Extract curator keys from request
	curatorKeys := make(map[string]bool)
	for _, curator := range curators {
		if curator.Key != "" {
			curatorKeys[curator.Key] = true
		}
	}

	// Check if at least one signature matches a curator
	for _, sig := range sigs {
		if curatorKeys[sig.Kid] {
			return nil // Found valid curator signature
		}
	}

	return ErrNoValidCuratorSignature
}

// verifyPlaylistGroupCuratorSignatures verifies that at least one signature matches the curator field.
// Playlist groups have a single curator string field, not an array.
func (e *impl) verifyPlaylistGroupCuratorSignatures(raw []byte, sigs []playlist.Signature, curatorKey string) error {
	// First, verify all signatures cryptographically
	ok, failed, err := e.dp1.VerifyPlaylistGroupSignatures(raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSignatureVerificationFailed, err)
	}
	if !ok {
		var failedKids []string
		for _, sig := range failed {
			failedKids = append(failedKids, sig.Kid)
		}
		return fmt.Errorf("%w: failed signatures: %v", ErrSignatureVerificationFailed, failedKids)
	}

	// Check if at least one signature matches the curator
	for _, sig := range sigs {
		if sig.Kid == curatorKey {
			return nil // Found valid curator signature
		}
	}

	return ErrNoValidCuratorSignature
}

// verifyChannelPublisherSignatures verifies that at least one signature matches the publisher.
func (e *impl) verifyChannelPublisherSignatures(raw []byte, sigs []playlist.Signature, publisher *identity.Entity) error {
	// First, verify all signatures cryptographically
	ok, failed, err := e.dp1.VerifyChannelSignatures(raw)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSignatureVerificationFailed, err)
	}
	if !ok {
		var failedKids []string
		for _, sig := range failed {
			failedKids = append(failedKids, sig.Kid)
		}
		return fmt.Errorf("%w: failed signatures: %v", ErrSignatureVerificationFailed, failedKids)
	}

	if publisher == nil || publisher.Key == "" {
		return fmt.Errorf("document has no publisher")
	}

	// Check if at least one signature matches the publisher
	for _, sig := range sigs {
		if sig.Kid == publisher.Key {
			return nil // Found valid publisher signature
		}
	}

	return ErrNoValidPublisherSignature
}
