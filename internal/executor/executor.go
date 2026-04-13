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
	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// Executor is the feed business logic surface used by HTTP handlers (mock in tests via this interface).
//
// Gomock: generated type mocks.MockExecutor in internal/mocks/executor_mock.go.
// Regenerate all mocks from repository root: go generate ./...
// (directives in internal/mocks/doc.go; uses go tool mockgen from go.mod tools.)
//
// Create/Replace methods return the validated, persisted document (signatures included); HTTP layer JSON-encodes the response.
type Executor interface {
	// CreatePlaylist signs (v1.1+ multisig), validates the signed document, and stores a new playlist.
	CreatePlaylist(ctx context.Context, req *models.PlaylistCreateRequest) (*playlist.Playlist, error)
	// GetPlaylist returns the stored playlist document for id or slug (HTTP layer JSON-encodes the response).
	GetPlaylist(ctx context.Context, idOrSlug string) (*playlist.Playlist, error)
	// ListPlaylists returns one page of playlist bodies and an optional next cursor (optional channel or playlist-group filter; id or slug).
	ListPlaylists(ctx context.Context, limit int, cursor string, sort store.SortOrder, channelFilter, playlistGroupFilter string) ([]playlist.Playlist, string, error)
	// ReplacePlaylist performs a full PUT: sign, validate signed JSON, and update storage.
	ReplacePlaylist(ctx context.Context, idOrSlug string, req *models.PlaylistReplaceRequest) (*playlist.Playlist, error)
	// UpdatePlaylist performs a partial PATCH: merges non-nil fields from req with existing playlist, then signs, validates, and updates storage.
	UpdatePlaylist(ctx context.Context, idOrSlug string, req *models.PlaylistUpdateRequest) (*playlist.Playlist, error)
	// DeletePlaylist removes a playlist row.
	DeletePlaylist(ctx context.Context, idOrSlug string) error

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
	// ReplacePlaylistGroup re-resolves playlist URIs, re-signs, and commits updates in one transaction.
	ReplacePlaylistGroup(ctx context.Context, idOrSlug string, req *models.PlaylistGroupReplaceRequest) (*playlistgroup.Group, error)
	// UpdatePlaylistGroup performs a partial PATCH: merges non-nil fields from req with existing group, then re-resolves URIs, re-signs, and updates.
	UpdatePlaylistGroup(ctx context.Context, idOrSlug string, req *models.PlaylistGroupUpdateRequest) (*playlistgroup.Group, error)
	// DeletePlaylistGroup removes a playlist-group row (membership CASCADE).
	DeletePlaylistGroup(ctx context.Context, idOrSlug string) error

	// CreateChannel resolves playlist URIs, signs the channel document, and commits channel + playlists + membership in one transaction (requires extensions).
	CreateChannel(ctx context.Context, req *models.ChannelCreateRequest) (*channels.Channel, error)
	// GetChannel returns the stored channel document for id or slug (HTTP layer JSON-encodes).
	GetChannel(ctx context.Context, idOrSlug string) (*channels.Channel, error)
	// ListChannels returns one page of channel bodies.
	ListChannels(ctx context.Context, limit int, cursor string, sort store.SortOrder) ([]channels.Channel, string, error)
	// ReplaceChannel re-resolves playlist URIs, re-signs, and commits updates in one transaction.
	ReplaceChannel(ctx context.Context, idOrSlug string, req *models.ChannelReplaceRequest) (*channels.Channel, error)
	// UpdateChannel performs a partial PATCH: merges non-nil fields from req with existing channel, then re-resolves URIs, re-signs, and updates.
	UpdateChannel(ctx context.Context, idOrSlug string, req *models.ChannelUpdateRequest) (*channels.Channel, error)
	// DeleteChannel removes a channel row (membership CASCADE).
	DeleteChannel(ctx context.Context, idOrSlug string) error

	// GetChannelRegistry returns the curated channel registry as ordered publisher items.
	GetChannelRegistry(ctx context.Context) ([]store.RegistryPublisher, []store.RegistryPublisherChannel, error)
	// ReplaceChannelRegistry atomically replaces the entire registry; returns total channel count.
	ReplaceChannelRegistry(ctx context.Context, req models.RegistryUpdateRequest) (int, error)

	// APIInfo returns deployment metadata for GET /api/v1.
	APIInfo(version string) map[string]any
}

