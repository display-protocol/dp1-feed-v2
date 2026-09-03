// Package executor contains feed business logic: validation, signing, persistence, and transactional
// ingest of referenced playlists when creating or updating playlist-groups and channels.
// Playlist URI resolution (local API vs HTTP fetch, ordering) lives in ingest_resolve.go.
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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
// Create/Replace methods return the validated, persisted document (signatures included); HTTP layer JSON-encodes the response.
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
	CreatePlaylist(ctx context.Context, req *models.PlaylistCreateRequest) (*playlist.Playlist, error)
	// GetPlaylist returns the stored playlist document for id or slug (HTTP layer JSON-encodes the response).
	GetPlaylist(ctx context.Context, idOrSlug string) (*playlist.Playlist, error)
	// ListPlaylists returns one page of playlist bodies and an optional next cursor (optional channel or playlist-group filter; id or slug).
	ListPlaylists(ctx context.Context, limit int, cursor string, sort store.SortOrder, channelFilter, playlistGroupFilter string) ([]playlist.Playlist, string, error)
	// ReplacePlaylist performs a full PUT (owner-bound, owner immutable): verify owner signature, feed co-sign, validate, update.
	ReplacePlaylist(ctx context.Context, idOrSlug string, req *models.PlaylistReplaceRequest) (*playlist.Playlist, error)
	// DeletePlaylist verifies the signed delete-intent against the stored owner keys, then removes the playlist row.
	DeletePlaylist(ctx context.Context, idOrSlug string, req *models.SignedDeleteRequest) error

	// ListPlaylistItems returns one page of stored playlist items from the item index (OpenAPI GET /playlist-items).
	ListPlaylistItems(ctx context.Context, limit int, cursor string, sort store.SortOrder, channelFilter, playlistGroupFilter string) ([]playlist.PlaylistItem, string, error)
	// GetPlaylistItem returns a single indexed playlist item by UUID (OpenAPI GET /playlist-items/{id}).
	GetPlaylistItem(ctx context.Context, itemID uuid.UUID) (*playlist.PlaylistItem, error)

	// CreatePlaylistGroup resolves each playlist URI (parallel fetch or local GET), then signs the group and commits group + upserted playlists + membership in one transaction.
	CreatePlaylistGroup(ctx context.Context, req *models.PlaylistGroupCreateRequest) (*playlistgroup.Group, error)
	// GetPlaylistGroup returns the stored playlist-group document for id or slug (HTTP layer JSON-encodes).
	GetPlaylistGroup(ctx context.Context, idOrSlug string) (*playlistgroup.Group, error)
	// ListPlaylistGroups returns one page of playlist-group bodies.
	ListPlaylistGroups(ctx context.Context, limit int, cursor string, sort store.SortOrder) ([]playlistgroup.Group, string, error)
	// ReplacePlaylistGroup re-resolves playlist URIs, verifies the owner signature, re-signs, and commits updates in one transaction.
	ReplacePlaylistGroup(ctx context.Context, idOrSlug string, req *models.PlaylistGroupReplaceRequest) (*playlistgroup.Group, error)
	// DeletePlaylistGroup verifies the signed delete-intent against the stored curator, then removes the playlist-group row (membership CASCADE).
	DeletePlaylistGroup(ctx context.Context, idOrSlug string, req *models.SignedDeleteRequest) error

	// CreateChannel resolves playlist URIs, signs the channel document, and commits channel + playlists + membership in one transaction (requires extensions).
	CreateChannel(ctx context.Context, req *models.ChannelCreateRequest) (*channels.Channel, error)
	// GetChannel returns the stored channel document for id or slug (HTTP layer JSON-encodes).
	GetChannel(ctx context.Context, idOrSlug string) (*channels.Channel, error)
	// ListChannels returns one page of channel bodies.
	ListChannels(ctx context.Context, limit int, cursor string, sort store.SortOrder) ([]channels.Channel, string, error)
	// ReplaceChannel re-resolves playlist URIs, verifies the owner (publisher) signature, re-signs, and commits updates in one transaction.
	ReplaceChannel(ctx context.Context, idOrSlug string, req *models.ChannelReplaceRequest) (*channels.Channel, error)
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
	deleteSkew         time.Duration
}

