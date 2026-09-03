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
// Document methods return store records: Raw is the persisted JSON and is what the HTTP layer writes to
// the wire, verbatim; Body is the decoded view for callers that need fields. Records returned by
// Create/Replace/Update carry ID, Slug, Raw, and Body only (timestamps are populated by reads).
//
// Two write paths share every mutation method, distinguished by a non-empty signatures[] in the request:
//   - API-key path: the feed is the author. It builds the document from the decoded request, assigns
//     id/slug/created defaults, feed-signs, validates, and stores.
//   - Signed-document path: the client is the author and first signer. The feed verifies the client's
//     signatures over req.Raw, appends its own feed signature to those same bytes, validates, and stores
//     them unchanged. It never rebuilds, normalises, or strips anything: DP-1 §7.1 binds every signature
//     to the JCS form of the entire document, so any edit would orphan the client's signature.
//
// A stored document that carries signatures from keys other than the feed's is immutable to the
// API-key path and to PATCH (ErrDocumentImmutable); only a fully re-signed PUT may replace it.
// Known carve-out: group/channel ingest upserts member playlists by id (store.IngestedPlaylist), so a
// referenced remote document with the same id replaces a stored one wholesale — the whole document is
// swapped for another validated one, no signature is orphaned, but the guard above does not apply there.
type Executor interface {
	// CreatePlaylist stores a new playlist (see the write-path contract above).
	CreatePlaylist(ctx context.Context, req *models.PlaylistCreateRequest) (*store.PlaylistRecord, error)
	// GetPlaylist returns the stored playlist for id or slug.
	GetPlaylist(ctx context.Context, idOrSlug string) (*store.PlaylistRecord, error)
	// ListPlaylists returns one page of playlists and an optional next cursor (optional channel or playlist-group filter; id or slug).
	ListPlaylists(ctx context.Context, limit int, cursor string, sort store.SortOrder, channelFilter, playlistGroupFilter string) ([]store.PlaylistRecord, string, error)
	// ReplacePlaylist performs a full PUT (see the write-path contract above).
	ReplacePlaylist(ctx context.Context, idOrSlug string, req *models.PlaylistReplaceRequest) (*store.PlaylistRecord, error)
	// UpdatePlaylist performs a partial PATCH (API-key path only): merges non-nil fields into the stored playlist, feed-signs, validates, and stores.
	UpdatePlaylist(ctx context.Context, idOrSlug string, req *models.PlaylistUpdateRequest) (*store.PlaylistRecord, error)
	// DeletePlaylist removes a playlist row.
	DeletePlaylist(ctx context.Context, idOrSlug string) error

	// ListPlaylistItems returns one page of stored playlist items from the item index (OpenAPI GET /playlist-items).
	ListPlaylistItems(ctx context.Context, limit int, cursor string, sort store.SortOrder, channelFilter, playlistGroupFilter string) ([]playlist.PlaylistItem, string, error)
	// GetPlaylistItem returns a single indexed playlist item by UUID (OpenAPI GET /playlist-items/{id}).
	GetPlaylistItem(ctx context.Context, itemID uuid.UUID) (*playlist.PlaylistItem, error)

	// CreatePlaylistGroup resolves each playlist URI (parallel fetch or local GET), then signs the group and commits group + upserted playlists + membership in one transaction.
	CreatePlaylistGroup(ctx context.Context, req *models.PlaylistGroupCreateRequest) (*store.PlaylistGroupRecord, error)
	// GetPlaylistGroup returns the stored playlist-group for id or slug.
	GetPlaylistGroup(ctx context.Context, idOrSlug string) (*store.PlaylistGroupRecord, error)
	// ListPlaylistGroups returns one page of playlist-groups.
	ListPlaylistGroups(ctx context.Context, limit int, cursor string, sort store.SortOrder) ([]store.PlaylistGroupRecord, string, error)
	// ReplacePlaylistGroup re-resolves playlist URIs, re-signs, and commits updates in one transaction.
	ReplacePlaylistGroup(ctx context.Context, idOrSlug string, req *models.PlaylistGroupReplaceRequest) (*store.PlaylistGroupRecord, error)
	// UpdatePlaylistGroup performs a partial PATCH (API-key path only): merges non-nil fields into the stored group, re-resolves URIs, re-signs, and updates.
	UpdatePlaylistGroup(ctx context.Context, idOrSlug string, req *models.PlaylistGroupUpdateRequest) (*store.PlaylistGroupRecord, error)
	// DeletePlaylistGroup removes a playlist-group row (membership CASCADE).
	DeletePlaylistGroup(ctx context.Context, idOrSlug string) error

	// CreateChannel resolves playlist URIs, signs the channel document, and commits channel + playlists + membership in one transaction (requires extensions).
	CreateChannel(ctx context.Context, req *models.ChannelCreateRequest) (*store.ChannelRecord, error)
	// GetChannel returns the stored channel for id or slug.
	GetChannel(ctx context.Context, idOrSlug string) (*store.ChannelRecord, error)
	// ListChannels returns one page of channels.
	ListChannels(ctx context.Context, limit int, cursor string, sort store.SortOrder) ([]store.ChannelRecord, string, error)
	// ReplaceChannel re-resolves playlist URIs, re-signs, and commits updates in one transaction.
	ReplaceChannel(ctx context.Context, idOrSlug string, req *models.ChannelReplaceRequest) (*store.ChannelRecord, error)
	// UpdateChannel performs a partial PATCH (API-key path only): merges non-nil fields into the stored channel, re-resolves URIs, re-signs, and updates.
	UpdateChannel(ctx context.Context, idOrSlug string, req *models.ChannelUpdateRequest) (*store.ChannelRecord, error)
	// DeleteChannel removes a channel row (membership CASCADE).
	DeleteChannel(ctx context.Context, idOrSlug string) error

	// GetChannelRegistry returns the curated channel registry as ordered publisher items.
	GetChannelRegistry(ctx context.Context) ([]store.RegistryPublisher, []store.RegistryPublisherChannel, error)
	// ReplaceChannelRegistry atomically replaces the entire registry; returns total channel count.
	ReplaceChannelRegistry(ctx context.Context, req models.ChannelRegistry) (int, error)

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
}