// impl is the concrete Executor: coordinates store, dp1-go validation/signing, optional HTTP fetch, and publicBaseURL for local playlist URLs.
type impl struct {
	store             store.Store
	dp1               dp1svc.ValidatorSigner
	extensionsEnabled bool
	fetch             fetcher.Fetcher
	publicBase        string
}

// New constructs an Executor. If extensionsEnabled is true, playlist validation and channel APIs use registry/extension rules.
// fetch may be nil; external playlist URLs in groups/channels then fail unless they match publicBaseURL as local /api/v1/playlists/{idOrSlug}.
func New(st store.Store, dp dp1svc.ValidatorSigner, extensionsEnabled bool, fetch fetcher.Fetcher, publicBaseURL string) Executor {
	return &impl{
		store:             st,
		dp1:               dp,
		extensionsEnabled: extensionsEnabled,
		fetch:             fetch,
		publicBase:        strings.TrimSpace(publicBaseURL),
	}
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

// CreatePlaylist builds a playlist document, signs with v1.1+ multisig, validates the signed JSON, then persists.
// Validation runs only after signing so the payload includes signatures (or legacy signature) as required by the schema.
//
// Trusted model: Accepts either API key (ops) or cryptographic signatures (user) authentication.
// - API key path: server generates id, slug, created; signs with feed role
// - Signature path: user provides id, slug, created, signatures[]; server verifies curator signatures and adds feed signature
func (e *impl) CreatePlaylist(ctx context.Context, req *models.PlaylistCreateRequest) (*playlist.Playlist, error) {
	var id uuid.UUID
	var slug string
	var created time.Time
	var raw []byte
	var err error

	// Determine authentication mode and validate accordingly
	if len(req.Signatures) > 0 {
		// Path B: Signature-based authentication (user path)
		// User provides complete document with id, created, and curator signatures
		id, err = parseUserProvidedID(req.ID)
		if err != nil {
			return nil, err
		}

		created, err = parseUserProvidedCreated(req.Created)
		if err != nil {
			return nil, err
		}

		// Generate slug if not provided
		if req.Slug == "" {
			slug = e.makePlaylistSlug(req, id)
		} else {
			slug = slugify(req.Slug)
		}

		// Build document with user-provided fields
		raw, err = e.buildPlaylistDocument(req, id, slug, created)
		if err != nil {
			return nil, err
		}

		// Verify user-provided curator signatures
		if err := e.verifyPlaylistCuratorSignatures(raw, req.Signatures, req.Curators); err != nil {
			return nil, fmt.Errorf("curator signature verification: %w", err)
		}
	} else {
		// Path A: API key authentication (ops path)
		// Server generates id, slug, created
		id = uuid.New()
		created = time.Now()
		slug = e.makePlaylistSlug(req, id)

		raw, err = e.buildPlaylistDocument(req, id, slug, created)
		if err != nil {
			return nil, err
		}
	}

	// ALWAYS sign with feed role (both paths)
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
	return json.Marshal(&p)
}

// makePlaylistSlug returns client slug if set (slugified), else title-based slug with short id suffix for uniqueness.
func (e *impl) makePlaylistSlug(req *models.PlaylistCreateRequest, id uuid.UUID) string {
	if s := strings.TrimSpace(req.Slug); s != "" {
		return slugify(s)
	}
	base := slugify(req.Title)
	if base == "" {
		base = "playlist"
	}
	return fmt.Sprintf("%s-%s", base, id.String()[:8])
}

// slugify lowercases, replaces non-alphanumeric runs with '-', trims edges (empty → "").
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return ""
	}
	return s
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