// Option configures optional executor side-effect boundaries.
type Option func(*impl)

// WithNotificationClient registers the client notified after successful channel mutations.
func WithNotificationClient(client notification.Client) Option {
	return func(e *impl) {
		e.notificationClient = client
	}
}

// WithDeleteClockSkew sets the signed delete-intent freshness window. A non-positive value leaves the
// executor default (defaultDeleteSkew) in place.
func WithDeleteClockSkew(d time.Duration) Option {
	return func(e *impl) {
		if d > 0 {
			e.deleteSkew = d
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
		deleteSkew:        defaultDeleteSkew,
	}
	for _, option := range options {
		option(e)
	}
	return e
}

// defaultDeleteSkew is the signed delete-intent freshness window when WithDeleteClockSkew is not set.
// Kept small to bound replay of a captured delete after the same id is re-created.
const defaultDeleteSkew = 5 * time.Minute

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

// CreatePlaylist builds the client's playlist document, verifies its curator signatures, feed co-signs,
// validates the signed JSON, then persists. Validation runs only after signing so the payload carries
// signatures as required by the schema.
//
// Create is open: any client may create a document validly self-signed by a key it declares in
// curators[]. id, created, and signatures[] are required (see requireSignatures); the signer becomes
// the resource's owner for later replace/delete.
func (e *impl) CreatePlaylist(ctx context.Context, req *models.PlaylistCreateRequest) (*playlist.Playlist, error) {
	if err := requireSignatures(req.Signatures); err != nil {
		return nil, err
	}
	id, err := parseUserProvidedID(req.ID)
	if err != nil {
		return nil, err
	}
	created, err := parseUserProvidedCreated(req.Created)
	if err != nil {
		return nil, err
	}
	slug := makeSlug(req.Slug, req.Title, id, "playlist")
	raw, err := e.buildPlaylistDocument(req, id, slug, created)
	if err != nil {
		return nil, err
	}
	if err := e.verifyPlaylistCuratorSignatures(raw, req.Signatures, req.Curators); err != nil {
		return nil, fmt.Errorf("curator signature verification: %w", err)
	}

	signed, err := e.dp1.SignPlaylist(raw, created)
	if err != nil {
		return nil, fmt.Errorf("feed sign: %w", err)
	}

	// Validate complete multi-signed document
	pl, err := e.parseValidatedPlaylist(signed)
	if err != nil {
		return nil, fmt.Errorf("post-sign validation: %w", err)
	}
	if pl == nil {
		return nil, fmt.Errorf("post-sign validation: nil playlist")
	}

	// Persist validated document
	if err := e.store.CreatePlaylist(ctx, id, slug, pl); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}

	return pl, nil
}

// parseValidatedPlaylist runs dp1-go ParseAndValidate for core or core+extension, returning the typed playlist.
func (e *impl) parseValidatedPlaylist(raw []byte) (*playlist.Playlist, error) {
	if e.extensionsEnabled {
		return e.dp1.ValidatePlaylistWithExtension(raw)
	}
	return e.dp1.ValidatePlaylist(raw)
}

// buildPlaylistDocument maps API input into a playlist.Playlist and marshals JSON.
// On create, pass the signing time. On replace/update, pass the timestamp parsed from the stored body JSON "created" (not playlists.created_at).
func (e *impl) buildPlaylistDocument(req *models.PlaylistCreateRequest, id uuid.UUID, slug string, createdAt time.Time) ([]byte, error) {
	dp := strings.TrimSpace(req.DPVersion)
	if dp == "" {
		dp = models.DefaultDPVersion
	}
	items := append([]playlist.PlaylistItem(nil), req.Items...)
	for i := range items {
		if strings.TrimSpace(items[i].ID) == "" {
			items[i].ID = uuid.New().String()
		}
	}
	p := playlist.Playlist{
		DPVersion: dp,
		ID:        id.String(),
		Slug:      slug,
		Title:     req.Title,
		Items:     items,
		Created:   documentCreatedRFC3339Nano(createdAt),
	}
	if len(req.Curators) > 0 {
		p.Curators = req.Curators
	}
	if req.Note != nil {
		p.Note = req.Note
	}
	if req.Summary != "" {
		p.Summary = req.Summary
	}
	if req.CoverImage != "" {
		p.CoverImage = req.CoverImage
	}
	if req.Defaults != nil {
		p.Defaults = req.Defaults
	}
	if req.DynamicQuery != nil {
		p.DynamicQuery = req.DynamicQuery
	}
	if len(req.Signatures) > 0 {
		p.Signatures = req.Signatures
	}
	return json.Marshal(&p)
}