// Option configures optional executor side-effect boundaries.
type Option func(*impl)

// WithNotificationClient registers the client notified after successful channel mutations.
func WithNotificationClient(client notification.Client) Option {
	return func(e *impl) {
		e.notificationClient = client
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
	}
	for _, option := range options {
		option(e)
	}
	return e
}

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

// Signed-document path errors (see the Executor write-path contract).
var (
	// ErrSlugRequired is returned when a signed submission omits slug: the feed cannot derive one without editing the document.
	ErrSlugRequired = errors.New("slug is required for signature-based submissions")
	// ErrSignedDocumentMismatch is returned when a signed PUT's id, slug, or created disagree with the stored resource.
	ErrSignedDocumentMismatch = errors.New("signed document does not match the stored resource")
	// ErrSignedItemIDRequired is returned when a signed playlist has an item without a UUID id. The feed's
	// playlist_item_index keys rows by items[].id; the API-key path assigns missing ids, but a signed
	// document cannot be edited, so the client must supply them.
	ErrSignedItemIDRequired = errors.New("signed playlists must give every item a UUID id")
	// ErrDocumentImmutable is returned when the API-key path or PATCH would mutate a document that carries
	// signatures from other keys; those signatures would no longer verify against the edited document.
	ErrDocumentImmutable = errors.New("document carries signatures from other keys and can only be replaced by a fully signed document")
	// errMissingRawBody means the HTTP layer did not attach the request bytes to a signed submission (programming error).
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
// (no slugify): normalising it would change the signed bytes.
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
	if strings.TrimSpace(slug) == "" {
		return signedIdentity{}, ErrSlugRequired
	}
	return signedIdentity{id: id, slug: slug, created: created}, nil
}

// mustMatchStored enforces that a signed PUT replaces the resource it targets: the document's id and
// slug must equal the stored row's, and created must denote the same instant as the stored document's
// (compared as times, since the two may be formatted differently). Changing identity means a new document.
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

// requireSignedItemIDs enforces ErrSignedItemIDRequired before any signing or storage work.
func requireSignedItemIDs(items []playlist.PlaylistItem) error {
	for i := range items {
		if _, err := uuid.Parse(strings.TrimSpace(items[i].ID)); err != nil {
			return fmt.Errorf("%w: items[%d]", ErrSignedItemIDRequired, i)
		}
	}
	return nil
}

// slugOr is the row slug after a write: the document's slug when it has one (the store's Update calls
// make the slug column follow the document), else the existing row slug.
func slugOr(docSlug, rowSlug string) string {
	if strings.TrimSpace(docSlug) != "" {
		return docSlug
	}
	return rowSlug
}

// requireFeedOwned guards the API-key and PATCH paths: a stored document with any signature not made
// by this feed's key is immutable to them. Rebuilding it would keep those foreign entries while
// changing the bytes they attest, leaving a stored document whose signatures no longer verify.
//
// Foreignness is decided by Kid, not role, because role is client-asserted. Operational caveat: after
// a feed key rotation, documents signed only by the previous feed key also count as foreign.
func (e *impl) requireFeedOwned(sigs []playlist.Signature) error {
	if len(sigs) == 0 {
		return nil
	}
	feedKid := e.dp1.Kid()
	for _, s := range sigs {
		if s.Kid != feedKid {
			return ErrDocumentImmutable
		}
	}
	return nil
}