// ReplacePlaylist replaces a playlist by id/slug (full body); id and slug come from the row; "created" in JSON follows the stored document.
func (e *impl) ReplacePlaylist(ctx context.Context, idOrSlug string, req *models.PlaylistReplaceRequest) (*playlist.Playlist, error) {
	// 1) Get the existing playlist row.
	rec, err := e.store.GetPlaylist(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}

	// 2) Build the new playlist document.
	created, err := parseDocumentCreated(rec.Body.Created)
	if err != nil {
		return nil, err
	}
	raw, err := e.buildPlaylistDocument(req, rec.ID, rec.Slug, created)
	if err != nil {
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

// UpdatePlaylist performs a partial update: merges non-nil fields from req with existing playlist, then signs, validates, and stores.
func (e *impl) UpdatePlaylist(ctx context.Context, idOrSlug string, req *models.PlaylistUpdateRequest) (*playlist.Playlist, error) {
	// 1. Fetch existing playlist once.
	rec, err := e.store.GetPlaylist(ctx, idOrSlug)
	if err != nil {
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

	// 3. Build the new playlist document using the existing record's id, slug, and document "created".
	created, err := parseDocumentCreated(rec.Body.Created)
	if err != nil {
		return nil, err
	}
	raw, err := e.buildPlaylistDocument(mergedReq, rec.ID, rec.Slug, created)
	if err != nil {
		return nil, err
	}

	// 4. Sign with v1.1+ multisig (feed role).
	signed, err := e.dp1.SignPlaylist(raw, time.Now())
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}

	// 5. Validate signed document (schema + §7.1 payload rules) and obtain typed playlist (dp1-go parse path).
	pl, err := e.parseValidatedPlaylist(signed)
	if err != nil {
		return nil, fmt.Errorf("post-sign validation: %w", err)
	}
	if pl == nil {
		return nil, fmt.Errorf("post-sign validation: nil playlist")
	}

	// 6. Persist validated document; DB also builds playlist_item_index from items[].
	if err := e.store.UpdatePlaylist(ctx, idOrSlug, pl); err != nil {
		return nil, err
	}
	return pl, nil
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
	return json.Marshal(&g)
}

// makeGroupSlug mirrors makeSlug for groups (optional client slug, else title- or "group"-based with id suffix).
func (e *impl) makeGroupSlug(req *models.PlaylistGroupCreateRequest, id uuid.UUID) string {
	if s := strings.TrimSpace(req.Slug); s != "" {
		return slugify(s)
	}
	base := slugify(req.Title)
	if base == "" {
		base = "group"
	}
	return fmt.Sprintf("%s-%s", base, id.String()[:8])
}

// makeChannelSlug mirrors makeGroupSlug for channels (optional client slug, else title- or "channel"-based with id suffix).
func (e *impl) makeChannelSlug(req *models.ChannelCreateRequest, id uuid.UUID) string {
	if s := strings.TrimSpace(req.Slug); s != "" {
		if got := slugify(s); got != "" {
			return got
		}
	}
	base := slugify(req.Title)
	if base == "" {
		base = "channel"
	}
	return fmt.Sprintf("%s-%s", base, id.String()[:8])
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

	var id uuid.UUID
	var slug string
	var created time.Time
	var raw []byte

	// Determine authentication mode and validate accordingly
	if len(req.Signatures) > 0 {
		// Path B: Signature-based authentication (user path)
		id, err = parseUserProvidedID(req.ID)
		if err != nil {
			return nil, err
		}

		created, err = parseUserProvidedCreated(req.Created)
		if err != nil {
			return nil, err
		}

		// Generate slug if not provided
		if req.Slug == "" {
			slug = e.makeGroupSlug(req, id)
		} else {
			slug = slugify(req.Slug)
		}

		// Build document with user-provided fields
		raw, err = e.buildPlaylistGroupDocument(req, uris, id, slug, created)
		if err != nil {
			return nil, err
		}

		// Verify user-provided curator signatures
		if err := e.verifyPlaylistGroupCuratorSignatures(raw, req.Signatures, req.Curator); err != nil {
			return nil, fmt.Errorf("curator signature verification: %w", err)
		}
	} else {
		// Path A: API key authentication (ops path)
		id = uuid.New()
		slug = e.makeGroupSlug(req, id)
		created = time.Now()

		raw, err = e.buildPlaylistGroupDocument(req, uris, id, slug, created)
		if err != nil {
			return nil, err
		}
	}

	// ALWAYS sign with feed role (both paths)
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
func (e *impl) ReplacePlaylistGroup(ctx context.Context, idOrSlug string, req *models.PlaylistGroupReplaceRequest) (*playlistgroup.Group, error) {
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

	// 3. Build the group document.
	created, err := parseDocumentCreated(rec.Body.Created)
	if err != nil {
		return nil, err
	}
	raw, err := e.buildPlaylistGroupDocument(req, uris, rec.ID, rec.Slug, created)
	if err != nil {
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

// UpdatePlaylistGroup performs a partial update: merges non-nil fields from req with existing group, then re-resolves URIs, re-signs, and updates.
func (e *impl) UpdatePlaylistGroup(ctx context.Context, idOrSlug string, req *models.PlaylistGroupUpdateRequest) (*playlistgroup.Group, error) {
	// 1. Fetch existing playlist-group once.
	rec, err := e.store.GetPlaylistGroup(ctx, idOrSlug)
	if err != nil {
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

	// 4. Build the group document using the existing record's id, slug, and document "created".
	created, err := parseDocumentCreated(rec.Body.Created)
	if err != nil {
		return nil, err
	}
	raw, err := e.buildPlaylistGroupDocument(mergedReq, uris, rec.ID, rec.Slug, created)
	if err != nil {
		return nil, err
	}

	// 5. Sign with v1.1+ multisig (feed role).
	signed, err := e.dp1.SignPlaylistGroup(raw, time.Now())
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}

	// 6. Validate signed document (playlist-group schema requires signatures) and obtain typed group (dp1-go parse path).
	group, err := e.dp1.ValidatePlaylistGroup(signed)
	if err != nil {
		return nil, fmt.Errorf("post-sign validation: %w", err)
	}
	if group == nil {
		return nil, fmt.Errorf("post-sign validation: nil playlist-group")
	}

	// 7. Persist validated document.
	if err := e.store.UpdatePlaylistGroup(ctx, idOrSlug, &store.PlaylistGroupInput{
		Body:      *group,
		Playlists: ingested,
	}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return group, nil
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
		Slug:      slugify(slug),
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

	var id uuid.UUID
	var slug string
	var created time.Time
	var raw []byte

	// Determine authentication mode and validate accordingly
	if len(req.Signatures) > 0 {
		// Path B: Signature-based authentication (user path)
		id, err = parseUserProvidedID(req.ID)
		if err != nil {
			return nil, err
		}

		created, err = parseUserProvidedCreated(req.Created)
		if err != nil {
			return nil, err
		}

		// Generate slug if not provided
		if req.Slug == "" {
			slug = e.makeChannelSlug(req, id)
		} else {
			slug = slugify(req.Slug)
		}

		// Build document with user-provided fields
		raw, err = e.buildChannelDocument(req, uris, id, slug, created)
		if err != nil {
			return nil, err
		}

		// Verify user-provided publisher signatures
		if err := e.verifyChannelPublisherSignatures(raw, req.Signatures, req.Publisher); err != nil {
			return nil, fmt.Errorf("publisher signature verification: %w", err)
		}
	} else {
		// Path A: API key authentication (ops path)
		id = uuid.New()
		slug = e.makeChannelSlug(req, id)
		created = time.Now()

		raw, err = e.buildChannelDocument(req, uris, id, slug, created)
		if err != nil {
			return nil, err
		}
	}

	// ALWAYS sign with feed role (both paths)
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
	if err := e.store.CreateChannel(ctx, &store.ChannelInput{
		ID:        id,
		Slug:      slug,
		Body:      *ch,
		Playlists: ingested,
	}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
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
func (e *impl) ReplaceChannel(ctx context.Context, idOrSlug string, req *models.ChannelReplaceRequest) (*channels.Channel, error) {
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

	// 3. Build the channel document.
	created, err := parseDocumentCreated(rec.Body.Created)
	if err != nil {
		return nil, err
	}
	raw, err := e.buildChannelDocument(req, uris, rec.ID, rec.Slug, created)
	if err != nil {
		return nil, err
	}

	// 4. Sign with v1.1+ multisig (curator role).
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
	if err := e.store.UpdateChannel(ctx, idOrSlug, &store.ChannelInput{
		Body:      *ch,
		Playlists: ingested,
	}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return ch, nil
}

// UpdateChannel performs a partial update: merges non-nil fields from req with existing channel, then re-resolves URIs, re-signs, and updates.
func (e *impl) UpdateChannel(ctx context.Context, idOrSlug string, req *models.ChannelUpdateRequest) (*channels.Channel, error) {
	if !e.extensionsEnabled {
		return nil, ErrExtensionsDisabled
	}

	// 1. Fetch existing channel once.
	rec, err := e.store.GetChannel(ctx, idOrSlug)
	if err != nil {
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

	// 4. Build the channel document using the existing record's id, slug, and document "created".
	created, err := parseDocumentCreated(rec.Body.Created)
	if err != nil {
		return nil, err
	}
	raw, err := e.buildChannelDocument(mergedReq, uris, rec.ID, rec.Slug, created)
	if err != nil {
		return nil, err
	}

	// 5. Sign with v1.1+ multisig (curator role).
	signed, err := e.dp1.SignChannel(raw, time.Now())
	if err != nil {
		return nil, fmt.Errorf("sign: %w", err)
	}

	// 6. Validate signed document (channels schema requires signatures) and obtain typed channel (dp1-go parse path).
	ch, err := e.dp1.ValidateChannel(signed)
	if err != nil {
		return nil, fmt.Errorf("post-sign validation: %w", err)
	}
	if ch == nil {
		return nil, fmt.Errorf("post-sign validation: nil channel")
	}

	// 7. Persist validated document.
	if err := e.store.UpdateChannel(ctx, idOrSlug, &store.ChannelInput{
		Body:      *ch,
		Playlists: ingested,
	}); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return ch, nil
}

// DeleteChannel removes a channel.
func (e *impl) DeleteChannel(ctx context.Context, idOrSlug string) error {
	if !e.extensionsEnabled {
		return ErrExtensionsDisabled
	}
	return e.store.DeleteChannel(ctx, idOrSlug)
}

// GetChannelRegistry returns the curated channel registry (publishers + channels in order).
func (e *impl) GetChannelRegistry(ctx context.Context) ([]store.RegistryPublisher, []store.RegistryPublisherChannel, error) {
	return e.store.GetChannelRegistry(ctx)
}

// ReplaceChannelRegistry atomically replaces the entire registry.
// Converts API input (array of {name, channel_urls}) to relational rows with positions.
// Returns total channel count for response.
func (e *impl) ReplaceChannelRegistry(ctx context.Context, req models.RegistryUpdateRequest) (int, error) {
	publishers := make([]store.RegistryPublisher, 0, len(req))
	channels := []store.RegistryPublisherChannel{}
	totalChannels := 0

	for pubPos, item := range req {
		pubID := uuid.New()
		publishers = append(publishers, store.RegistryPublisher{
			ID:       pubID,
			Name:     item.Name,
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