// slugify lowercases, replaces non-alphanumeric runs with '-', trims edges (empty → "").
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

// shortID returns the first 8 characters of the UUID for slug generation.
func shortID(id uuid.UUID) string {
	return id.String()[:8]
}

// makeSlug generates a URL-friendly slug for playlists, groups, and channels.
// Priority: 1) client-provided slug, 2) title-based slug, 3) default+id.
func makeSlug(clientSlug, title string, id uuid.UUID, defaultName string) string {
	// First: try using the client-provided slug
	if clientSlug != "" {
		slug := slugify(clientSlug)
		if slug != "" {
			return slug
		}
	}

	// Second: generate from title
	base := slugify(title)
	if base == "" {
		base = defaultName
	}

	return fmt.Sprintf("%s-%s", base, shortID(id))
}

// documentCreatedRFC3339Nano formats a timestamp for DP-1 JSON "created" (date-time).
func documentCreatedRFC3339Nano(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// parseDocumentCreated parses JSON "created" from a stored DP-1 document body (RFC3339 / RFC3339Nano).
func parseDocumentCreated(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse document created: %w", err)
	}
	return t, nil
}

// GetPlaylist returns the stored playlist document for id or slug.
func (e *impl) GetPlaylist(ctx context.Context, idOrSlug string) (*playlist.Playlist, error) {
	rec, err := e.store.GetPlaylist(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	return &rec.Body, nil
}

// ListPlaylists returns one page of stored playlist documents.
func (e *impl) ListPlaylists(ctx context.Context, limit int, cursor string, sort store.SortOrder, channelFilter, playlistGroupFilter string) ([]playlist.Playlist, string, error) {
	if !e.extensionsEnabled && strings.TrimSpace(channelFilter) != "" {
		return nil, "", ErrExtensionsDisabled
	}

	recs, nextCur, err := e.store.ListPlaylists(ctx, &store.ListPlaylistsParams{
		Limit:               limit,
		Cursor:              cursor,
		Sort:                sort,
		ChannelFilter:       channelFilter,
		PlaylistGroupFilter: playlistGroupFilter,
	})
	if err != nil {
		return nil, "", err
	}
	out := make([]playlist.Playlist, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Body)
	}
	return out, nextCur, nil
}

// ReplacePlaylist replaces a playlist by id/slug (full body); id and document "created" follow the stored
// row; JSON slug comes from request slug/title + id (see makeSlug), not from rec.Slug alone.
//
// Owner-bound and owner-immutable: signatures[] is required; the curator (owner) set may not change from
// the stored document; the new document must be validly self-signed by its curators; and at least one
// verifying signature must come from a stored owner key. Any edit re-derives the bytes, so the owner
// re-signs and the feed co-signs.
func (e *impl) ReplacePlaylist(ctx context.Context, idOrSlug string, req *models.PlaylistReplaceRequest) (*playlist.Playlist, error) {
	if err := requireSignatures(req.Signatures); err != nil {
		return nil, err
	}

	// 1) Get the existing playlist row and its owner (curator) key set.
	rec, err := e.store.GetPlaylist(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	ownerKeys := entityKeySet(rec.Body.Curators)
	if err := requireImmutableEntityOwner(ownerKeys, entityKeySet(req.Curators)); err != nil {
		return nil, err
	}

	// 2) Build the new playlist document.
	created, err := parseDocumentCreated(rec.Body.Created)
	if err != nil {
		return nil, err
	}
	slug := makeSlug(req.Slug, req.Title, rec.ID, "playlist")
	raw, err := e.buildPlaylistDocument(req, rec.ID, slug, created)
	if err != nil {
		return nil, err
	}

	// Authorize the replace: all signatures must cryptographically verify (400 on failure), and at least
	// one must come from a stored owner key (403). Owner-immutability above already pins the declared
	// curators to the stored set, so the stored-owner check is the authoritative identity gate — mirrors
	// the delete path rather than re-checking the document's own declared curators.
	ok, failed, err := e.dp1.VerifyPlaylistSignatures(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSignatureVerificationFailed, err)
	}
	if !ok {
		return nil, signatureFailure(failed)
	}
	if err := requireStoredOwnerSignature(ownerKeys, req.Signatures); err != nil {
		return nil, err
	}

	// 3) Sign with v1.1+ multisig (feed role).
	signed, err := e.dp1.SignPlaylist(raw, time.Now())
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}

	// 4) Validate signed document (schema + §7.1 payload rules) and obtain typed playlist (dp1-go parse path).
	pl, err := e.parseValidatedPlaylist(signed)
	if err != nil {
		return nil, fmt.Errorf("post-sign validation: %w", err)
	}
	if pl == nil {
		return nil, fmt.Errorf("post-sign validation: nil playlist")
	}

	// 5) Persist validated document; DB also builds playlist_item_index from items[].
	if err := e.store.UpdatePlaylist(ctx, idOrSlug, pl); err != nil {
		return nil, err
	}
	return pl, nil
}