// CreatePlaylist stores a new playlist on either write path (see the Executor contract).
func (e *impl) CreatePlaylist(ctx context.Context, req *models.PlaylistCreateRequest) (*store.PlaylistRecord, error) {
	if len(req.Signatures) > 0 {
		return e.createSignedPlaylist(ctx, req)
	}

	// API-key path: the feed is the author. Optional client id/created are validated when present and
	// defaulted otherwise; slug follows makeSlug. Validation runs after signing because the schema
	// requires signatures on the document.
	id, err := resolveOptionalCreateID(req.ID)
	if err != nil {
		return nil, err
	}
	created, err := resolveOptionalCreateCreated(req.Created)
	if err != nil {
		return nil, err
	}
	slug := makeSlug(req.Slug, req.Title, id, "playlist")
	raw, err := e.buildPlaylistDocument(req, id, slug, created)
	if err != nil {
		return nil, err
	}

	signed, pl, err := e.signAndValidatePlaylist(raw, created)
	if err != nil {
		return nil, err
	}
	if err := e.store.CreatePlaylist(ctx, id, slug, signed); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return &store.PlaylistRecord{ID: id, Slug: slug, Raw: signed, Body: *pl}, nil
}

// createSignedPlaylist is the signed-document path for POST: verify the curator signatures over the
// received bytes, co-sign those bytes, validate, and store them verbatim.
func (e *impl) createSignedPlaylist(ctx context.Context, req *models.PlaylistCreateRequest) (*store.PlaylistRecord, error) {
	si, err := newSignedIdentity(req.ID, req.Created, req.Slug, req.Raw)
	if err != nil {
		return nil, err
	}
	if err := requireSignedItemIDs(req.Items); err != nil {
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

// signAndValidatePlaylist appends the feed signature and validates the result (core or core+extension).
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

// GetPlaylist returns the stored playlist for id or slug.
func (e *impl) GetPlaylist(ctx context.Context, idOrSlug string) (*store.PlaylistRecord, error) {
	return e.store.GetPlaylist(ctx, idOrSlug)
}

// ListPlaylists returns one page of stored playlists.
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

// ReplacePlaylist replaces a playlist by id/slug (full body) on either write path (see the Executor contract).
// API-key path: id and document "created" follow the stored row; JSON slug comes from makeSlug(request slug/title, id).
// Signed path: the document's id, slug, and created must match the stored resource (mustMatchStored).
func (e *impl) ReplacePlaylist(ctx context.Context, idOrSlug string, req *models.PlaylistReplaceRequest) (*store.PlaylistRecord, error) {
	rec, err := e.store.GetPlaylist(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	if len(req.Signatures) > 0 {
		return e.replaceSignedPlaylist(ctx, idOrSlug, rec, req)
	}
	if err := e.requireFeedOwned(rec.Body.Signatures); err != nil {
		return nil, err
	}

	created, err := parseDocumentCreated(rec.Body.Created)
	if err != nil {
		return nil, err
	}
	slug := makeSlug(req.Slug, req.Title, rec.ID, "playlist")
	raw, err := e.buildPlaylistDocument(req, rec.ID, slug, created)
	if err != nil {
		return nil, err
	}
	signed, pl, err := e.signAndValidatePlaylist(raw, time.Now())
	if err != nil {
		return nil, err
	}
	// The store rebuilds playlist_item_index from items[] in the same transaction.
	if err := e.store.UpdatePlaylist(ctx, idOrSlug, signed); err != nil {
		return nil, err
	}
	return &store.PlaylistRecord{ID: rec.ID, Slug: slugOr(pl.Slug, rec.Slug), Raw: signed, Body: *pl}, nil
}

// replaceSignedPlaylist is the signed-document path for PUT (see createSignedPlaylist).
func (e *impl) replaceSignedPlaylist(ctx context.Context, idOrSlug string, rec *store.PlaylistRecord, req *models.PlaylistReplaceRequest) (*store.PlaylistRecord, error) {
	si, err := newSignedIdentity(req.ID, req.Created, req.Slug, req.Raw)
	if err != nil {
		return nil, err
	}
	if err := si.mustMatchStored(rec.ID, rec.Slug, rec.Body.Created); err != nil {
		return nil, err
	}
	if err := requireSignedItemIDs(req.Items); err != nil {
		return nil, err
	}
	if err := e.verifyPlaylistCuratorSignatures(req.Raw, req.Signatures, req.Curators); err != nil {
		return nil, fmt.Errorf("curator signature verification: %w", err)
	}
	signed, pl, err := e.signAndValidatePlaylist(req.Raw, time.Now())
	if err != nil {
		return nil, err
	}
	if err := e.store.UpdatePlaylist(ctx, idOrSlug, signed); err != nil {
		return nil, err
	}
	return &store.PlaylistRecord{ID: rec.ID, Slug: slugOr(pl.Slug, rec.Slug), Raw: signed, Body: *pl}, nil
}

// UpdatePlaylist performs a partial update (API-key path only): merges non-nil fields from req with the
// stored playlist, then signs, validates, and stores. Refused for documents with foreign signatures.
func (e *impl) UpdatePlaylist(ctx context.Context, idOrSlug string, req *models.PlaylistUpdateRequest) (*store.PlaylistRecord, error) {
	// 1. Fetch existing playlist once.
	rec, err := e.store.GetPlaylist(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	if err := e.requireFeedOwned(rec.Body.Signatures); err != nil {
		return nil, err
	}
	existing := &rec.Body

	// 2. Merge patch fields with existing playlist.
	mergedReq := &models.PlaylistReplaceRequest{
		DPVersion:    existing.DPVersion,
		Title:        existing.Title,
		Slug:         existing.Slug,
		Items:        existing.Items,
		Curators:     existing.Curators,
		Summary:      existing.Summary,
		CoverImage:   existing.CoverImage,
		Defaults:     existing.Defaults,
		DynamicQuery: existing.DynamicQuery,
		Note:         existing.Note,
	}

	if req.DPVersion != nil {
		mergedReq.DPVersion = *req.DPVersion
	}
	if req.Title != nil {
		mergedReq.Title = *req.Title
	}
	if req.Slug != nil {
		mergedReq.Slug = *req.Slug
	}
	if req.Items != nil {
		mergedReq.Items = req.Items
	}
	if req.Curators != nil {
		mergedReq.Curators = req.Curators
	}
	if req.Summary != nil {
		mergedReq.Summary = *req.Summary
	}
	if req.CoverImage != nil {
		mergedReq.CoverImage = *req.CoverImage
	}
	if req.Defaults != nil {
		mergedReq.Defaults = req.Defaults
	}
	if req.DynamicQuery != nil {
		mergedReq.DynamicQuery = req.DynamicQuery
	}
	if req.Note != nil {
		mergedReq.Note = req.Note
	}

	// 3. Build the new playlist document: stable id + slug from merged Slug/Title + stored "created".
	created, err := parseDocumentCreated(rec.Body.Created)
	if err != nil {
		return nil, err
	}
	slug := makeSlug(mergedReq.Slug, mergedReq.Title, rec.ID, "playlist")
	raw, err := e.buildPlaylistDocument(mergedReq, rec.ID, slug, created)
	if err != nil {
		return nil, err
	}

	// 4. Sign, validate, persist (the store rebuilds playlist_item_index from items[]).
	signed, pl, err := e.signAndValidatePlaylist(raw, time.Now())
	if err != nil {
		return nil, err
	}
	if err := e.store.UpdatePlaylist(ctx, idOrSlug, signed); err != nil {
		return nil, err
	}
	return &store.PlaylistRecord{ID: rec.ID, Slug: slugOr(pl.Slug, rec.Slug), Raw: signed, Body: *pl}, nil
}

// DeletePlaylist removes a playlist.
func (e *impl) DeletePlaylist(ctx context.Context, idOrSlug string) error {
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

// signAndValidatePlaylistGroup appends the feed signature and validates the result (the playlist-group
// schema requires signatures, so unlike core playlists there is no pre-sign schema pass).
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

// CreatePlaylistGroup resolves playlist URIs (parallel fetch or local GET), signs the group document,
// validates it, and commits upserted playlists, the group row, and membership in one transaction.
// Either write path (see the Executor contract).
func (e *impl) CreatePlaylistGroup(ctx context.Context, req *models.PlaylistGroupCreateRequest) (*store.PlaylistGroupRecord, error) {
	uris := req.Playlists

	// 1. Resolve every URI to stored playlist rows (parallel), preserving order for membership and FK targets.
	ingested, err := e.resolvePlaylistURIs(ctx, uris)
	if err != nil {
		return nil, err
	}

	if len(req.Signatures) > 0 {
		si, err := newSignedIdentity(req.ID, req.Created, req.Slug, req.Raw)
		if err != nil {
			return nil, err
		}
		if err := e.verifyPlaylistGroupCuratorSignatures(req.Raw, req.Signatures, req.Curator); err != nil {
			return nil, fmt.Errorf("curator signature verification: %w", err)
		}
		signed, group, err := e.signAndValidatePlaylistGroup(req.Raw, si.created)
		if err != nil {
			return nil, err
		}
		if err := e.store.CreatePlaylistGroup(ctx, &store.PlaylistGroupInput{ID: si.id, Slug: si.slug, Raw: signed, Playlists: ingested}); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		return &store.PlaylistGroupRecord{ID: si.id, Slug: si.slug, Raw: signed, Body: *group}, nil
	}

	// API-key path.
	id, err := resolveOptionalCreateID(req.ID)
	if err != nil {
		return nil, err
	}
	created, err := resolveOptionalCreateCreated(req.Created)
	if err != nil {
		return nil, err
	}
	slug := makeSlug(req.Slug, req.Title, id, "group")
	raw, err := e.buildPlaylistGroupDocument(req, uris, id, slug, created)
	if err != nil {
		return nil, err
	}
	signed, group, err := e.signAndValidatePlaylistGroup(raw, created)
	if err != nil {
		return nil, err
	}
	if err := e.store.CreatePlaylistGroup(ctx, &store.PlaylistGroupInput{ID: id, Slug: slug, Raw: signed, Playlists: ingested}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return &store.PlaylistGroupRecord{ID: id, Slug: slug, Raw: signed, Body: *group}, nil
}

// GetPlaylistGroup returns the stored playlist-group for id or slug.
func (e *impl) GetPlaylistGroup(ctx context.Context, idOrSlug string) (*store.PlaylistGroupRecord, error) {
	return e.store.GetPlaylistGroup(ctx, idOrSlug)
}

// ListPlaylistGroups returns one page of stored playlist-groups.
func (e *impl) ListPlaylistGroups(ctx context.Context, limit int, cursor string, sort store.SortOrder) ([]store.PlaylistGroupRecord, string, error) {
	return e.store.ListPlaylistGroups(ctx, &store.ListPlaylistsParams{
		Limit:  limit,
		Cursor: cursor,
		Sort:   sort,
	})
}

// ReplacePlaylistGroup re-resolves playlist URIs and commits an update like CreatePlaylistGroup.
// Either write path (see the Executor contract and ReplacePlaylist for the identity rules).
func (e *impl) ReplacePlaylistGroup(ctx context.Context, idOrSlug string, req *models.PlaylistGroupReplaceRequest) (*store.PlaylistGroupRecord, error) {
	// 1. Get the existing playlist-group row.
	rec, err := e.store.GetPlaylistGroup(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	uris := req.Playlists

	// 2. Fresh fetch/lookup for every URI; membership rows are replaced in the same store transaction.
	ingested, err := e.resolvePlaylistURIs(ctx, uris)
	if err != nil {
		return nil, err
	}

	if len(req.Signatures) > 0 {
		si, err := newSignedIdentity(req.ID, req.Created, req.Slug, req.Raw)
		if err != nil {
			return nil, err
		}
		if err := si.mustMatchStored(rec.ID, rec.Slug, rec.Body.Created); err != nil {
			return nil, err
		}
		if err := e.verifyPlaylistGroupCuratorSignatures(req.Raw, req.Signatures, req.Curator); err != nil {
			return nil, fmt.Errorf("curator signature verification: %w", err)
		}
		signed, group, err := e.signAndValidatePlaylistGroup(req.Raw, time.Now())
		if err != nil {
			return nil, err
		}
		if err := e.store.UpdatePlaylistGroup(ctx, idOrSlug, &store.PlaylistGroupInput{Raw: signed, Playlists: ingested}); err != nil {
			return nil, fmt.Errorf("store: %w", err)
		}
		return &store.PlaylistGroupRecord{ID: rec.ID, Slug: slugOr(group.Slug, rec.Slug), Raw: signed, Body: *group}, nil
	}

	// API-key path.
	if err := e.requireFeedOwned(rec.Body.Signatures); err != nil {
		return nil, err
	}
	created, err := parseDocumentCreated(rec.Body.Created)
	if err != nil {
		return nil, err
	}
	slug := makeSlug(req.Slug, req.Title, rec.ID, "group")
	raw, err := e.buildPlaylistGroupDocument(req, uris, rec.ID, slug, created)
	if err != nil {
		return nil, err
	}
	signed, group, err := e.signAndValidatePlaylistGroup(raw, time.Now())
	if err != nil {
		return nil, err
	}
	if err := e.store.UpdatePlaylistGroup(ctx, idOrSlug, &store.PlaylistGroupInput{Raw: signed, Playlists: ingested}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return &store.PlaylistGroupRecord{ID: rec.ID, Slug: slugOr(group.Slug, rec.Slug), Raw: signed, Body: *group}, nil
}

// UpdatePlaylistGroup performs a partial update (API-key path only): merges non-nil fields from req with
// the stored group, re-resolves URIs, re-signs, and updates. Refused for documents with foreign signatures.
func (e *impl) UpdatePlaylistGroup(ctx context.Context, idOrSlug string, req *models.PlaylistGroupUpdateRequest) (*store.PlaylistGroupRecord, error) {
	// 1. Fetch existing playlist-group once.
	rec, err := e.store.GetPlaylistGroup(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	if err := e.requireFeedOwned(rec.Body.Signatures); err != nil {
		return nil, err
	}
	existing := &rec.Body

	// 2. Merge patch fields with existing group.
	mergedReq := &models.PlaylistGroupReplaceRequest{
		Title:      existing.Title,
		Slug:       existing.Slug,
		Playlists:  existing.Playlists,
		Curator:    existing.Curator,
		Summary:    existing.Summary,
		CoverImage: existing.CoverImage,
	}

	if req.Title != nil {
		mergedReq.Title = *req.Title
	}
	if req.Slug != nil {
		mergedReq.Slug = *req.Slug
	}
	if req.Playlists != nil {
		mergedReq.Playlists = req.Playlists
	}
	if req.Curator != nil {
		mergedReq.Curator = *req.Curator
	}
	if req.Summary != nil {
		mergedReq.Summary = *req.Summary
	}
	if req.CoverImage != nil {
		mergedReq.CoverImage = *req.CoverImage
	}

	// 3. Resolve playlist URIs from merged request.
	uris := mergedReq.Playlists
	ingested, err := e.resolvePlaylistURIs(ctx, uris)
	if err != nil {
		return nil, err
	}

	// 4. Build the group document: stable id + slug from merged Slug/Title + stored "created".
	created, err := parseDocumentCreated(rec.Body.Created)
	if err != nil {
		return nil, err
	}
	slug := makeSlug(mergedReq.Slug, mergedReq.Title, rec.ID, "group")
	raw, err := e.buildPlaylistGroupDocument(mergedReq, uris, rec.ID, slug, created)
	if err != nil {
		return nil, err
	}

	// 5. Sign, validate, persist.
	signed, group, err := e.signAndValidatePlaylistGroup(raw, time.Now())
	if err != nil {
		return nil, err
	}
	if err := e.store.UpdatePlaylistGroup(ctx, idOrSlug, &store.PlaylistGroupInput{Raw: signed, Playlists: ingested}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return &store.PlaylistGroupRecord{ID: rec.ID, Slug: slugOr(group.Slug, rec.Slug), Raw: signed, Body: *group}, nil
}

// DeletePlaylistGroup removes a playlist-group.
func (e *impl) DeletePlaylistGroup(ctx context.Context, idOrSlug string) error {
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

// signAndValidateChannel appends the feed signature and validates the result (channels schema requires signatures).
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

// CreateChannel resolves playlist URIs, signs the channel document, validates it, and commits in one transaction.
// Either write path (see the Executor contract).
func (e *impl) CreateChannel(ctx context.Context, req *models.ChannelCreateRequest) (*store.ChannelRecord, error) {
	if !e.extensionsEnabled {
		return nil, ErrExtensionsDisabled
	}
	uris := req.Playlists

	// 1. Resolve every URI to stored playlist rows (parallel), preserving order for membership and FK targets.
	ingested, err := e.resolvePlaylistURIs(ctx, uris)
	if err != nil {
		return nil, err
	}

	var id uuid.UUID
	var slug string
	var signed []byte
	var ch *channels.Channel

	if len(req.Signatures) > 0 {
		si, err := newSignedIdentity(req.ID, req.Created, req.Slug, req.Raw)
		if err != nil {
			return nil, err
		}
		if err := e.verifyChannelPublisherSignatures(req.Raw, req.Signatures, req.Publisher); err != nil {
			return nil, fmt.Errorf("publisher signature verification: %w", err)
		}
		if signed, ch, err = e.signAndValidateChannel(req.Raw, si.created); err != nil {
			return nil, err
		}
		id, slug = si.id, si.slug
	} else {
		// API-key path.
		if id, err = resolveOptionalCreateID(req.ID); err != nil {
			return nil, err
		}
		created, err := resolveOptionalCreateCreated(req.Created)
		if err != nil {
			return nil, err
		}
		slug = makeSlug(req.Slug, req.Title, id, "channel")
		raw, err := e.buildChannelDocument(req, uris, id, slug, created)
		if err != nil {
			return nil, err
		}
		if signed, ch, err = e.signAndValidateChannel(raw, created); err != nil {
			return nil, err
		}
	}

	if err := e.runChannelMutation(ctx, func(mutationCtx context.Context) error {
		return e.store.CreateChannel(mutationCtx, &store.ChannelInput{ID: id, Slug: slug, Raw: signed, Playlists: ingested})
	}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	e.notifyChannel(ctx, notification.ChannelAdded, id)
	return &store.ChannelRecord{ID: id, Slug: slug, Raw: signed, Body: *ch}, nil
}

// GetChannel returns the stored channel for id or slug.
func (e *impl) GetChannel(ctx context.Context, idOrSlug string) (*store.ChannelRecord, error) {
	if !e.extensionsEnabled {
		return nil, ErrExtensionsDisabled
	}
	return e.store.GetChannel(ctx, idOrSlug)
}

// ListChannels returns one page of stored channels.
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

// ReplaceChannel re-resolves playlist URIs and commits a channel update like CreateChannel.
// Either write path (see the Executor contract and ReplacePlaylist for the identity rules).
func (e *impl) ReplaceChannel(ctx context.Context, idOrSlug string, req *models.ChannelReplaceRequest) (*store.ChannelRecord, error) {
	if !e.extensionsEnabled {
		return nil, ErrExtensionsDisabled
	}

	// 1. Get the existing channel row.
	rec, err := e.store.GetChannel(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	uris := req.Playlists

	// 2. Fresh fetch/lookup for every URI; membership rows are replaced in the same store transaction.
	ingested, err := e.resolvePlaylistURIs(ctx, uris)
	if err != nil {
		return nil, err
	}

	var signed []byte
	var ch *channels.Channel
	if len(req.Signatures) > 0 {
		si, err := newSignedIdentity(req.ID, req.Created, req.Slug, req.Raw)
		if err != nil {
			return nil, err
		}
		if err := si.mustMatchStored(rec.ID, rec.Slug, rec.Body.Created); err != nil {
			return nil, err
		}
		if err := e.verifyChannelPublisherSignatures(req.Raw, req.Signatures, req.Publisher); err != nil {
			return nil, fmt.Errorf("publisher signature verification: %w", err)
		}
		if signed, ch, err = e.signAndValidateChannel(req.Raw, time.Now()); err != nil {
			return nil, err
		}
	} else {
		// API-key path.
		if err := e.requireFeedOwned(rec.Body.Signatures); err != nil {
			return nil, err
		}
		created, err := parseDocumentCreated(rec.Body.Created)
		if err != nil {
			return nil, err
		}
		slug := makeSlug(req.Slug, req.Title, rec.ID, "channel")
		raw, err := e.buildChannelDocument(req, uris, rec.ID, slug, created)
		if err != nil {
			return nil, err
		}
		if signed, ch, err = e.signAndValidateChannel(raw, time.Now()); err != nil {
			return nil, err
		}
	}

	// Write by UUID so the committed row and the notification identity cannot diverge on slug reuse.
	if err := e.runChannelMutation(ctx, func(mutationCtx context.Context) error {
		return e.store.UpdateChannel(mutationCtx, rec.ID.String(), &store.ChannelInput{Raw: signed, Playlists: ingested})
	}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	e.notifyChannel(ctx, notification.ChannelUpdated, rec.ID)
	return &store.ChannelRecord{ID: rec.ID, Slug: slugOr(ch.Slug, rec.Slug), Raw: signed, Body: *ch}, nil
}

// UpdateChannel performs a partial update (API-key path only): merges non-nil fields from req with the
// stored channel, re-resolves URIs, re-signs, and updates. Refused for documents with foreign signatures.
func (e *impl) UpdateChannel(ctx context.Context, idOrSlug string, req *models.ChannelUpdateRequest) (*store.ChannelRecord, error) {
	if !e.extensionsEnabled {
		return nil, ErrExtensionsDisabled
	}

	// 1. Fetch existing channel once.
	rec, err := e.store.GetChannel(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	if err := e.requireFeedOwned(rec.Body.Signatures); err != nil {
		return nil, err
	}
	existing := &rec.Body

	// 2. Merge patch fields with existing channel.
	mergedReq := &models.ChannelReplaceRequest{
		Title:      existing.Title,
		Slug:       existing.Slug,
		Version:    existing.Version,
		Playlists:  existing.Playlists,
		Curators:   existing.Curators,
		Publisher:  existing.Publisher,
		Summary:    existing.Summary,
		CoverImage: existing.CoverImage,
	}

	if req.Title != nil {
		mergedReq.Title = *req.Title
	}
	if req.Slug != nil {
		mergedReq.Slug = *req.Slug
	}
	if req.Version != nil {
		mergedReq.Version = *req.Version
	}
	if req.Playlists != nil {
		mergedReq.Playlists = req.Playlists
	}
	if req.Curators != nil {
		mergedReq.Curators = req.Curators
	}
	if req.Publisher != nil {
		mergedReq.Publisher = req.Publisher
	}
	if req.Summary != nil {
		mergedReq.Summary = *req.Summary
	}
	if req.CoverImage != nil {
		mergedReq.CoverImage = *req.CoverImage
	}

	// 3. Resolve playlist URIs from merged request.
	uris := mergedReq.Playlists
	ingested, err := e.resolvePlaylistURIs(ctx, uris)
	if err != nil {
		return nil, err
	}

	// 4. Build the channel document: stable id + slug from merged Slug/Title + stored "created".
	created, err := parseDocumentCreated(rec.Body.Created)
	if err != nil {
		return nil, err
	}
	slug := makeSlug(mergedReq.Slug, mergedReq.Title, rec.ID, "channel")
	raw, err := e.buildChannelDocument(mergedReq, uris, rec.ID, slug, created)
	if err != nil {
		return nil, err
	}

	// 5. Sign, validate, persist (by UUID; see ReplaceChannel).
	signed, ch, err := e.signAndValidateChannel(raw, time.Now())
	if err != nil {
		return nil, err
	}
	if err := e.runChannelMutation(ctx, func(mutationCtx context.Context) error {
		return e.store.UpdateChannel(mutationCtx, rec.ID.String(), &store.ChannelInput{Raw: signed, Playlists: ingested})
	}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	e.notifyChannel(ctx, notification.ChannelUpdated, rec.ID)
	return &store.ChannelRecord{ID: rec.ID, Slug: slugOr(ch.Slug, rec.Slug), Raw: signed, Body: *ch}, nil
}

// DeleteChannel removes a channel.
func (e *impl) DeleteChannel(ctx context.Context, idOrSlug string) error {
	if !e.extensionsEnabled {
		return ErrExtensionsDisabled
	}
	rec, err := e.store.GetChannel(ctx, idOrSlug)
	if err != nil {
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
func (e *impl) GetChannelRegistry(ctx context.Context) ([]store.RegistryPublisher, []store.RegistryPublisherChannel, error) {
	return e.store.GetChannelRegistry(ctx)
}

// ReplaceChannelRegistry atomically replaces the entire registry.
// Converts API input (publishers with a single ordered URL list) to relational rows with positions.
// Returns total channel count for response.
func (e *impl) ReplaceChannelRegistry(ctx context.Context, req models.ChannelRegistry) (int, error) {
	publishers := make([]store.RegistryPublisher, 0, len(req.Publishers))
	channels := []store.RegistryPublisherChannel{}
	totalChannels := 0

	for pubPos, item := range req.Publishers {
		pubID := uuid.New()
		var didPtr *string
		if d := strings.TrimSpace(item.DID); d != "" {
			didPtr = &d
		}
		publishers = append(publishers, store.RegistryPublisher{
			ID:       pubID,
			Name:     item.Name,
			DID:      didPtr,
			Position: pubPos,
		})

		for chPos, url := range item.ChannelURLs {
			channels = append(channels, store.RegistryPublisherChannel{
				ID:          uuid.New(),
				PublisherID: pubID,
				ChannelURL:  url,
				Position:    chPos,
			})
			totalChannels++
		}
	}

	if err := e.store.ReplaceChannelRegistry(ctx, publishers, channels); err != nil {
		return 0, fmt.Errorf("replace channel registry: %w", err)
	}

	return totalChannels, nil
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

// IsSignedSubmissionError reports whether err is a client-correctable defect in a signed submission
// (missing slug, an item without a UUID id, or a PUT whose document identity does not match the stored resource).
func IsSignedSubmissionError(err error) bool {
	return err != nil && (errors.Is(err, ErrSlugRequired) || errors.Is(err, ErrSignedItemIDRequired) || errors.Is(err, ErrSignedDocumentMismatch))
}

// IsDocumentImmutableError reports whether err is ErrDocumentImmutable.
func IsDocumentImmutableError(err error) bool {
	return err != nil && errors.Is(err, ErrDocumentImmutable)
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

// resolveOptionalCreateID interprets an optional JSON "id" for creates: absent or blank → new UUID;
// otherwise parses the trimmed string as a UUID (same validity rules as trusted-model id fields).
func resolveOptionalCreateID(idStr *string) (uuid.UUID, error) {
	if idStr == nil || strings.TrimSpace(*idStr) == "" {
		return uuid.New(), nil
	}
	id, err := uuid.Parse(strings.TrimSpace(*idStr))
	if err != nil {
		return uuid.UUID{}, fmt.Errorf("%w: %w", ErrInvalidID, err)
	}
	return id, nil
}

// resolveOptionalCreateCreated interprets an optional JSON "created" for creates: absent or blank → now (UTC semantics via callers);
// otherwise parses RFC3339 and rejects future timestamps via [parseUserProvidedCreated].
func resolveOptionalCreateCreated(createdStr *string) (time.Time, error) {
	if createdStr == nil || strings.TrimSpace(*createdStr) == "" {
		return time.Now(), nil
	}
	s := strings.TrimSpace(*createdStr)
	return parseUserProvidedCreated(&s)
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