// DeletePlaylist authorizes a signed delete-intent against the stored playlist's curator (owner) keys,
// then removes the playlist row. The intent must name this exact resource and carry a fresh, verifying
// owner signature (see verifyDeleteIntent).
func (e *impl) DeletePlaylist(ctx context.Context, idOrSlug string, req *models.SignedDeleteRequest) error {
	rec, err := e.store.GetPlaylist(ctx, idOrSlug)
	if err != nil {
		return err
	}
	if err := e.verifyDeleteIntent(req, rec.ID, rec.Slug, models.DeleteTargetPlaylist, entityKeySet(rec.Body.Curators)); err != nil {
		return err
	}
	return e.store.DeletePlaylist(ctx, idOrSlug)
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

// buildPlaylistGroupDocument builds the group JSON; Playlists holds the same URI strings the client submitted (order preserved).
// On create, pass the signing time. On replace/update, pass the timestamp parsed from the stored body JSON "created" (not playlist_groups.created_at).
func (e *impl) buildPlaylistGroupDocument(req *models.PlaylistGroupCreateRequest, uris []string, id uuid.UUID, slug string, createdAt time.Time) ([]byte, error) {
	g := playlistgroup.Group{
		ID:        id.String(),
		Slug:      slug,
		Title:     req.Title,
		Playlists: uris,
		Created:   documentCreatedRFC3339Nano(createdAt),
	}
	if req.Curator != "" {
		g.Curator = req.Curator
	}
	if req.Summary != "" {
		g.Summary = req.Summary
	}
	if req.CoverImage != "" {
		g.CoverImage = req.CoverImage
	}
	if len(req.Signatures) > 0 {
		g.Signatures = req.Signatures
	}
	return json.Marshal(&g)
}

// CreatePlaylistGroup resolves playlist URIs (parallel fetch or local GET), signs the group document,
// validates the signed JSON (playlist-group schema requires signatures, so unlike core playlists there is no pre-sign schema pass),
// and commits upserted playlists, the group row, and membership in one transaction.
func (e *impl) CreatePlaylistGroup(ctx context.Context, req *models.PlaylistGroupCreateRequest) (*playlistgroup.Group, error) {
	uris := req.Playlists

	// 1. Resolve every URI to stored playlist rows (parallel), preserving order for membership and FK targets.
	ingested, err := e.resolvePlaylistURIs(ctx, uris)
	if err != nil {
		return nil, err
	}

	if err := requireSignatures(req.Signatures); err != nil {
		return nil, err
	}
	id, err := parseUserProvidedID(req.ID)
	if err != nil {
		return nil, err
	}
	created, err := parseUserProvidedCreated(req.Created)
	if err != nil {
		return nil, err
	}
	slug := makeSlug(req.Slug, req.Title, id, "group")
	raw, err := e.buildPlaylistGroupDocument(req, uris, id, slug, created)
	if err != nil {
		return nil, err
	}
	if err := e.verifyPlaylistGroupCuratorSignatures(raw, req.Signatures, req.Curator); err != nil {
		return nil, fmt.Errorf("curator signature verification: %w", err)
	}

	signed, err := e.dp1.SignPlaylistGroup(raw, created)
	if err != nil {
		return nil, fmt.Errorf("feed sign: %w", err)
	}

	// Validate complete multi-signed document
	group, err := e.dp1.ValidatePlaylistGroup(signed)
	if err != nil {
		return nil, fmt.Errorf("post-sign validation: %w", err)
	}
	if group == nil {
		return nil, fmt.Errorf("post-sign validation: nil playlist-group")
	}

	// Persist validated document
	if err := e.store.CreatePlaylistGroup(ctx, &store.PlaylistGroupInput{
		ID:        id,
		Slug:      slug,
		Body:      *group,
		Playlists: ingested,
	}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}

	return group, nil
}

// GetPlaylistGroup returns the stored playlist-group document for id or slug.
func (e *impl) GetPlaylistGroup(ctx context.Context, idOrSlug string) (*playlistgroup.Group, error) {
	rec, err := e.store.GetPlaylistGroup(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	return &rec.Body, nil
}

// ListPlaylistGroups returns one page of stored playlist-group documents.
func (e *impl) ListPlaylistGroups(ctx context.Context, limit int, cursor string, sort store.SortOrder) ([]playlistgroup.Group, string, error) {
	recs, nextCur, err := e.store.ListPlaylistGroups(ctx, &store.ListPlaylistsParams{
		Limit:  limit,
		Cursor: cursor,
		Sort:   sort,
	})
	if err != nil {
		return nil, "", err
	}
	out := make([]playlistgroup.Group, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Body)
	}
	return out, nextCur, nil
}

// ReplacePlaylistGroup re-resolves playlist URIs and commits an update like CreatePlaylistGroup.
// Owner-bound and owner-immutable: signatures[] is required; the curator (owner) may not change; the
// new document must be validly self-signed by that curator, whose signature also authorizes the replace.
func (e *impl) ReplacePlaylistGroup(ctx context.Context, idOrSlug string, req *models.PlaylistGroupReplaceRequest) (*playlistgroup.Group, error) {
	if err := requireSignatures(req.Signatures); err != nil {
		return nil, err
	}

	// 1. Get the existing playlist-group row and its owner (curator).
	rec, err := e.store.GetPlaylistGroup(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	if err := requireImmutableStringOwner(rec.Body.Curator, req.Curator); err != nil {
		return nil, err
	}
	ownerKeys := stringOwnerKeySet(rec.Body.Curator)
	uris := req.Playlists

	// 2. Fresh fetch/lookup for every URI; membership rows are replaced in the same store transaction.
	ingested, err := e.resolvePlaylistURIs(ctx, uris)
	if err != nil {
		return nil, err
	}

	// 3. Build the group document.
	created, err := parseDocumentCreated(rec.Body.Created)
	if err != nil {
		return nil, err
	}
	slug := makeSlug(req.Slug, req.Title, rec.ID, "group")
	raw, err := e.buildPlaylistGroupDocument(req, uris, rec.ID, slug, created)
	if err != nil {
		return nil, err
	}

	// Authorize the replace: crypto-verify all signatures (400), then require a stored-owner signature
	// (403). Owner-immutability pins the declared curator to the stored one; see ReplacePlaylist.
	ok, failed, err := e.dp1.VerifyPlaylistGroupSignatures(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSignatureVerificationFailed, err)
	}
	if !ok {
		return nil, signatureFailure(failed)
	}
	if err := requireStoredOwnerSignature(ownerKeys, req.Signatures); err != nil {
		return nil, err
	}

	// 4. Sign with v1.1+ multisig (feed role).
	signed, err := e.dp1.SignPlaylistGroup(raw, time.Now())
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}

	// 5. Validate signed document (playlist-group schema requires signatures) and obtain typed group (dp1-go parse path).
	group, err := e.dp1.ValidatePlaylistGroup(signed)
	if err != nil {
		return nil, fmt.Errorf("post-sign validation: %w", err)
	}
	if group == nil {
		return nil, fmt.Errorf("post-sign validation: nil playlist-group")
	}

	// 6. Persist validated document.
	if err := e.store.UpdatePlaylistGroup(ctx, idOrSlug, &store.PlaylistGroupInput{
		Body:      *group,
		Playlists: ingested,
	}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return group, nil
}

// DeletePlaylistGroup authorizes a signed delete-intent against the stored group's curator (owner), then
// removes the playlist-group row (membership CASCADE).
func (e *impl) DeletePlaylistGroup(ctx context.Context, idOrSlug string, req *models.SignedDeleteRequest) error {
	rec, err := e.store.GetPlaylistGroup(ctx, idOrSlug)
	if err != nil {
		return err
	}
	if err := e.verifyDeleteIntent(req, rec.ID, rec.Slug, models.DeleteTargetPlaylistGroup, stringOwnerKeySet(rec.Body.Curator)); err != nil {
		return err
	}
	return e.store.DeletePlaylistGroup(ctx, idOrSlug)
}

// buildChannelDocument maps API input to channels.Channel (extensions schema) including curators/publisher entities.
// On create, pass the signing time. On replace/update, pass the timestamp parsed from the stored body JSON "created" (not channels.created_at).
func (e *impl) buildChannelDocument(req *models.ChannelCreateRequest, uris []string, id uuid.UUID, slug string, createdAt time.Time) ([]byte, error) {
	ver := strings.TrimSpace(req.Version)
	if ver == "" {
		ver = models.DefaultChannelVersion
	}
	ch := channels.Channel{
		ID:        id.String(),
		Slug:      slug,
		Title:     req.Title,
		Version:   ver,
		Playlists: uris,
		Created:   documentCreatedRFC3339Nano(createdAt),
	}
	if len(req.Curators) > 0 {
		ch.Curators = req.Curators
	}
	if req.Publisher != nil {
		ch.Publisher = req.Publisher
	}
	if req.Summary != "" {
		ch.Summary = req.Summary
	}
	if req.CoverImage != "" {
		ch.CoverImage = req.CoverImage
	}
	if len(req.Signatures) > 0 {
		ch.Signatures = req.Signatures
	}
	return json.Marshal(&ch)
}

// CreateChannel resolves playlist URIs, signs the channel document, validates signed JSON (channels schema requires signatures), and commits in one transaction.
func (e *impl) CreateChannel(ctx context.Context, req *models.ChannelCreateRequest) (*channels.Channel, error) {
	if !e.extensionsEnabled {
		return nil, ErrExtensionsDisabled
	}
	uris := req.Playlists

	// 1. Resolve every URI to stored playlist rows (parallel), preserving order for membership and FK targets.
	ingested, err := e.resolvePlaylistURIs(ctx, uris)
	if err != nil {
		return nil, err
	}

	if err := requireSignatures(req.Signatures); err != nil {
		return nil, err
	}
	id, err := parseUserProvidedID(req.ID)
	if err != nil {
		return nil, err
	}
	created, err := parseUserProvidedCreated(req.Created)
	if err != nil {
		return nil, err
	}
	slug := makeSlug(req.Slug, req.Title, id, "channel")
	raw, err := e.buildChannelDocument(req, uris, id, slug, created)
	if err != nil {
		return nil, err
	}
	if err := e.verifyChannelPublisherSignatures(raw, req.Signatures, req.Publisher); err != nil {
		return nil, fmt.Errorf("publisher signature verification: %w", err)
	}

	signed, err := e.dp1.SignChannel(raw, created)
	if err != nil {
		return nil, fmt.Errorf("feed sign: %w", err)
	}

	// Validate complete multi-signed document
	ch, err := e.dp1.ValidateChannel(signed)
	if err != nil {
		return nil, fmt.Errorf("post-sign validation: %w", err)
	}
	if ch == nil {
		return nil, fmt.Errorf("post-sign validation: nil channel")
	}

	// Persist validated document
	if err := e.runChannelMutation(ctx, func(mutationCtx context.Context) error {
		return e.store.CreateChannel(mutationCtx, &store.ChannelInput{
			ID:        id,
			Slug:      slug,
			Body:      *ch,
			Playlists: ingested,
		})
	}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	e.notifyChannel(ctx, notification.ChannelAdded, id)
	return ch, nil
}

// GetChannel returns the stored channel document for id or slug.
func (e *impl) GetChannel(ctx context.Context, idOrSlug string) (*channels.Channel, error) {
	if !e.extensionsEnabled {
		return nil, ErrExtensionsDisabled
	}
	rec, err := e.store.GetChannel(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	return &rec.Body, nil
}

// ListChannels returns one page of stored channel documents.
func (e *impl) ListChannels(ctx context.Context, limit int, cursor string, sort store.SortOrder) ([]channels.Channel, string, error) {
	if !e.extensionsEnabled {
		return nil, "", ErrExtensionsDisabled
	}
	recs, nextCur, err := e.store.ListChannels(ctx, &store.ListPlaylistsParams{
		Limit:  limit,
		Cursor: cursor,
		Sort:   sort,
	})
	if err != nil {
		return nil, "", err
	}
	out := make([]channels.Channel, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Body)
	}
	return out, nextCur, nil
}

// ReplaceChannel re-resolves playlist URIs and commits a channel update like CreateChannel.
// Owner-bound and owner-immutable: signatures[] is required; the publisher (owner) may not change; the
// new document must be validly self-signed by that publisher, whose signature also authorizes the replace.
func (e *impl) ReplaceChannel(ctx context.Context, idOrSlug string, req *models.ChannelReplaceRequest) (*channels.Channel, error) {
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
	if err := requireImmutableStringOwner(publisherKey(rec.Body.Publisher), publisherKey(req.Publisher)); err != nil {
		return nil, err
	}
	ownerKeys := stringOwnerKeySet(publisherKey(rec.Body.Publisher))
	uris := req.Playlists

	// 2. Fresh fetch/lookup for every URI; membership rows are replaced in the same store transaction.
	ingested, err := e.resolvePlaylistURIs(ctx, uris)
	if err != nil {
		return nil, err
	}

	// 3. Build the channel document.
	created, err := parseDocumentCreated(rec.Body.Created)
	if err != nil {
		return nil, err
	}
	slug := makeSlug(req.Slug, req.Title, rec.ID, "channel")
	raw, err := e.buildChannelDocument(req, uris, rec.ID, slug, created)
	if err != nil {
		return nil, err
	}

	// Authorize the replace: crypto-verify all signatures (400), then require a stored-owner (publisher)
	// signature (403). Owner-immutability pins the declared publisher to the stored one; see ReplacePlaylist.
	ok, failed, err := e.dp1.VerifyChannelSignatures(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSignatureVerificationFailed, err)
	}
	if !ok {
		return nil, signatureFailure(failed)
	}
	if err := requireStoredOwnerSignature(ownerKeys, req.Signatures); err != nil {
		return nil, err
	}

	// 4. Sign with v1.1+ multisig (feed role).
	signed, err := e.dp1.SignChannel(raw, time.Now())
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}

	// 5. Validate signed document (channels schema requires signatures) and obtain typed channel (dp1-go parse path).
	ch, err := e.dp1.ValidateChannel(signed)
	if err != nil {
		return nil, fmt.Errorf("post-sign validation: %w", err)
	}
	if ch == nil {
		return nil, fmt.Errorf("post-sign validation: nil channel")
	}

	// 6. Persist validated document.
	if err := e.runChannelMutation(ctx, func(mutationCtx context.Context) error {
		return e.store.UpdateChannel(mutationCtx, rec.ID.String(), &store.ChannelInput{
			Body:      *ch,
			Playlists: ingested,
		})
	}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	e.notifyChannel(ctx, notification.ChannelUpdated, rec.ID)
	return ch, nil
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
	if err := e.verifyDeleteIntent(req, rec.ID, rec.Slug, models.DeleteTargetChannel, stringOwnerKeySet(publisherKey(rec.Body.Publisher))); err != nil {
		return err
	}
	if err := e.runChannelMutation(ctx, func(mutationCtx context.Context) error {
		return e.store.DeleteChannel(mutationCtx, rec.ID.String())
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
