package executor_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	dp1 "github.com/display-protocol/dp1-go"
	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/extension/identity"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/playlistgroup"
	"github.com/display-protocol/dp1-go/sign"

	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/mocks"
	"github.com/display-protocol/dp1-feed-v2/internal/models"
	"github.com/display-protocol/dp1-feed-v2/internal/notification"
	"github.com/display-protocol/dp1-feed-v2/internal/store"
	"github.com/display-protocol/dp1-feed-v2/internal/utils"
)

// Shared test signer identities. Create/replace are authorized by a client signature whose kid matches a
// declared curator (playlist/group) or publisher (channel). The dp1 signature verification is mocked in
// unit tests, so only the kid wiring has to line up.
const (
	testCuratorKid   = "did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"
	testPublisherKid = "did:key:z6MkpubTESTaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testCreatedRFC   = "2026-01-01T00:00:00Z"
)

func testSig(kid string) playlist.Signature {
	return playlist.Signature{
		Alg:         "ed25519",
		Kid:         kid,
		Ts:          testCreatedRFC,
		PayloadHash: "hash",
		Role:        "curator",
		Sig:         "sig",
	}
}

// deleteReq builds a signed delete-intent for a target, signed (in unit tests, mock-verified) by kid.
// created defaults to now so the freshness window passes; callers override it for staleness tests.
func deleteReq(targetType, id, slug, kid string) *models.SignedDeleteRequest {
	r := &models.SignedDeleteRequest{
		Action:     models.DeleteAction,
		Target:     models.DeleteTarget{Type: targetType, ID: id, Slug: slug},
		Created:    time.Now().UTC().Format(time.RFC3339),
		Signatures: []playlist.Signature{testSig(kid)},
	}
	raw, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	r.Raw = raw
	return r
}

// testItemID is a fixed UUID so signed submissions carry a deterministic item id (the feed no longer
// mints one after signing).
const testItemID = "aaaaaaaa-0000-0000-0000-0000000000a1"

func validCreateReq() *models.PlaylistCreateRequest {
	return &models.PlaylistCreateRequest{
		DPVersion: "1.1.0",
		Title:     "Test playlist",
		Slug:      "test-playlist",
		Items: []playlist.PlaylistItem{
			{ID: testItemID, Source: "https://example.com/item"},
		},
		ID:         stringPtr("11111111-1111-1111-1111-111111111111"),
		Created:    stringPtr(testCreatedRFC),
		Curators:   []identity.Entity{{Name: "Curator", Key: testCuratorKid}},
		Signatures: []playlist.Signature{testSig(testCuratorKid)},
	}
}

func mustDecodeJSON[T any](t *testing.T, raw []byte, label string) T {
	t.Helper()
	v, err := utils.DecodeJSONB[T](raw, label)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mustDecodePlaylist(t *testing.T, raw []byte) playlist.Playlist {
	t.Helper()
	return mustDecodeJSON[playlist.Playlist](t, raw, "playlist")
}

func mustDecodeGroup(t *testing.T, raw []byte) playlistgroup.Group {
	t.Helper()
	return mustDecodeJSON[playlistgroup.Group](t, raw, "playlist group")
}

func mustDecodeChannel(t *testing.T, raw []byte) channels.Channel {
	t.Helper()
	return mustDecodeJSON[channels.Channel](t, raw, "channel")
}

func stringPtr(s string) *string {
	return &s
}

func notifiedMutationContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	t.Cleanup(cancel)
	return ctx
}

func displayAtValue(item playlist.PlaylistItem) string {
	if item.DisplayAt == nil {
		return ""
	}
	return *item.DisplayAt
}

type recordingNotificationClient struct {
	events []notification.Event
}

func (c *recordingNotificationClient) Notify(_ context.Context, event notification.Event) error {
	c.events = append(c.events, event)
	return nil
}

type contextRecordingNotificationClient struct {
	contextErr  error
	deadline    time.Time
	hasDeadline bool
	events      []notification.Event
}

func (c *contextRecordingNotificationClient) Notify(ctx context.Context, event notification.Event) error {
	c.contextErr = ctx.Err()
	c.deadline, c.hasDeadline = ctx.Deadline()
	c.events = append(c.events, event)
	return nil
}

// inlineManifestJSON is a minimal DP-1 Ref Manifest carried on an item (playlists extension
// §3.6). The present-but-empty artist id is deliberate: it is the field a decode/re-encode
// round trip would drop, and the bytes are covered by the playlist signature.
func inlineManifestJSON(id string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"refVersion":"1.0.0","id":%q,"created":"2026-08-01T00:00:00Z","locale":"en","metadata":{"title":"Work","artists":[{"id":"","name":"Artist"}],"thumbnails":{"small":{"uri":"https://cdn.example.com/thumb.png"}}}}`, id))
}

// assertInlineManifest compares raw JSON as bytes rather than semantically: up to the point the
// executor hands the document to the signer it only marshals the item struct, so the manifest
// the client sent must still be byte-identical. Signing and storage normalize member order
// later; nothing before them has any reason to touch these bytes.
func assertInlineManifest(t *testing.T, label string, item playlist.PlaylistItem, want json.RawMessage) {
	t.Helper()
	if string(item.InlineManifest) != string(want) {
		t.Fatalf("%s: inlineManifest=%s want %s", label, item.InlineManifest, want)
	}
}

func TestAPIInfo(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), true, nil, "")
	info := e.APIInfo("9.9.9")
	if info["version"] != "9.9.9" {
		t.Fatalf("version: %v", info["version"])
	}
	if info["extensionsEnabled"] != true {
		t.Fatalf("extensionsEnabled: %v", info["extensionsEnabled"])
	}
	e2 := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	if e2.APIInfo("1")["extensionsEnabled"] != false {
		t.Fatal("expected extensions disabled")
	}
}

// =============================================================================
// Playlist Tests
// =============================================================================

func TestCreatePlaylist_success_coreValidation(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	signed := []byte(`{"dpVersion":"1.1.0","title":"x","items":[{"source":"https://x"}]}`)

	parsed := mustDecodePlaylist(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().CreatePlaylist(gomock.Any(), gomock.AssignableToTypeOf(uuid.UUID{}), gomock.Any(), &parsed).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	out, err := e.CreatePlaylist(context.Background(), validCreateReq())
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, parsed) {
		t.Fatalf("body mismatch")
	}
}

func TestCreatePlaylist_success_extensionValidation(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	signed := []byte(`{"ok":true}`)
	parsed := mustDecodePlaylist(t, signed)

	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylistWithExtension(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().CreatePlaylist(gomock.Any(), gomock.AssignableToTypeOf(uuid.UUID{}), gomock.Any(), &parsed).Return(nil)

	e := executor.New(mockStore, mockDP1, true, nil, "")
	_, err := e.CreatePlaylist(context.Background(), validCreateReq())
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreatePlaylist_preservesItemDisplayAtWithExtensions(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	req := &models.PlaylistCreateRequest{
		DPVersion: "1.1.0",
		Title:     "Daily",
		Slug:      "daily",
		Items: []playlist.PlaylistItem{
			{ID: "11111111-0000-0000-0000-000000000001", Source: "https://cdn.example.com/day1.html", DisplayAt: stringPtr("2026-07-21T00:00:00")},
			{ID: "11111111-0000-0000-0000-000000000002", Source: "https://cdn.example.com/day2.html", DisplayAt: stringPtr("2026-07-22T00:00:00Z")},
			{ID: "11111111-0000-0000-0000-000000000003", Source: "https://cdn.example.com/intro.html"},
		},
		ID:         stringPtr("11111111-1111-1111-1111-111111111111"),
		Created:    stringPtr(testCreatedRFC),
		Curators:   []identity.Entity{{Key: testCuratorKid}},
		Signatures: []playlist.Signature{testSig(testCuratorKid)},
	}

	var preSign []byte
	signed := []byte(`{"dpVersion":"1.1.0","title":"Daily"}`)
	parsed := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "Daily",
		Items: []playlist.PlaylistItem{
			{Source: "https://cdn.example.com/day1.html", DisplayAt: stringPtr("2026-07-21T00:00:00")},
			{Source: "https://cdn.example.com/day2.html", DisplayAt: stringPtr("2026-07-22T00:00:00Z")},
			{Source: "https://cdn.example.com/intro.html"},
		},
	}
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).DoAndReturn(func(raw []byte, _ time.Time) ([]byte, error) {
			preSign = append([]byte(nil), raw...)
			return signed, nil
		}),
		mockDP1.EXPECT().ValidatePlaylistWithExtension(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().CreatePlaylist(gomock.Any(), gomock.AssignableToTypeOf(uuid.UUID{}), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ uuid.UUID, _ string, body *playlist.Playlist) error {
			if len(body.Items) != 3 {
				t.Fatalf("store body items: want 3, got %d", len(body.Items))
			}
			if displayAtValue(body.Items[0]) != "2026-07-21T00:00:00" || displayAtValue(body.Items[1]) != "2026-07-22T00:00:00Z" {
				t.Fatalf("store body displayAt: %+v", body.Items)
			}
			if body.Items[2].DisplayAt != nil {
				t.Fatalf("evergreen store item should omit displayAt, got %v", body.Items[2].DisplayAt)
			}
			return nil
		})

	e := executor.New(mockStore, mockDP1, true, nil, "")
	if _, err := e.CreatePlaylist(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	var check playlist.Playlist
	if err := json.Unmarshal(preSign, &check); err != nil {
		t.Fatalf("pre-sign JSON: %v", err)
	}
	if len(check.Items) != 3 {
		t.Fatalf("items: want 3, got %d", len(check.Items))
	}
	if displayAtValue(check.Items[0]) != "2026-07-21T00:00:00" {
		t.Fatalf("item0 displayAt: got %v", check.Items[0].DisplayAt)
	}
	if displayAtValue(check.Items[1]) != "2026-07-22T00:00:00Z" {
		t.Fatalf("item1 displayAt: got %v", check.Items[1].DisplayAt)
	}
	if check.Items[2].DisplayAt != nil {
		t.Fatalf("evergreen item should omit displayAt, got %v", check.Items[2].DisplayAt)
	}
}

func TestCreatePlaylist_preservesItemDisplayAtWithCoreValidation(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	req := validCreateReq()
	req.Items = []playlist.PlaylistItem{
		{ID: testItemID, Source: "https://cdn.example.com/day1.html", DisplayAt: stringPtr("2026-07-21T00:00:00")},
	}
	signed := []byte(`{"dpVersion":"1.1.0","items":[{"source":"https://cdn.example.com/day1.html","displayAt":"2026-07-21T00:00:00"}]}`)
	parsed := mustDecodePlaylist(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().CreatePlaylist(gomock.Any(), gomock.AssignableToTypeOf(uuid.UUID{}), gomock.Any(), &parsed).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	out, err := e.CreatePlaylist(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || displayAtValue(out.Items[0]) != "2026-07-21T00:00:00" {
		t.Fatalf("expected displayAt to be preserved, got %+v", out)
	}
}

func TestCreatePlaylist_preservesItemInlineManifest(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	manifest := inlineManifestJSON("manifest-1")
	req := validCreateReq()
	req.Items = []playlist.PlaylistItem{
		{ID: "11111111-0000-0000-0000-000000000001", Source: "https://cdn.example.com/day1.html", InlineManifest: manifest},
		{ID: "11111111-0000-0000-0000-000000000002", Source: "https://cdn.example.com/day2.html"},
	}

	var preSign []byte
	signed := []byte(`{"dpVersion":"1.1.0","title":"Test playlist"}`)
	parsed := playlist.Playlist{DPVersion: "1.1.0", Title: "Test playlist", Items: req.Items}
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).DoAndReturn(func(raw []byte, _ time.Time) ([]byte, error) {
			preSign = append([]byte(nil), raw...)
			return signed, nil
		}),
		mockDP1.EXPECT().ValidatePlaylistWithExtension(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().CreatePlaylist(gomock.Any(), gomock.AssignableToTypeOf(uuid.UUID{}), gomock.Any(), &parsed).Return(nil)

	e := executor.New(mockStore, mockDP1, true, nil, "")
	if _, err := e.CreatePlaylist(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	var check playlist.Playlist
	if err := json.Unmarshal(preSign, &check); err != nil {
		t.Fatalf("pre-sign JSON: %v", err)
	}
	if len(check.Items) != 2 {
		t.Fatalf("items: want 2, got %d", len(check.Items))
	}
	assertInlineManifest(t, "pre-sign document", check.Items[0], manifest)
	if len(check.Items[1].InlineManifest) != 0 {
		t.Fatalf("item without inlineManifest should omit it, got %s", check.Items[1].InlineManifest)
	}
}

func TestCreatePlaylist_signError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(nil, errors.New("sign failed"))

	e := executor.New(mocks.NewMockStore(ctrl), mockDP1, false, nil, "")
	_, err := e.CreatePlaylist(context.Background(), validCreateReq())
	if err == nil || !strings.Contains(err.Error(), "sign: sign failed") {
		t.Fatalf("got %v", err)
	}
}

func TestCreatePlaylist_postSignValidationError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	signed := []byte(`{}`)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylist(signed).Return(nil, errors.New("post fail")),
	)

	e := executor.New(mocks.NewMockStore(ctrl), mockDP1, false, nil, "")
	_, err := e.CreatePlaylist(context.Background(), validCreateReq())
	if err == nil || !strings.Contains(err.Error(), "post-sign validation: post fail") {
		t.Fatalf("got %v", err)
	}
}

func TestCreatePlaylist_storeError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	signed := []byte(`{"x":1}`)
	decoded := mustDecodePlaylist(t, signed)

	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylist(signed).Return(&decoded, nil),
	)
	mockStore.EXPECT().CreatePlaylist(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(errors.New("db down"))

	e := executor.New(mockStore, mockDP1, false, nil, "")
	_, err := e.CreatePlaylist(context.Background(), validCreateReq())
	if err == nil || !strings.Contains(err.Error(), "store: db down") {
		t.Fatalf("got %v", err)
	}
}

func TestCreatePlaylist_optionalCreate_respectsProvidedID(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	wantID := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	idStr := wantID.String()
	req := validCreateReq()
	req.ID = &idStr

	signed := []byte(`{"dpVersion":"1.1.0","title":"x","items":[{"source":"https://x"}]}`)
	parsed := mustDecodePlaylist(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().CreatePlaylist(gomock.Any(), wantID, gomock.Any(), &parsed).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	if _, err := e.CreatePlaylist(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestCreatePlaylist_optionalCreate_invalidID(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	bad := "not-a-uuid"
	req := validCreateReq()
	req.ID = &bad

	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	_, err := e.CreatePlaylist(context.Background(), req)
	if !executor.IsInvalidIDError(err) {
		t.Fatalf("want invalid id error, got %v", err)
	}
}

func TestCreatePlaylist_optionalCreate_futureCreated(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	future := time.Now().Add(48 * time.Hour).UTC().Format(time.RFC3339)
	req := validCreateReq()
	req.Created = &future

	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	_, err := e.CreatePlaylist(context.Background(), req)
	if !executor.IsInvalidTimestampError(err) {
		t.Fatalf("want invalid timestamp error, got %v", err)
	}
}

func TestCreatePlaylist_optionalCreate_invalidCreated(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	bad := "Tuesday"
	req := validCreateReq()
	req.Created = &bad

	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	_, err := e.CreatePlaylist(context.Background(), req)
	if !executor.IsInvalidTimestampError(err) {
		t.Fatalf("want invalid timestamp error, got %v", err)
	}
}

func TestGetPlaylist(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	pl := mustDecodePlaylist(t, []byte(`{"title":"x"}`))
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "slug-1").Return(&store.PlaylistRecord{Body: pl}, nil)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	out, err := e.GetPlaylist(context.Background(), "slug-1")
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, pl) {
		t.Fatalf("body mismatch: %+v vs %+v", out, pl)
	}
}

func TestGetPlaylist_notFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "nope").Return(nil, fmt.Errorf("%w", store.ErrNotFound))

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	_, err := e.GetPlaylist(context.Background(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestListPlaylists(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	recs := []store.PlaylistRecord{
		{Body: playlist.Playlist{Title: "A"}},
		{Body: playlist.Playlist{Title: "B"}},
	}
	mockStore.EXPECT().ListPlaylists(gomock.Any(), &store.ListPlaylistsParams{
		Limit:  25,
		Cursor: "cur",
		Sort:   store.SortDesc,
	}).Return(recs, "next-page", nil)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	items, next, err := e.ListPlaylists(context.Background(), 25, "cur", store.SortDesc, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if next != "next-page" || len(items) != 2 {
		t.Fatalf("next=%q len=%d", next, len(items))
	}
	if len(recs) != 2 {
		t.Fatalf("recs len=%d", len(recs))
	}
	if !reflect.DeepEqual(items[0], recs[0].Body) || !reflect.DeepEqual(items[1], recs[1].Body) {
		t.Fatalf("items mismatch: %+v %+v", items[0], items[1])
	}
}

func TestListPlaylists_filters(t *testing.T) {
	t.Parallel()

	errStoreUnavailable := errors.New("db unavailable")

	wantParams := func(limit int, cursor string, sort store.SortOrder, ch, pg string) *store.ListPlaylistsParams {
		return &store.ListPlaylistsParams{
			Limit:               limit,
			Cursor:              cursor,
			Sort:                sort,
			ChannelFilter:       ch,
			PlaylistGroupFilter: pg,
		}
	}

	tests := []struct {
		name           string
		extEnabled     bool
		ch             string
		pg             string
		setupMock      func(*mocks.MockStore) // nil when the store must not be called
		wantErr        error
		wantItems      int
		wantNext       string
		wantFirstTitle string
	}{
		{
			name:       "channel_filter_extensions_disabled_returns_without_calling_store",
			extEnabled: false,
			ch:         "any-channel",
			pg:         "",
			setupMock:  nil,
			wantErr:    executor.ErrExtensionsDisabled,
		},
		{
			name:       "channel_filter_forwards_to_store_when_extensions_enabled",
			extEnabled: true,
			ch:         "my-channel",
			pg:         "",
			setupMock: func(m *mocks.MockStore) {
				m.EXPECT().ListPlaylists(gomock.Any(), wantParams(10, "", store.SortAsc, "my-channel", "")).
					Return([]store.PlaylistRecord{{Body: playlist.Playlist{Title: "In Channel"}}}, "next-c", nil)
			},
			wantErr:        nil,
			wantItems:      1,
			wantNext:       "next-c",
			wantFirstTitle: "In Channel",
		},
		{
			name:       "playlist_group_filter_forwards_when_extensions_disabled",
			extEnabled: false,
			ch:         "",
			pg:         "my-group",
			setupMock: func(m *mocks.MockStore) {
				m.EXPECT().ListPlaylists(gomock.Any(), wantParams(10, "", store.SortAsc, "", "my-group")).
					Return([]store.PlaylistRecord{{Body: playlist.Playlist{Title: "In Group"}}}, "", nil)
			},
			wantErr:        nil,
			wantItems:      1,
			wantNext:       "",
			wantFirstTitle: "In Group",
		},
		{
			name:       "both_channel_and_playlist_group_filters_forwarded_to_store",
			extEnabled: true,
			ch:         "ch-slug",
			pg:         "grp-slug",
			setupMock: func(m *mocks.MockStore) {
				// HTTP rejects both query params together; executor still forwards if a caller passes both (e.g. tests or future RPC).
				m.EXPECT().ListPlaylists(gomock.Any(), wantParams(10, "", store.SortAsc, "ch-slug", "grp-slug")).
					Return(nil, "", nil)
			},
			wantErr:   nil,
			wantItems: 0,
			wantNext:  "",
		},
		{
			name:       "channel_filter_whitespace_only_does_not_trigger_extensions_gate",
			extEnabled: false,
			ch:         "   ",
			pg:         "",
			setupMock: func(m *mocks.MockStore) {
				m.EXPECT().ListPlaylists(gomock.Any(), wantParams(10, "", store.SortAsc, "   ", "")).
					Return(nil, "", nil)
			},
			wantErr:   nil,
			wantItems: 0,
			wantNext:  "",
		},
		{
			name:       "store_error_propagates_with_channel_filter",
			extEnabled: true,
			ch:         "ch",
			pg:         "",
			setupMock: func(m *mocks.MockStore) {
				m.EXPECT().ListPlaylists(gomock.Any(), wantParams(10, "", store.SortAsc, "ch", "")).
					Return(nil, "", errStoreUnavailable)
			},
			wantErr: errStoreUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			mockStore := mocks.NewMockStore(ctrl)
			if tt.setupMock != nil {
				tt.setupMock(mockStore)
			}

			e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), tt.extEnabled, nil, "")
			items, next, err := e.ListPlaylists(context.Background(), 10, "", store.SortAsc, tt.ch, tt.pg)

			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err: got %v want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != tt.wantItems {
				t.Fatalf("len(items)=%d want %d", len(items), tt.wantItems)
			}
			if next != tt.wantNext {
				t.Fatalf("next=%q want %q", next, tt.wantNext)
			}
			if tt.wantItems > 0 && items[0].Title != tt.wantFirstTitle {
				t.Fatalf("items[0].Title=%q want %q", items[0].Title, tt.wantFirstTitle)
			}
		})
	}
}

// A create/replace must carry an explicit slug and give every item a UUID id; the feed no longer derives
// them after signing.
func TestCreatePlaylist_missingSlug(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	req := validCreateReq()
	req.Slug = "   "
	if _, err := e.CreatePlaylist(context.Background(), req); !executor.IsInvalidSubmissionError(err) {
		t.Fatalf("want invalid-submission (slug required), got %v", err)
	}
}

func TestCreatePlaylist_itemMissingID(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	req := validCreateReq()
	req.Items = []playlist.PlaylistItem{{Source: "https://example.com/x"}}
	if _, err := e.CreatePlaylist(context.Background(), req); !executor.IsInvalidSubmissionError(err) {
		t.Fatalf("want invalid-submission (item id), got %v", err)
	}
}

func TestReplacePlaylist_itemMissingID(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	// requireItemIDs runs before the store is read, so no GetPlaylist expectation is needed.
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	req := validCreateReq()
	req.Items = []playlist.PlaylistItem{{Source: "https://example.com/x"}}
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", req); !executor.IsInvalidSubmissionError(err) {
		t.Fatalf("want invalid-submission (item id), got %v", err)
	}
}

func TestDeletePlaylist(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	body := playlist.Playlist{ID: id.String(), Slug: "id-1", Curators: []identity.Entity{{Key: testCuratorKid}}}
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(&store.PlaylistRecord{ID: id, Slug: "id-1", Body: body}, nil)
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil)
	mockStore.EXPECT().DeletePlaylist(gomock.Any(), id.String()).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.DeleteTargetPlaylist, id.String(), "id-1", testCuratorKid)
	if err := e.DeletePlaylist(context.Background(), "id-1", req); err != nil {
		t.Fatal(err)
	}
}

func TestDeletePlaylist_notOwner(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	body := playlist.Playlist{ID: id.String(), Slug: "id-1", Curators: []identity.Entity{{Key: testCuratorKid}}}
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(&store.PlaylistRecord{ID: id, Slug: "id-1", Body: body}, nil)
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.DeleteTargetPlaylist, id.String(), "id-1", "did:key:someoneElse")
	err := e.DeletePlaylist(context.Background(), "id-1", req)
	if !executor.IsForbiddenError(err) {
		t.Fatalf("want forbidden (not owner), got %v", err)
	}
}

func TestDeletePlaylist_targetMismatch(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	body := playlist.Playlist{ID: id.String(), Slug: "id-1", Curators: []identity.Entity{{Key: testCuratorKid}}}
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(&store.PlaylistRecord{ID: id, Slug: "id-1", Body: body}, nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.DeleteTargetPlaylist, id.String(), "wrong-slug", testCuratorKid)
	err := e.DeletePlaylist(context.Background(), "id-1", req)
	if !executor.IsDeleteRequestError(err) {
		t.Fatalf("want delete-request error (target mismatch), got %v", err)
	}
}

func TestDeletePlaylist_staleTimestamp(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	body := playlist.Playlist{ID: id.String(), Slug: "id-1", Curators: []identity.Entity{{Key: testCuratorKid}}}
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(&store.PlaylistRecord{ID: id, Slug: "id-1", Body: body}, nil)

	e := executor.New(mockStore, mockDP1, false, nil, "", executor.WithDeleteClockSkew(time.Minute))
	req := deleteReq(models.DeleteTargetPlaylist, id.String(), "id-1", testCuratorKid)
	req.Created = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	err := e.DeletePlaylist(context.Background(), "id-1", req)
	if !executor.IsInvalidTimestampError(err) {
		t.Fatalf("want invalid-timestamp error (stale), got %v", err)
	}
}

// storedOwnedPlaylist is the stored record the delete-branch tests target (owner = testCuratorKid).
func storedOwnedPlaylist(id uuid.UUID) *store.PlaylistRecord {
	body := playlist.Playlist{ID: id.String(), Slug: "id-1", Curators: []identity.Entity{{Key: testCuratorKid}}}
	return &store.PlaylistRecord{ID: id, Slug: "id-1", Body: body}
}

func TestDeletePlaylist_wrongAction(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(storedOwnedPlaylist(id), nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.DeleteTargetPlaylist, id.String(), "id-1", testCuratorKid)
	req.Action = "nuke"
	if err := e.DeletePlaylist(context.Background(), "id-1", req); !executor.IsDeleteRequestError(err) {
		t.Fatalf("want delete-request error (bad action), got %v", err)
	}
}

func TestDeletePlaylist_wrongTargetType(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(storedOwnedPlaylist(id), nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.DeleteTargetChannel, id.String(), "id-1", testCuratorKid)
	if err := e.DeletePlaylist(context.Background(), "id-1", req); !executor.IsDeleteRequestError(err) {
		t.Fatalf("want delete-request error (wrong target type), got %v", err)
	}
}

func TestDeletePlaylist_missingSignatures(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(storedOwnedPlaylist(id), nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.DeleteTargetPlaylist, id.String(), "id-1", testCuratorKid)
	req.Signatures = nil
	if err := e.DeletePlaylist(context.Background(), "id-1", req); !executor.IsSignaturesRequiredError(err) {
		t.Fatalf("want signatures-required error, got %v", err)
	}
}

func TestDeletePlaylist_malformedCreated(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(storedOwnedPlaylist(id), nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.DeleteTargetPlaylist, id.String(), "id-1", testCuratorKid)
	req.Created = "nope"
	if err := e.DeletePlaylist(context.Background(), "id-1", req); !executor.IsInvalidTimestampError(err) {
		t.Fatalf("want invalid-timestamp error (malformed created), got %v", err)
	}
}

func TestDeletePlaylist_futureTimestamp(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(storedOwnedPlaylist(id), nil)

	e := executor.New(mockStore, mockDP1, false, nil, "", executor.WithDeleteClockSkew(time.Minute))
	req := deleteReq(models.DeleteTargetPlaylist, id.String(), "id-1", testCuratorKid)
	req.Created = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := e.DeletePlaylist(context.Background(), "id-1", req); !executor.IsInvalidTimestampError(err) {
		t.Fatalf("want invalid-timestamp error (future), got %v", err)
	}
}

func TestDeletePlaylist_verifyError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(storedOwnedPlaylist(id), nil)
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(false, nil, errors.New("boom"))

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.DeleteTargetPlaylist, id.String(), "id-1", testCuratorKid)
	if err := e.DeletePlaylist(context.Background(), "id-1", req); !executor.IsSignatureVerificationError(err) {
		t.Fatalf("want signature-verification error (verify err), got %v", err)
	}
}

func TestDeletePlaylist_verifyFalse(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(storedOwnedPlaylist(id), nil)
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(false, []playlist.Signature{{Kid: "x"}}, nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.DeleteTargetPlaylist, id.String(), "id-1", testCuratorKid)
	if err := e.DeletePlaylist(context.Background(), "id-1", req); !executor.IsSignatureVerificationError(err) {
		t.Fatalf("want signature-verification error (ok=false), got %v", err)
	}
}

func TestDeletePlaylist_missingRaw(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(storedOwnedPlaylist(id), nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	// No Raw set: verifyDeleteIntent rejects a delete-intent whose signed bytes were not captured.
	req := &models.SignedDeleteRequest{
		Action:     models.DeleteAction,
		Target:     models.DeleteTarget{Type: models.DeleteTargetPlaylist, ID: id.String(), Slug: "id-1"},
		Created:    time.Now().UTC().Format(time.RFC3339),
		Signatures: []playlist.Signature{testSig(testCuratorKid)},
	}
	if err := e.DeletePlaylist(context.Background(), "id-1", req); !executor.IsDeleteRequestError(err) {
		t.Fatalf("want delete-request error (missing raw), got %v", err)
	}
}

// TestReplacePlaylist_verifyCryptoFails covers the signatureFailure path when a signature does not verify.
func TestReplacePlaylist_verifyCryptoFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(storedPlaylistRecord(t, id, "keep-me"), nil)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(false, []playlist.Signature{{Kid: "x"}}, nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", validCreateReq()); !executor.IsSignatureVerificationError(err) {
		t.Fatalf("want signature-verification error (ok=false), got %v", err)
	}
}

// TestReplacePlaylist_verifyCryptoError covers the verify-returns-error branch on replace.
func TestReplacePlaylist_verifyCryptoError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(storedPlaylistRecord(t, id, "keep-me"), nil)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(false, nil, errors.New("boom"))

	e := executor.New(mockStore, mockDP1, false, nil, "")
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", validCreateReq()); !executor.IsSignatureVerificationError(err) {
		t.Fatalf("want signature-verification error (verify err), got %v", err)
	}
}

func TestReplacePlaylist_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	existing := []byte(`{"dpVersion":"1.1.0","id":"11111111-1111-1111-1111-111111111111","slug":"keep-me","title":"Old","created":"2020-01-02T03:04:05Z","curators":[{"key":"did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"}],"items":[{"source":"https://old"}]}`)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(&store.PlaylistRecord{
		ID:   id,
		Slug: "keep-me",
		Body: mustDecodePlaylist(t, existing),
	}, nil)

	var preSign []byte
	signed := []byte(`{"replaced":true}`)
	parsed := mustDecodePlaylist(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).DoAndReturn(func(raw []byte, _ time.Time) ([]byte, error) {
			preSign = append([]byte(nil), raw...)
			return signed, nil
		}),
		mockDP1.EXPECT().ValidatePlaylistWithExtension(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), id.String(), &parsed).Return(nil)

	e := executor.New(mockStore, mockDP1, true, nil, "")
	req := validCreateReq()
	req.Title = "New title"
	req.Items = []playlist.PlaylistItem{
		{ID: testItemID, Source: "https://cdn.example.com/day1.html", DisplayAt: stringPtr("2026-07-21T00:00:00")},
	}
	out, err := e.ReplacePlaylist(context.Background(), "keep-me", req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, parsed) {
		t.Fatalf("out mismatch")
	}
	var check playlist.Playlist
	if err := json.Unmarshal(preSign, &check); err != nil {
		t.Fatalf("pre-sign JSON: %v", err)
	}
	if len(check.Items) != 1 || displayAtValue(check.Items[0]) != "2026-07-21T00:00:00" {
		t.Fatalf("replace should keep item displayAt, got %+v", check.Items)
	}
}

func TestReplacePlaylist_withSignatures_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	existing := []byte(`{"dpVersion":"1.1.0","id":"11111111-1111-1111-1111-111111111111","slug":"keep-me","title":"Old","created":"2020-01-02T03:04:05Z","curators":[{"key":"did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"}],"items":[{"source":"https://old"}]}`)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(&store.PlaylistRecord{
		ID:   id,
		Slug: "keep-me",
		Body: mustDecodePlaylist(t, existing),
	}, nil)

	kid := "did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"
	sig := playlist.Signature{Kid: kid, Alg: "ed25519", Sig: "test-sig"}

	signed := []byte(`{"replaced":true}`)
	parsed := mustDecodePlaylist(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil),
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), id.String(), &parsed).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := validCreateReq()
	req.Title = "New title"
	req.Signatures = []playlist.Signature{sig}
	req.Curators = []identity.Entity{{Key: kid}}

	out, err := e.ReplacePlaylist(context.Background(), "keep-me", req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, parsed) {
		t.Fatalf("out mismatch")
	}
}

// storedPlaylistRecord is the stored playlist the replace-deny tests target: owner is testCuratorKid.
func storedPlaylistRecord(t *testing.T, id uuid.UUID, slug string) *store.PlaylistRecord {
	t.Helper()
	existing := []byte(`{"dpVersion":"1.1.0","id":"` + id.String() + `","slug":"` + slug + `","title":"Old","created":"2020-01-02T03:04:05Z","curators":[{"key":"` + testCuratorKid + `"}],"items":[{"source":"https://old"}]}`)
	return &store.PlaylistRecord{ID: id, Slug: slug, Body: mustDecodePlaylist(t, existing)}
}

// TestReplacePlaylist_ownerImmutable: changing the curator (owner) set on a PUT is refused with 403,
// before any signature verification.
func TestReplacePlaylist_ownerImmutable(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(storedPlaylistRecord(t, id, "keep-me"), nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := validCreateReq()
	req.Curators = []identity.Entity{{Key: "did:key:z6MkNewOwnerXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"}}
	req.Signatures = []playlist.Signature{testSig("did:key:z6MkNewOwnerXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX")}

	_, err := e.ReplacePlaylist(context.Background(), "keep-me", req)
	if !executor.IsForbiddenError(err) {
		t.Fatalf("want forbidden (owner immutable), got %v", err)
	}
}

// TestReplacePlaylist_notOwner: a cryptographically valid replace signed only by a non-owner key is
// refused with 403.
func TestReplacePlaylist_notOwner(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(storedPlaylistRecord(t, id, "keep-me"), nil)
	// Signatures verify cryptographically, but none is a stored owner key.
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := validCreateReq() // curators == stored (immutability passes)
	req.Signatures = []playlist.Signature{testSig("did:key:z6MkNotAnOwnerXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX")}

	_, err := e.ReplacePlaylist(context.Background(), "keep-me", req)
	if !executor.IsForbiddenError(err) {
		t.Fatalf("want forbidden (not owner), got %v", err)
	}
}

// TestReplacePlaylistGroup_ownerImmutable: changing the curator on a group PUT is refused with 403.
func TestReplacePlaylistGroup_ownerImmutable(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	existing := []byte(`{"id":"` + id.String() + `","slug":"grp","title":"Old","created":"2020-01-02T03:04:05Z","curator":"` + testCuratorKid + `","playlists":[]}`)
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "grp").Return(&store.PlaylistGroupRecord{ID: id, Slug: "grp", Body: mustDecodeGroup(t, existing)}, nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := validGroupCreateReq()
	req.Curator = "did:key:z6MkNewGroupOwnerXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
	req.Signatures = []playlist.Signature{testSig(req.Curator)}

	_, err := e.ReplacePlaylistGroup(context.Background(), "grp", req)
	if !executor.IsForbiddenError(err) {
		t.Fatalf("want forbidden (group owner immutable), got %v", err)
	}
}

// TestReplaceChannel_ownerImmutable: changing the publisher on a channel PUT is refused with 403.
func TestReplaceChannel_ownerImmutable(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	existing := []byte(`{"id":"` + id.String() + `","slug":"chan","title":"Old","version":"1.0.0","created":"2020-01-02T03:04:05Z","publisher":{"key":"` + testPublisherKid + `"},"playlists":[],"signatures":[{"alg":"ed25519","kid":"` + testPublisherKid + `","ts":"2020-01-02T03:04:05Z","payload_hash":"h","role":"publisher","sig":"s"}]}`)
	mockStore.EXPECT().GetChannel(gomock.Any(), "chan").Return(&store.ChannelRecord{ID: id, Slug: "chan", Body: mustDecodeChannel(t, existing)}, nil)

	e := executor.New(mockStore, mockDP1, true, nil, "")
	req := validChannelCreateReq("chan")
	req.Publisher = &identity.Entity{Key: "did:key:z6MkNewPublisherXXXXXXXXXXXXXXXXXXXXXXXXXXXX"}
	req.Signatures = []playlist.Signature{testSig(req.Publisher.Key)}

	_, err := e.ReplaceChannel(context.Background(), "chan", req)
	if !executor.IsForbiddenError(err) {
		t.Fatalf("want forbidden (channel owner immutable), got %v", err)
	}
}

func TestReplacePlaylist_preservesItemDisplayAtWithCoreValidation(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	existing := []byte(`{"dpVersion":"1.1.0","id":"11111111-1111-1111-1111-111111111111","slug":"daily","title":"Old","created":"2020-01-02T03:04:05Z","curators":[{"key":"did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"}],"items":[{"source":"https://old"}]}`)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "daily").Return(&store.PlaylistRecord{
		ID:   id,
		Slug: "daily",
		Body: mustDecodePlaylist(t, existing),
	}, nil)

	req := validCreateReq()
	req.Items = []playlist.PlaylistItem{
		{ID: testItemID, Source: "https://cdn.example.com/day1.html", DisplayAt: stringPtr("2026-07-21T00:00:00")},
	}
	signed := []byte(`{"dpVersion":"1.1.0","items":[{"source":"https://cdn.example.com/day1.html","displayAt":"2026-07-21T00:00:00"}]}`)
	parsed := mustDecodePlaylist(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), id.String(), &parsed).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	out, err := e.ReplacePlaylist(context.Background(), "daily", req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || displayAtValue(out.Items[0]) != "2026-07-21T00:00:00" {
		t.Fatalf("expected displayAt to be preserved, got %+v", out)
	}
}

func TestReplacePlaylist_notFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "x").Return(nil, store.ErrNotFound)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	_, err := e.ReplacePlaylist(context.Background(), "x", validCreateReq())
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestPlaylist_replace_parseDocumentCreatedFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		created string
	}{
		{"empty", ""},
		{"malformed", "not-a-valid-rfc3339"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			mockStore := mocks.NewMockStore(ctrl)
			id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
			mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{
				ID:   id,
				Slug: "pl",
				Body: playlist.Playlist{
					DPVersion: "1.1.0",
					Title:     "T",
					Created:   tc.created,
					// Owner set must match the request's so the replace passes the owner-immutability
					// check and reaches the created-timestamp parse under test.
					Curators: []identity.Entity{{Key: testCuratorKid}},
					Items:    []playlist.PlaylistItem{{Source: "https://x"}},
				},
			}, nil)

			e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, "")
			_, err := e.ReplacePlaylist(context.Background(), "pl", validCreateReq())
			if err == nil || !strings.Contains(err.Error(), "parse document created") {
				t.Fatalf("expected parse document created error, got %v", err)
			}
		})
	}
}

// =============================================================================
// Playlist Group Tests
// =============================================================================

const testPublicBase = "https://feed.example"

func localPlaylistRef(slug string) string {
	return strings.TrimSuffix(testPublicBase, "/") + "/api/v1/playlists/" + slug
}

func validGroupCreateReq(uris ...string) *models.PlaylistGroupCreateRequest {
	return &models.PlaylistGroupCreateRequest{
		Title:      "Group title",
		Slug:       "group-title",
		Playlists:  uris,
		ID:         stringPtr("33333333-3333-3333-3333-333333333333"),
		Created:    stringPtr(testCreatedRFC),
		Curator:    testCuratorKid,
		Signatures: []playlist.Signature{testSig(testCuratorKid)},
	}
}

func validChannelCreateReq(slug string, uris ...string) *models.ChannelCreateRequest {
	return &models.ChannelCreateRequest{
		Title:      "Channel title",
		Slug:       slug,
		Playlists:  uris,
		ID:         stringPtr("44444444-4444-4444-4444-444444444444"),
		Created:    stringPtr(testCreatedRFC),
		Publisher:  &identity.Entity{Name: "Publisher", Key: testPublisherKid},
		Signatures: []playlist.Signature{testSig(testPublisherKid)},
	}
}

func TestIsExtensionsDisabled(t *testing.T) {
	t.Parallel()
	if !executor.IsExtensionsDisabled(executor.ErrExtensionsDisabled) {
		t.Fatal("expected true for ErrExtensionsDisabled")
	}
	if executor.IsExtensionsDisabled(errors.New("other")) {
		t.Fatal("expected false")
	}
}

func TestCreatePlaylistGroup_success_localResolve(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	plID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	plBody := []byte(`{"id":"22222222-2222-2222-2222-222222222222","slug":"pl-one","title":"P"}`)
	plDoc := mustDecodePlaylist(t, plBody)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl-one").Return(&store.PlaylistRecord{
		ID:   plID,
		Slug: "pl-one",
		Body: plDoc,
	}, nil)

	signed := []byte(`{"kind":"signed-group"}`)
	wantGroup := mustDecodeGroup(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylistGroup(signed).Return(&wantGroup, nil),
	)
	mockStore.EXPECT().CreatePlaylistGroup(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.PlaylistGroupInput) {
		if in.ID == uuid.Nil || in.Slug == "" {
			t.Fatalf("create expects non-zero id and slug, got id=%v slug=%q", in.ID, in.Slug)
		}
		if len(in.Playlists) != 1 || in.Playlists[0].ID != plID || !reflect.DeepEqual(in.Playlists[0].Body, plDoc) {
			t.Fatalf("ingested playlists: %+v", in.Playlists)
		}
		if !reflect.DeepEqual(in.Body, wantGroup) {
			t.Fatalf("body: %+v", in.Body)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	out, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(localPlaylistRef("pl-one")))
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, wantGroup) {
		t.Fatalf("response body mismatch")
	}
}

func TestCreatePlaylistGroup_optionalCreate_respectsProvidedID(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	plID := uuid.MustParse("22222222-2222-4222-a222-222222222222")
	plBody := []byte(`{"id":"22222222-2222-4222-a222-222222222222","slug":"pl-one","title":"P"}`)
	plDoc := mustDecodePlaylist(t, plBody)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl-one").Return(&store.PlaylistRecord{
		ID:   plID,
		Slug: "pl-one",
		Body: plDoc,
	}, nil)

	wantGroupID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	idStr := wantGroupID.String()
	req := validGroupCreateReq(localPlaylistRef("pl-one"))
	req.ID = &idStr

	signed := []byte(`{"kind":"signed-group-ops-id"}`)
	wantGroup := mustDecodeGroup(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylistGroup(signed).Return(&wantGroup, nil),
	)
	mockStore.EXPECT().CreatePlaylistGroup(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.PlaylistGroupInput) {
		if in.ID != wantGroupID {
			t.Fatalf("want group id %v, got %v", wantGroupID, in.ID)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	if _, err := e.CreatePlaylistGroup(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestCreatePlaylistGroup_emptyPlaylists(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	// Authorization precedes URI resolution, so a signed request is verified before the empty-playlists
	// check inside resolvePlaylistURIs is reached.
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil)
	e := executor.New(mocks.NewMockStore(ctrl), mockDP1, false, nil, testPublicBase)
	_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq())
	if err == nil || !strings.Contains(err.Error(), "playlists must be non-empty") {
		t.Fatalf("got %v", err)
	}
}

func TestCreatePlaylistGroup_externalURINoFetcher(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil)
	e := executor.New(mocks.NewMockStore(ctrl), mockDP1, false, nil, testPublicBase)
	_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq("https://elsewhere.test/p.json"))
	if err == nil || !strings.Contains(err.Error(), "fetcher is not configured") {
		t.Fatalf("got %v", err)
	}
}

func TestCreatePlaylistGroup_localPlaylistNotFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "missing").Return(nil, store.ErrNotFound)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(localPlaylistRef("missing")))
	if err == nil || !strings.Contains(err.Error(), "local playlist") {
		t.Fatalf("got %v", err)
	}
}

func TestCreatePlaylistGroup_preservesLocalDisplayAtWithCoreValidation(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	plID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "daily").Return(&store.PlaylistRecord{
		ID:   plID,
		Slug: "daily",
		Body: playlist.Playlist{
			ID:    plID.String(),
			Slug:  "daily",
			Title: "Daily",
			Items: []playlist.PlaylistItem{
				{Source: "https://cdn.example.com/day1.html", DisplayAt: stringPtr("2026-07-21T00:00:00")},
			},
		},
	}, nil)
	signed := []byte(`{"signed":true}`)
	parsedGroup := mustDecodeGroup(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylistGroup(signed).Return(&parsedGroup, nil),
	)
	mockStore.EXPECT().CreatePlaylistGroup(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.PlaylistGroupInput) {
		if len(in.Playlists) != 1 || displayAtValue(in.Playlists[0].Body.Items[0]) != "2026-07-21T00:00:00" {
			t.Fatalf("expected local displayAt to be preserved, got %+v", in.Playlists)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	if _, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(localPlaylistRef("daily"))); err != nil {
		t.Fatal(err)
	}
}

func TestCreatePlaylistGroup_repeatedURIPreservesOrder(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	plID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	body := []byte(`{"id":"33333333-3333-3333-3333-333333333333"}`)
	plDoc := mustDecodePlaylist(t, body)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "same").Return(&store.PlaylistRecord{
		ID: plID, Slug: "same", Body: plDoc,
	}, nil).Times(2)

	signed := []byte(`{"signed":true}`)
	parsedGroup := mustDecodeGroup(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylistGroup(signed).Return(&parsedGroup, nil),
	)
	mockStore.EXPECT().CreatePlaylistGroup(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.PlaylistGroupInput) {
		if len(in.Playlists) != 2 {
			t.Fatalf("len=%d want 2", len(in.Playlists))
		}
		if in.Playlists[0].ID != plID || in.Playlists[1].ID != plID {
			t.Fatalf("ids: %+v", in.Playlists)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(
		localPlaylistRef("same"),
		localPlaylistRef("same"),
	))
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreatePlaylistGroup_signError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	plID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "a").Return(&store.PlaylistRecord{
		ID: plID, Slug: "a", Body: mustDecodePlaylist(t, []byte(`{"id":"44444444-4444-4444-4444-444444444444"}`)),
	}, nil)
	mockDP1.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(nil, errors.New("no key"))

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(localPlaylistRef("a")))
	if err == nil || !strings.Contains(err.Error(), "sign: no key") {
		t.Fatalf("got %v", err)
	}
}

func TestCreatePlaylistGroup_postSignValidationError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	plID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "a").Return(&store.PlaylistRecord{
		ID: plID, Slug: "a", Body: mustDecodePlaylist(t, []byte(`{"id":"55555555-5555-5555-5555-555555555555"}`)),
	}, nil)
	signed := []byte(`{}`)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylistGroup(signed).Return(nil, errors.New("bad group")),
	)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(localPlaylistRef("a")))
	if err == nil || !strings.Contains(err.Error(), "post-sign validation: bad group") {
		t.Fatalf("got %v", err)
	}
}

func TestCreatePlaylistGroup_storeError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	plID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "a").Return(&store.PlaylistRecord{
		ID: plID, Slug: "a", Body: mustDecodePlaylist(t, []byte(`{"id":"66666666-6666-6666-6666-666666666666"}`)),
	}, nil)
	signed := []byte(`{"signed":true}`)
	parsedGroup := mustDecodeGroup(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylistGroup(signed).Return(&parsedGroup, nil),
	)
	mockStore.EXPECT().CreatePlaylistGroup(gomock.Any(), gomock.Any()).Return(errors.New("tx failed"))

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(localPlaylistRef("a")))
	if err == nil || !strings.Contains(err.Error(), "store: tx failed") {
		t.Fatalf("got %v", err)
	}
}

func TestGetPlaylistGroup(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	g := mustDecodeGroup(t, []byte(`{"title":"g"}`))
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "g1").Return(&store.PlaylistGroupRecord{Body: g}, nil)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	out, err := e.GetPlaylistGroup(context.Background(), "g1")
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, g) {
		t.Fatal("body mismatch")
	}
}

func TestGetPlaylistGroup_notFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "nope").Return(nil, fmt.Errorf("%w", store.ErrNotFound))

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	_, err := e.GetPlaylistGroup(context.Background(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestListPlaylistGroups(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	recs := []store.PlaylistGroupRecord{{Body: playlistgroup.Group{Title: "A"}}}
	mockStore.EXPECT().ListPlaylistGroups(gomock.Any(), &store.ListPlaylistsParams{
		Limit: 10, Cursor: "", Sort: store.SortAsc,
	}).Return(recs, "n", nil)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	items, next, err := e.ListPlaylistGroups(context.Background(), 10, "", store.SortAsc)
	if err != nil {
		t.Fatal(err)
	}
	if next != "n" || len(items) != 1 {
		t.Fatalf("next=%q items=%v", next, items)
	}
	if len(recs) != 1 {
		t.Fatalf("recs len=%d", len(recs))
	}
	if !reflect.DeepEqual(items[0], recs[0].Body) {
		t.Fatalf("body mismatch: %+v", items[0])
	}
}

func TestDeletePlaylistGroup(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	body := playlistgroup.Group{ID: id.String(), Slug: "gid", Curator: testCuratorKid}
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "gid").Return(&store.PlaylistGroupRecord{ID: id, Slug: "gid", Body: body}, nil)
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil)
	mockStore.EXPECT().DeletePlaylistGroup(gomock.Any(), id.String()).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.DeleteTargetPlaylistGroup, id.String(), "gid", testCuratorKid)
	if err := e.DeletePlaylistGroup(context.Background(), "gid", req); err != nil {
		t.Fatal(err)
	}
}

func TestReplacePlaylistGroup_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	gid := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	created := time.Date(2019, 6, 1, 12, 0, 0, 0, time.UTC)
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "keep-g").Return(&store.PlaylistGroupRecord{
		ID:   gid,
		Slug: "keep-g",
		Body: playlistgroup.Group{
			Created: created.UTC().Format(time.RFC3339Nano),
			Curator: testCuratorKid,
		},
		CreatedAt: created,
	}, nil)

	plID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	plBody := []byte(`{"id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}`)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{
		ID: plID, Slug: "pl", Body: mustDecodePlaylist(t, plBody),
	}, nil)

	signed := []byte(`{"replacedGroup":true}`)
	parsedGroup := mustDecodeGroup(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylistGroup(signed).Return(&parsedGroup, nil),
	)
	mockStore.EXPECT().UpdatePlaylistGroup(gomock.Any(), gid.String(), gomock.Any()).Do(func(_ context.Context, _ string, in *store.PlaylistGroupInput) {
		if in.ID != uuid.Nil || in.Slug != "" {
			t.Fatalf("update input should not set row id/slug (store resolves from idOrSlug): id=%v slug=%q", in.ID, in.Slug)
		}
		if len(in.Playlists) != 1 || in.Playlists[0].ID != plID {
			t.Fatalf("playlists: %+v", in.Playlists)
		}
		if !reflect.DeepEqual(in.Body, parsedGroup) {
			t.Fatalf("body: %+v", in.Body)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	req := validGroupCreateReq(localPlaylistRef("pl"))
	req.Title = "New group title"
	out, err := e.ReplacePlaylistGroup(context.Background(), "keep-g", req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, parsedGroup) {
		t.Fatal("out mismatch")
	}
}

func TestReplacePlaylistGroup_withSignatures_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	gid := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	created := time.Date(2019, 6, 1, 12, 0, 0, 0, time.UTC)
	curatorKid := "did:key:groupCuratorTest"
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "keep-g").Return(&store.PlaylistGroupRecord{
		ID:   gid,
		Slug: "keep-g",
		Body: playlistgroup.Group{
			Created: created.UTC().Format(time.RFC3339Nano),
			Curator: curatorKid,
		},
		CreatedAt: created,
	}, nil)

	plID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	plBody := []byte(`{"id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}`)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{
		ID: plID, Slug: "pl", Body: mustDecodePlaylist(t, plBody),
	}, nil)

	signed := []byte(`{"replacedGroupSig":true}`)
	parsedGroup := mustDecodeGroup(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil),
		mockDP1.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylistGroup(signed).Return(&parsedGroup, nil),
	)
	mockStore.EXPECT().UpdatePlaylistGroup(gomock.Any(), gid.String(), gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	req := validGroupCreateReq(localPlaylistRef("pl"))
	req.Title = "New group title"
	req.Curator = curatorKid
	req.Signatures = []playlist.Signature{{Kid: curatorKid, Alg: "ed25519", Sig: "sig"}}

	out, err := e.ReplacePlaylistGroup(context.Background(), "keep-g", req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, parsedGroup) {
		t.Fatal("out mismatch")
	}
}

func TestReplacePlaylistGroup_notFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "x").Return(nil, store.ErrNotFound)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, testPublicBase)
	_, err := e.ReplacePlaylistGroup(context.Background(), "x", validGroupCreateReq(localPlaylistRef("y")))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

// =============================================================================
// Channel Tests
// =============================================================================

// --- group replace deny/error branches ---

// storedOwnedGroup returns a stored group whose owner (curator) is testCuratorKid.
func storedOwnedGroup(id uuid.UUID, slug string) *store.PlaylistGroupRecord {
	return &store.PlaylistGroupRecord{
		ID:   id,
		Slug: slug,
		Body: playlistgroup.Group{ID: id.String(), Slug: slug, Curator: testCuratorKid, Created: "2020-01-02T03:04:05Z"},
	}
}

// memberPlaylistExpect wires a local member playlist so resolvePlaylistURIs succeeds (replace requires a
// non-empty, resolvable playlists list before reaching the auth checks).
func memberPlaylistExpect(t *testing.T, mockStore *mocks.MockStore) string {
	t.Helper()
	plID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{
		ID: plID, Slug: "pl", Body: mustDecodePlaylist(t, []byte(`{"id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"}`)),
	}, nil).AnyTimes()
	return localPlaylistRef("pl")
}

func TestReplacePlaylistGroup_notOwner(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "gid").Return(storedOwnedGroup(id, "gid"), nil)
	ref := memberPlaylistExpect(t, mockStore)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	req := validGroupCreateReq(ref) // curator == stored (immutability passes)
	req.Signatures = []playlist.Signature{testSig("did:key:notGroupOwner")}
	if _, err := e.ReplacePlaylistGroup(context.Background(), "gid", req); !executor.IsForbiddenError(err) {
		t.Fatalf("want forbidden (not owner), got %v", err)
	}
}

func TestReplacePlaylistGroup_verifyCryptoFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "gid").Return(storedOwnedGroup(id, "gid"), nil)
	ref := memberPlaylistExpect(t, mockStore)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(false, []playlist.Signature{{Kid: "x"}}, nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	if _, err := e.ReplacePlaylistGroup(context.Background(), "gid", validGroupCreateReq(ref)); !executor.IsSignatureVerificationError(err) {
		t.Fatalf("want signature-verification error (ok=false), got %v", err)
	}
}

func TestReplacePlaylistGroup_verifyCryptoError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "gid").Return(storedOwnedGroup(id, "gid"), nil)
	ref := memberPlaylistExpect(t, mockStore)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(false, nil, errors.New("boom"))

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	if _, err := e.ReplacePlaylistGroup(context.Background(), "gid", validGroupCreateReq(ref)); !executor.IsSignatureVerificationError(err) {
		t.Fatalf("want signature-verification error (verify err), got %v", err)
	}
}

// --- group delete deny branches (happy path: TestDeletePlaylistGroup) ---

func TestDeletePlaylistGroup_notOwner(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "gid").Return(storedOwnedGroup(id, "gid"), nil)
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.DeleteTargetPlaylistGroup, id.String(), "gid", "did:key:notGroupOwner")
	if err := e.DeletePlaylistGroup(context.Background(), "gid", req); !executor.IsForbiddenError(err) {
		t.Fatalf("want forbidden (not owner), got %v", err)
	}
}

func TestDeletePlaylistGroup_targetMismatch(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "gid").Return(storedOwnedGroup(id, "gid"), nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.DeleteTargetPlaylistGroup, id.String(), "wrong-slug", testCuratorKid)
	if err := e.DeletePlaylistGroup(context.Background(), "gid", req); !executor.IsDeleteRequestError(err) {
		t.Fatalf("want delete-request error (target mismatch), got %v", err)
	}
}

// --- channel replace deny/error branches ---

// storedOwnedChannel returns a stored channel whose owner (publisher) is testPublisherKid, with a
// parseable created (ReplaceChannel parses stored created before the auth checks).
func storedOwnedChannel(id uuid.UUID, slug string) *store.ChannelRecord {
	return &store.ChannelRecord{
		ID:   id,
		Slug: slug,
		Body: channels.Channel{
			ID: id.String(), Slug: slug, Version: "1.0.0",
			Publisher: &identity.Entity{Key: testPublisherKid},
			Created:   "2020-01-02T03:04:05Z",
		},
	}
}

func TestReplaceChannel_notOwner(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	mockStore.EXPECT().GetChannel(gomock.Any(), "cid").Return(storedOwnedChannel(id, "cid"), nil)
	ref := memberPlaylistExpect(t, mockStore)
	mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	req := validChannelCreateReq("cid", ref) // publisher == stored (immutability passes)
	req.Signatures = []playlist.Signature{testSig("did:key:notPublisher")}
	if _, err := e.ReplaceChannel(context.Background(), "cid", req); !executor.IsForbiddenError(err) {
		t.Fatalf("want forbidden (not owner), got %v", err)
	}
}

func TestReplaceChannel_verifyCryptoFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	mockStore.EXPECT().GetChannel(gomock.Any(), "cid").Return(storedOwnedChannel(id, "cid"), nil)
	ref := memberPlaylistExpect(t, mockStore)
	mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(false, []playlist.Signature{{Kid: "x"}}, nil)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	if _, err := e.ReplaceChannel(context.Background(), "cid", validChannelCreateReq("cid", ref)); !executor.IsSignatureVerificationError(err) {
		t.Fatalf("want signature-verification error (ok=false), got %v", err)
	}
}

func TestReplaceChannel_verifyCryptoError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	mockStore.EXPECT().GetChannel(gomock.Any(), "cid").Return(storedOwnedChannel(id, "cid"), nil)
	ref := memberPlaylistExpect(t, mockStore)
	mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(false, nil, errors.New("boom"))

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	if _, err := e.ReplaceChannel(context.Background(), "cid", validChannelCreateReq("cid", ref)); !executor.IsSignatureVerificationError(err) {
		t.Fatalf("want signature-verification error (verify err), got %v", err)
	}
}

// --- channel delete deny branches (happy path: TestDeleteChannel) ---

func TestDeleteChannel_notOwner(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	mockStore.EXPECT().GetChannel(gomock.Any(), "cid").Return(storedOwnedChannel(id, "cid"), nil)
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	req := deleteReq(models.DeleteTargetChannel, id.String(), "cid", "did:key:notPublisher")
	if err := e.DeleteChannel(context.Background(), "cid", req); !executor.IsForbiddenError(err) {
		t.Fatalf("want forbidden (not owner), got %v", err)
	}
}

func TestDeleteChannel_targetMismatch(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	mockStore.EXPECT().GetChannel(gomock.Any(), "cid").Return(storedOwnedChannel(id, "cid"), nil)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	req := deleteReq(models.DeleteTargetChannel, id.String(), "wrong-slug", testPublisherKid)
	if err := e.DeleteChannel(context.Background(), "cid", req); !executor.IsDeleteRequestError(err) {
		t.Fatalf("want delete-request error (target mismatch), got %v", err)
	}
}

// --- channel create error branches (group equivalents already covered) ---

func TestCreateChannel_signError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	ref := memberPlaylistExpect(t, mockStore)
	mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(nil, errors.New("no key"))

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	_, err := e.CreateChannel(context.Background(), validChannelCreateReq("chan", ref))
	if err == nil || !strings.Contains(err.Error(), "feed sign: no key") {
		t.Fatalf("got %v", err)
	}
}

func TestCreateChannel_postSignValidationError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	ref := memberPlaylistExpect(t, mockStore)
	signed := []byte(`{}`)
	gomock.InOrder(
		mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidateChannel(signed).Return(nil, errors.New("bad channel")),
	)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	_, err := e.CreateChannel(context.Background(), validChannelCreateReq("chan", ref))
	if err == nil || !strings.Contains(err.Error(), "post-sign validation: bad channel") {
		t.Fatalf("got %v", err)
	}
}

func TestCreateChannel_extensionsDisabled(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, testPublicBase)
	out, err := e.CreateChannel(context.Background(), validChannelCreateReq("ch", localPlaylistRef("p")))
	if !executor.IsExtensionsDisabled(err) {
		t.Fatalf("got %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil body, got %+v", out)
	}
}

func TestGetChannel_extensionsDisabled(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	out, err := e.GetChannel(context.Background(), "c")
	if !executor.IsExtensionsDisabled(err) {
		t.Fatalf("got %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil body, got %+v", out)
	}
}

func TestListChannels_extensionsDisabled(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	_, _, err := e.ListChannels(context.Background(), 10, "", store.SortDesc)
	if !executor.IsExtensionsDisabled(err) {
		t.Fatalf("got %v", err)
	}
}

func TestReplaceChannel_extensionsDisabled(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	out, err := e.ReplaceChannel(context.Background(), "c", validChannelCreateReq("c", localPlaylistRef("p")))
	if !executor.IsExtensionsDisabled(err) {
		t.Fatalf("got %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil body, got %+v", out)
	}
}

func TestDeleteChannel_extensionsDisabled(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	err := e.DeleteChannel(context.Background(), "c", deleteReq(models.DeleteTargetChannel, "id", "c", testPublisherKid))
	if !executor.IsExtensionsDisabled(err) {
		t.Fatalf("got %v", err)
	}
}

// Slug is required and stored verbatim (no derivation/slugification): the client signs over it.
func TestCreateChannel_missingSlug(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), true, nil, testPublicBase)
	req := validChannelCreateReq("   ", localPlaylistRef("p")) // whitespace-only slug
	if _, err := e.CreateChannel(context.Background(), req); !executor.IsInvalidSubmissionError(err) {
		t.Fatalf("want invalid-submission (slug required), got %v", err)
	}
}

func TestCreateChannel_slugStoredVerbatim(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	plID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "p").Return(&store.PlaylistRecord{
		ID: plID, Slug: "p", Body: mustDecodePlaylist(t, []byte(`{"id":"77777777-7777-7777-7777-777777777777"}`)),
	}, nil)

	signed := []byte(`{"kind":"signed-channel"}`)
	wantCh := mustDecodeChannel(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidateChannel(signed).Return(&wantCh, nil),
	)
	mockStore.EXPECT().CreateChannel(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.ChannelInput) {
		if in.Slug != "My-Channel" {
			t.Fatalf("slug must be stored verbatim, got %q", in.Slug)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	if _, err := e.CreateChannel(context.Background(), validChannelCreateReq("My-Channel", localPlaylistRef("p"))); err != nil {
		t.Fatal(err)
	}
}

func TestCreateChannel_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	plID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	plBody := []byte(`{"id":"88888888-8888-8888-8888-888888888888","slug":"pl-ch"}`)
	plDoc := mustDecodePlaylist(t, plBody)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl-ch").Return(&store.PlaylistRecord{
		ID: plID, Slug: "pl-ch", Body: plDoc,
	}, nil)

	signed := []byte(`{"kind":"signed-channel"}`)
	wantCh := mustDecodeChannel(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidateChannel(signed).Return(&wantCh, nil),
	)
	var createdChannelID uuid.UUID
	mockStore.EXPECT().CreateChannel(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.ChannelInput) {
		createdChannelID = in.ID
		if in.ID == uuid.Nil || in.Slug != "My Channel" {
			t.Fatalf("create expects id and verbatim slug, id=%v slug=%q", in.ID, in.Slug)
		}
		if len(in.Playlists) != 1 || in.Playlists[0].ID != plID {
			t.Fatalf("playlists: %+v", in.Playlists)
		}
		if !reflect.DeepEqual(in.Body, wantCh) {
			t.Fatalf("body %+v", in.Body)
		}
	}).Return(nil)

	notifications := &recordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	out, err := e.CreateChannel(notifiedMutationContext(t), validChannelCreateReq("My Channel", localPlaylistRef("pl-ch")))
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, wantCh) {
		t.Fatal("response mismatch")
	}
	if len(notifications.events) != 1 || notifications.events[0].Type != notification.ChannelAdded {
		t.Fatalf("notification events = %#v", notifications.events)
	}
	if got, want := notifications.events[0].Channel.URL, testPublicBase+"/api/v1/channels/"+createdChannelID.String(); got != want {
		t.Fatalf("notification channel URL = %q", got)
	}
}

func TestCreateChannel_optionalCreate_respectsProvidedID(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	plID := uuid.MustParse("88888888-8888-4888-a888-888888888888")
	plBody := []byte(`{"id":"88888888-8888-4888-a888-888888888888","slug":"pl-ch"}`)
	plDoc := mustDecodePlaylist(t, plBody)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl-ch").Return(&store.PlaylistRecord{
		ID: plID, Slug: "pl-ch", Body: plDoc,
	}, nil)

	wantChID := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	idStr := wantChID.String()
	req := validChannelCreateReq("My Channel", localPlaylistRef("pl-ch"))
	req.ID = &idStr

	signed := []byte(`{"kind":"signed-channel-ops-id"}`)
	wantCh := mustDecodeChannel(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidateChannel(signed).Return(&wantCh, nil),
	)
	mockStore.EXPECT().CreateChannel(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.ChannelInput) {
		if in.ID != wantChID {
			t.Fatalf("want channel id %v, got %v", wantChID, in.ID)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	if _, err := e.CreateChannel(context.Background(), req); err != nil {
		t.Fatal(err)
	}
}

func TestCreateChannel_storeError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	plID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "p").Return(&store.PlaylistRecord{
		ID: plID, Slug: "p", Body: mustDecodePlaylist(t, []byte(`{"id":"99999999-9999-9999-9999-999999999999"}`)),
	}, nil)
	signed := []byte(`{"ok":true}`)
	parsedCh := mustDecodeChannel(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidateChannel(signed).Return(&parsedCh, nil),
	)
	mockStore.EXPECT().CreateChannel(gomock.Any(), gomock.Any()).Return(errors.New("db"))

	notifications := &recordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	_, err := e.CreateChannel(notifiedMutationContext(t), validChannelCreateReq("slug", localPlaylistRef("p")))
	if err == nil || !strings.Contains(err.Error(), "store: db") {
		t.Fatalf("got %v", err)
	}
	if len(notifications.events) != 0 {
		t.Fatalf("notification sent before commit: %#v", notifications.events)
	}
}

func TestGetChannel(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	ch := mustDecodeChannel(t, []byte(`{"title":"ch"}`))
	mockStore.EXPECT().GetChannel(gomock.Any(), "c1").Return(&store.ChannelRecord{Body: ch}, nil)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), true, nil, "")
	out, err := e.GetChannel(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, ch) {
		t.Fatal("body mismatch")
	}
}

func TestGetChannel_notFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetChannel(gomock.Any(), "nope").Return(nil, fmt.Errorf("%w", store.ErrNotFound))

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), true, nil, "")
	_, err := e.GetChannel(context.Background(), "nope")
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestListChannels(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	recs := []store.ChannelRecord{{Body: channels.Channel{Title: "X"}}}
	mockStore.EXPECT().ListChannels(gomock.Any(), &store.ListPlaylistsParams{
		Limit: 5, Cursor: "c", Sort: store.SortDesc,
	}).Return(recs, "next", nil)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), true, nil, "")
	items, next, err := e.ListChannels(context.Background(), 5, "c", store.SortDesc)
	if err != nil {
		t.Fatal(err)
	}
	if next != "next" || len(items) != 1 {
		t.Fatalf("next=%q n=%d", next, len(items))
	}
	if len(recs) != 1 {
		t.Fatalf("recs len=%d", len(recs))
	}
	if !reflect.DeepEqual(items[0], recs[0].Body) {
		t.Fatalf("body mismatch: %+v", items[0])
	}
}

type delayedFetcher struct {
	delay   time.Duration
	started chan struct{}
}

func (f delayedFetcher) FetchPlaylist(ctx context.Context, _ string) ([]byte, error) {
	f.started <- struct{}{}
	select {
	case <-time.After(f.delay):
		return []byte(`{"playlist":true}`), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestCreateChannel_playlistResolutionPreservesPerFetchTimeoutAcrossBatches(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	remotePlaylist := &playlist.Playlist{
		ID:   "77777777-7777-4777-8777-777777777777",
		Slug: "remote",
	}
	mockDP1.EXPECT().ValidatePlaylistWithExtension(gomock.Any()).Return(remotePlaylist, nil).Times(9)
	signed := []byte(`{"kind":"signed-channel"}`)
	wantChannel := mustDecodeChannel(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidateChannel(signed).Return(&wantChannel, nil),
	)
	mockStore.EXPECT().CreateChannel(gomock.Any(), gomock.Any()).Return(nil)

	fetcher := delayedFetcher{delay: 40 * time.Millisecond, started: make(chan struct{}, 9)}
	e := executor.New(
		mockStore,
		mockDP1,
		true,
		fetcher,
		testPublicBase,
	)
	playlists := make([]string, 9) // More than the resolver concurrency limit of eight.
	for i := range playlists {
		playlists[i] = fmt.Sprintf("https://remote.example/playlists/%d", i)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := e.CreateChannel(ctx, &models.ChannelCreateRequest{
		Title:      "Second fetch batch",
		Slug:       "second-fetch-batch",
		Playlists:  playlists,
		ID:         stringPtr("44444444-4444-4444-4444-444444444444"),
		Created:    stringPtr(testCreatedRFC),
		Publisher:  &identity.Entity{Key: testPublisherKid},
		Signatures: []playlist.Signature{testSig(testPublisherKid)},
	})
	if err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	if got := len(fetcher.started); got != len(playlists) {
		t.Fatalf("fetches started = %d, want %d", got, len(playlists))
	}
}

func channelOwnerRecord(cid uuid.UUID, slug string) *store.ChannelRecord {
	return &store.ChannelRecord{
		ID:   cid,
		Slug: slug,
		Body: channels.Channel{ID: cid.String(), Slug: slug, Publisher: &identity.Entity{Key: testPublisherKid}},
	}
}

func TestDeleteChannel(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	cid := uuid.MustParse("eeeeeeee-eeee-4eee-aeee-eeeeeeeeeeee")
	mockStore.EXPECT().GetChannel(gomock.Any(), "cid").Return(channelOwnerRecord(cid, "cid"), nil)
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil)
	mockStore.EXPECT().DeleteChannel(gomock.Any(), cid.String()).Return(nil)

	notifications := &recordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	req := deleteReq(models.DeleteTargetChannel, cid.String(), "cid", testPublisherKid)
	if err := e.DeleteChannel(notifiedMutationContext(t), "cid", req); err != nil {
		t.Fatal(err)
	}
	if len(notifications.events) != 1 || notifications.events[0].Type != notification.ChannelDeleted || notifications.events[0].Channel.URL != testPublicBase+"/api/v1/channels/"+cid.String() {
		t.Fatalf("notification events = %#v", notifications.events)
	}
}

func TestDeleteChannel_notificationSurvivesRequestCancellationAfterCommit(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	cid := uuid.MustParse("eeeeeeee-eeee-4eee-aeee-eeeeeeeeeeee")
	mockStore.EXPECT().GetChannel(gomock.Any(), "cid").Return(channelOwnerRecord(cid, "cid"), nil)
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil)

	deadlineCtx := notifiedMutationContext(t)
	wantDeadline, _ := deadlineCtx.Deadline()
	ctx, cancel := context.WithCancel(deadlineCtx)
	mockStore.EXPECT().DeleteChannel(gomock.Any(), cid.String()).DoAndReturn(func(mutationCtx context.Context, _ string) error {
		cancel() // The row committed just before the HTTP request context was canceled.
		if err := mutationCtx.Err(); err != nil {
			t.Fatalf("mutation context error after request cancellation = %v, want detached context", err)
		}
		return nil
	})

	notifications := &contextRecordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	req := deleteReq(models.DeleteTargetChannel, cid.String(), "cid", testPublisherKid)
	if err := e.DeleteChannel(ctx, "cid", req); err != nil {
		t.Fatal(err)
	}
	if notifications.contextErr != nil {
		t.Fatalf("notification context error = %v, want detached post-commit context", notifications.contextErr)
	}
	if !notifications.hasDeadline || !notifications.deadline.Equal(wantDeadline) {
		t.Fatalf("notification deadline = %v, %t; want %v, true", notifications.deadline, notifications.hasDeadline, wantDeadline)
	}
	if len(notifications.events) != 1 || notifications.events[0].Type != notification.ChannelDeleted {
		t.Fatalf("notification events = %#v", notifications.events)
	}
}

func TestDeleteChannel_doesNotBeginMutationAfterRequestCancellation(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	cid := uuid.MustParse("eeeeeeee-eeee-4eee-aeee-eeeeeeeeeeee")
	ctx, cancel := context.WithCancel(notifiedMutationContext(t))
	mockStore.EXPECT().GetChannel(gomock.Any(), "cid").DoAndReturn(func(context.Context, string) (*store.ChannelRecord, error) {
		cancel()
		return channelOwnerRecord(cid, "cid"), nil
	})
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil)

	notifications := &recordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	req := deleteReq(models.DeleteTargetChannel, cid.String(), "cid", testPublisherKid)
	err := e.DeleteChannel(ctx, "cid", req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DeleteChannel error = %v, want context.Canceled", err)
	}
	if len(notifications.events) != 0 {
		t.Fatalf("notification events = %#v, want none", notifications.events)
	}
}

func TestDeleteChannel_detachedMutationContextIsBounded(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	cid := uuid.MustParse("eeeeeeee-eeee-4eee-aeee-eeeeeeeeeeee")
	mockStore.EXPECT().GetChannel(gomock.Any(), "cid").Return(channelOwnerRecord(cid, "cid"), nil)
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil)
	mockStore.EXPECT().DeleteChannel(gomock.Any(), cid.String()).DoAndReturn(func(mutationCtx context.Context, _ string) error {
		<-mutationCtx.Done()
		return mutationCtx.Err()
	})

	notifications := &recordingNotificationClient{}
	e := executor.New(
		mockStore,
		mockDP1,
		true,
		nil,
		testPublicBase,
		executor.WithNotificationClient(notifications),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	req := deleteReq(models.DeleteTargetChannel, cid.String(), "cid", testPublisherKid)
	err := e.DeleteChannel(ctx, "cid", req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DeleteChannel error = %v, want context.DeadlineExceeded", err)
	}
	if len(notifications.events) != 0 {
		t.Fatalf("notification events = %#v, want none", notifications.events)
	}
}

func TestReplaceChannel_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	cid := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	created := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	mockStore.EXPECT().GetChannel(gomock.Any(), "ch-slug").Return(&store.ChannelRecord{
		ID:   cid,
		Slug: "ch-slug",
		Body: channels.Channel{
			Created:   created.UTC().Format(time.RFC3339Nano),
			Publisher: &identity.Entity{Key: testPublisherKid},
		},
		CreatedAt: created,
	}, nil)

	plID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl2").Return(&store.PlaylistRecord{
		ID: plID, Slug: "pl2", Body: mustDecodePlaylist(t, []byte(`{"id":"dddddddd-dddd-dddd-dddd-dddddddddddd"}`)),
	}, nil)

	signed := []byte(`{"channelUpdated":true}`)
	parsedCh := mustDecodeChannel(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidateChannel(signed).Return(&parsedCh, nil),
	)
	mockStore.EXPECT().UpdateChannel(gomock.Any(), cid.String(), gomock.Any()).Do(func(_ context.Context, _ string, in *store.ChannelInput) {
		if in.ID != uuid.Nil || in.Slug != "" {
			t.Fatalf("update input should not set row id/slug: id=%v slug=%q", in.ID, in.Slug)
		}
		if len(in.Playlists) != 1 || in.Playlists[0].ID != plID {
			t.Fatalf("playlists: %+v", in.Playlists)
		}
		if !reflect.DeepEqual(in.Body, parsedCh) {
			t.Fatalf("body: %+v", in.Body)
		}
	}).Return(nil)

	notifications := &recordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	req := validChannelCreateReq("ignored-on-replace", localPlaylistRef("pl2"))
	req.Title = "New title"
	out, err := e.ReplaceChannel(notifiedMutationContext(t), "ch-slug", req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, parsedCh) {
		t.Fatal("out mismatch")
	}
	if len(notifications.events) != 1 || notifications.events[0].Type != notification.ChannelUpdated || notifications.events[0].Channel.URL != testPublicBase+"/api/v1/channels/"+cid.String() {
		t.Fatalf("notification events = %#v", notifications.events)
	}
}

func TestReplaceChannel_withSignatures_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	cid := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	created := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	pubKid := "did:key:channelPublisherTest"
	mockStore.EXPECT().GetChannel(gomock.Any(), "ch-slug").Return(&store.ChannelRecord{
		ID:   cid,
		Slug: "ch-slug",
		Body: channels.Channel{
			Created:   created.UTC().Format(time.RFC3339Nano),
			Publisher: &identity.Entity{Key: pubKid},
		},
		CreatedAt: created,
	}, nil)

	plID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl2").Return(&store.PlaylistRecord{
		ID: plID, Slug: "pl2", Body: mustDecodePlaylist(t, []byte(`{"id":"dddddddd-dddd-dddd-dddd-dddddddddddd"}`)),
	}, nil)

	signed := []byte(`{"channelSigPath":true}`)
	parsedCh := mustDecodeChannel(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil),
		mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidateChannel(signed).Return(&parsedCh, nil),
	)
	mockStore.EXPECT().UpdateChannel(gomock.Any(), cid.String(), gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	req := validChannelCreateReq("ignored-on-replace", localPlaylistRef("pl2"))
	req.Title = "New title"
	req.Publisher = &identity.Entity{Key: pubKid}
	req.Signatures = []playlist.Signature{{Kid: pubKid, Alg: "ed25519", Sig: "sig"}}

	out, err := e.ReplaceChannel(context.Background(), "ch-slug", req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, parsedCh) {
		t.Fatal("out mismatch")
	}
}

func TestReplaceChannel_notFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetChannel(gomock.Any(), "missing").Return(nil, store.ErrNotFound)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), true, nil, testPublicBase)
	_, err := e.ReplaceChannel(context.Background(), "missing", validChannelCreateReq("x", localPlaylistRef("p")))
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

// --- signatures-required guards, invalid created, create verify-failures, delete store errors ---

func TestCreatePlaylist_missingSignatures(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	req := validCreateReq()
	req.Signatures = nil
	if _, err := e.CreatePlaylist(context.Background(), req); !executor.IsSignaturesRequiredError(err) {
		t.Fatalf("want signatures-required, got %v", err)
	}
}

func TestCreatePlaylist_invalidCreated(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	req := validCreateReq()
	req.Created = stringPtr("nope")
	if _, err := e.CreatePlaylist(context.Background(), req); !executor.IsInvalidTimestampError(err) {
		t.Fatalf("want invalid-timestamp, got %v", err)
	}
}

func TestReplacePlaylist_missingSignatures(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	req := validCreateReq()
	req.Signatures = nil
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", req); !executor.IsSignaturesRequiredError(err) {
		t.Fatalf("want signatures-required, got %v", err)
	}
}

func TestDeletePlaylist_storeError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(storedOwnedPlaylist(id), nil)
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil)
	mockStore.EXPECT().DeletePlaylist(gomock.Any(), id.String()).Return(errors.New("db down"))

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.DeleteTargetPlaylist, id.String(), "id-1", testCuratorKid)
	if err := e.DeletePlaylist(context.Background(), "id-1", req); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("got %v", err)
	}
}

func TestCreatePlaylistGroup_missingSignatures(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	ref := memberPlaylistExpect(t, mockStore)
	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, testPublicBase)
	req := validGroupCreateReq(ref)
	req.Signatures = nil
	if _, err := e.CreatePlaylistGroup(context.Background(), req); !executor.IsSignaturesRequiredError(err) {
		t.Fatalf("want signatures-required, got %v", err)
	}
}

func TestCreatePlaylistGroup_invalidCreated(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	ref := memberPlaylistExpect(t, mockStore)
	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, testPublicBase)
	req := validGroupCreateReq(ref)
	req.Created = stringPtr("nope")
	if _, err := e.CreatePlaylistGroup(context.Background(), req); !executor.IsInvalidTimestampError(err) {
		t.Fatalf("want invalid-timestamp, got %v", err)
	}
}

func TestCreatePlaylistGroup_verifyFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	ref := memberPlaylistExpect(t, mockStore)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(false, []playlist.Signature{{Kid: "x"}}, nil)
	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	if _, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(ref)); !executor.IsSignatureVerificationError(err) {
		t.Fatalf("want signature-verification error, got %v", err)
	}
}

func TestReplacePlaylistGroup_missingSignatures(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, testPublicBase)
	req := validGroupCreateReq(localPlaylistRef("pl"))
	req.Signatures = nil
	if _, err := e.ReplacePlaylistGroup(context.Background(), "gid", req); !executor.IsSignaturesRequiredError(err) {
		t.Fatalf("want signatures-required, got %v", err)
	}
}

func TestDeletePlaylistGroup_storeError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "gid").Return(storedOwnedGroup(id, "gid"), nil)
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil)
	mockStore.EXPECT().DeletePlaylistGroup(gomock.Any(), id.String()).Return(errors.New("db down"))

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.DeleteTargetPlaylistGroup, id.String(), "gid", testCuratorKid)
	if err := e.DeletePlaylistGroup(context.Background(), "gid", req); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("got %v", err)
	}
}

func TestCreateChannel_missingSignatures(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	ref := memberPlaylistExpect(t, mockStore)
	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), true, nil, testPublicBase)
	req := validChannelCreateReq("chan", ref)
	req.Signatures = nil
	if _, err := e.CreateChannel(context.Background(), req); !executor.IsSignaturesRequiredError(err) {
		t.Fatalf("want signatures-required, got %v", err)
	}
}

func TestCreateChannel_invalidCreated(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	ref := memberPlaylistExpect(t, mockStore)
	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), true, nil, testPublicBase)
	req := validChannelCreateReq("chan", ref)
	req.Created = stringPtr("nope")
	if _, err := e.CreateChannel(context.Background(), req); !executor.IsInvalidTimestampError(err) {
		t.Fatalf("want invalid-timestamp, got %v", err)
	}
}

func TestCreateChannel_verifyFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	ref := memberPlaylistExpect(t, mockStore)
	mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(false, []playlist.Signature{{Kid: "x"}}, nil)
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	if _, err := e.CreateChannel(context.Background(), validChannelCreateReq("chan", ref)); !executor.IsSignatureVerificationError(err) {
		t.Fatalf("want signature-verification error, got %v", err)
	}
}

func TestReplaceChannel_missingSignatures(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), true, nil, testPublicBase)
	req := validChannelCreateReq("cid", localPlaylistRef("pl"))
	req.Signatures = nil
	if _, err := e.ReplaceChannel(context.Background(), "cid", req); !executor.IsSignaturesRequiredError(err) {
		t.Fatalf("want signatures-required, got %v", err)
	}
}

// TestDeleteChannel_noPublisher covers publisherKey(nil) and the empty-owner-keys guard: a stored channel
// without a publisher has no owner, so no signature can authorize its deletion.
func TestDeleteChannel_noPublisher(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	rec := &store.ChannelRecord{ID: id, Slug: "cid", Body: channels.Channel{ID: id.String(), Slug: "cid"}}
	mockStore.EXPECT().GetChannel(gomock.Any(), "cid").Return(rec, nil)
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	req := deleteReq(models.DeleteTargetChannel, id.String(), "cid", testPublisherKid)
	if err := e.DeleteChannel(context.Background(), "cid", req); !executor.IsForbiddenError(err) {
		t.Fatalf("want forbidden (no owner), got %v", err)
	}
}

// --- replace store-write error branches (cover the post-verify sign/validate/persist path) ---

func TestReplacePlaylist_storeError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(storedPlaylistRecord(t, id, "keep-me"), nil)
	signed := []byte(`{"replaced":true}`)
	parsed := mustDecodePlaylist(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil),
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), id.String(), &parsed).Return(errors.New("db down"))

	e := executor.New(mockStore, mockDP1, false, nil, "")
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", validCreateReq()); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("got %v", err)
	}
}

func TestReplacePlaylistGroup_storeError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "gid").Return(storedOwnedGroup(id, "gid"), nil)
	ref := memberPlaylistExpect(t, mockStore)
	signed := []byte(`{"g":true}`)
	parsedGroup := mustDecodeGroup(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil),
		mockDP1.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylistGroup(signed).Return(&parsedGroup, nil),
	)
	mockStore.EXPECT().UpdatePlaylistGroup(gomock.Any(), id.String(), gomock.Any()).Return(errors.New("db down"))

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	if _, err := e.ReplacePlaylistGroup(context.Background(), "gid", validGroupCreateReq(ref)); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("got %v", err)
	}
}

func TestReplaceChannel_storeError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	mockStore.EXPECT().GetChannel(gomock.Any(), "cid").Return(storedOwnedChannel(id, "cid"), nil)
	ref := memberPlaylistExpect(t, mockStore)
	signed := []byte(`{"c":true}`)
	parsedChannel := mustDecodeChannel(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil),
		mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidateChannel(signed).Return(&parsedChannel, nil),
	)
	mockStore.EXPECT().UpdateChannel(gomock.Any(), id.String(), gomock.Any()).Return(errors.New("db down"))

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	if _, err := e.ReplaceChannel(context.Background(), "cid", validChannelCreateReq("cid", ref)); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("got %v", err)
	}
}

func TestIsDP1SignError(t *testing.T) {
	t.Parallel()
	if executor.IsDP1SignError(nil) {
		t.Fatal("nil")
	}
	if !executor.IsDP1SignError(sign.ErrSigInvalid) ||
		!executor.IsDP1SignError(sign.ErrUnsupportedAlg) ||
		!executor.IsDP1SignError(sign.ErrNoSignatures) {
		t.Fatal("expected sentinels to match")
	}
	wrapped := fmt.Errorf("layer: %w", sign.ErrSigInvalid)
	if !executor.IsDP1SignError(wrapped) {
		t.Fatal("expected errors.Is through fmt.Errorf wrap")
	}
	if executor.IsDP1SignError(errors.New("plain")) {
		t.Fatal("plain error should not match")
	}
}

func TestIsDP1ValidationError(t *testing.T) {
	t.Parallel()
	if executor.IsDP1ValidationError(nil) {
		t.Fatal("nil")
	}
	if !executor.IsDP1ValidationError(dp1.ErrValidation) {
		t.Fatal("ErrValidation")
	}
	wrappedVal := fmt.Errorf("layer: %w", dp1.ErrValidation)
	if !executor.IsDP1ValidationError(wrappedVal) {
		t.Fatal("wrapped ErrValidation")
	}
	coded := dp1.WithCode(dp1.CodePlaylistInvalid, fmt.Errorf("inner: %w", dp1.ErrValidation))
	if !executor.IsDP1ValidationError(coded) {
		t.Fatal("CodedError wrapping ErrValidation (Unwrap chain)")
	}
	if !executor.IsDP1ValidationError(fmt.Errorf("post: %w", coded)) {
		t.Fatal("wrapped CodedError")
	}
	if executor.IsDP1ValidationError(errors.New("plain")) {
		t.Fatal("plain should not match")
	}
	if executor.IsDP1ValidationError(sign.ErrSigInvalid) {
		t.Fatal("sign error is not validation")
	}
}

// =============================================================================
// Trusted Model Tests
// =============================================================================

func TestCreatePlaylistWithSignatures_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.New().String()
	created := time.Now().Add(-5 * time.Second).Format(time.RFC3339)
	kid := "did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"
	sig := playlist.Signature{
		Kid: kid,
		Alg: "ed25519",
		Sig: "test-sig",
	}

	req := &models.PlaylistCreateRequest{
		DPVersion:  "1.1.0",
		Title:      "Test Playlist",
		Slug:       "test-playlist",
		Items:      []playlist.PlaylistItem{{ID: testItemID, Source: "https://example.com"}},
		Curators:   []identity.Entity{{Key: kid}},
		ID:         &id,
		Created:    &created,
		Signatures: []playlist.Signature{sig},
	}

	// Mock signature verification passes
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil)

	// Mock feed signing
	signed := []byte(`{"dpVersion":"1.1.0","title":"Test Playlist","items":[{"source":"https://example.com"}],"curators":[{"key":"` + kid + `"}]}`)
	mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil)

	// Mock validation
	parsed := mustDecodePlaylist(t, signed)
	mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil)

	// Mock store
	mockStore.EXPECT().CreatePlaylist(gomock.Any(), gomock.Any(), gomock.Any(), &parsed).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	result, err := e.CreatePlaylist(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
}

func TestCreatePlaylistWithSignatures_verificationFailure(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.New().String()
	created := time.Now().Add(-5 * time.Second).Format(time.RFC3339)
	kid := "did:key:test"
	sig := playlist.Signature{
		Kid: kid,
		Alg: "ed25519",
		Sig: "bad-sig",
	}

	req := &models.PlaylistCreateRequest{
		DPVersion:  "1.1.0",
		Title:      "Test",
		Slug:       "test",
		Items:      []playlist.PlaylistItem{{ID: testItemID, Source: "https://example.com"}},
		ID:         &id,
		Created:    &created,
		Signatures: []playlist.Signature{sig},
	}

	// Mock signature verification failure
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(false, []playlist.Signature{sig}, nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	_, err := e.CreatePlaylist(context.Background(), req)
	if err == nil || !errors.Is(err, executor.ErrSignatureVerificationFailed) {
		t.Fatalf("expected ErrSignatureVerificationFailed, got: %v", err)
	}
}

func TestIsSignatureVerificationError(t *testing.T) {
	t.Parallel()

	if !executor.IsSignatureVerificationError(executor.ErrSignatureVerificationFailed) {
		t.Error("should recognize ErrSignatureVerificationFailed")
	}
	if !executor.IsSignatureVerificationError(executor.ErrNoValidCuratorSignature) {
		t.Error("should recognize ErrNoValidCuratorSignature")
	}
	if !executor.IsSignatureVerificationError(executor.ErrNoValidPublisherSignature) {
		t.Error("should recognize ErrNoValidPublisherSignature")
	}
	if executor.IsSignatureVerificationError(errors.New("other")) {
		t.Error("should not recognize other error")
	}
	if executor.IsSignatureVerificationError(nil) {
		t.Error("should not recognize nil")
	}
}

func TestIsInvalidTimestampError(t *testing.T) {
	t.Parallel()

	if !executor.IsInvalidTimestampError(executor.ErrInvalidTimestamp) {
		t.Error("should recognize ErrInvalidTimestamp")
	}
	if executor.IsInvalidTimestampError(errors.New("other")) {
		t.Error("should not recognize other error")
	}
	if executor.IsInvalidTimestampError(nil) {
		t.Error("should not recognize nil")
	}
}

func TestIsInvalidIDError(t *testing.T) {
	t.Parallel()

	if !executor.IsInvalidIDError(executor.ErrInvalidID) {
		t.Error("should recognize ErrInvalidID")
	}
	if executor.IsInvalidIDError(errors.New("other")) {
		t.Error("should not recognize other error")
	}
	if executor.IsInvalidIDError(nil) {
		t.Error("should not recognize nil")
	}
}

// =============================================================================
// Registry Tests
// =============================================================================

func TestGetChannelRegistry_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	pub1ID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	pub2ID := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	expectedPubs := []store.RegistryPublisher{
		{
			ID:       pub1ID,
			Name:     "Publisher One",
			Position: 0,
		},
		{
			ID:       pub2ID,
			Name:     "Publisher Two",
			Position: 1,
		},
	}

	expectedChans := []store.RegistryPublisherChannel{
		{
			ID:          uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
			PublisherID: pub1ID,
			ChannelURL:  "https://example.com/api/v1/channels/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			Position:    0,
		},
		{
			ID:          uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
			PublisherID: pub2ID,
			ChannelURL:  "https://example.com/api/v1/channels/bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
			Position:    0,
		},
	}

	mockStore.EXPECT().GetChannelRegistry(gomock.Any()).Return(expectedPubs, expectedChans, nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	pubs, chans, err := e.GetChannelRegistry(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pubs) != 2 {
		t.Fatalf("expected 2 publishers, got %d", len(pubs))
	}
	if len(chans) != 2 {
		t.Fatalf("expected 2 channels, got %d", len(chans))
	}

	if pubs[0].Name != "Publisher One" {
		t.Errorf("expected 'Publisher One', got %q", pubs[0].Name)
	}
	if pubs[1].Name != "Publisher Two" {
		t.Errorf("expected 'Publisher Two', got %q", pubs[1].Name)
	}
}

func TestGetChannelRegistry_storeError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	storeErr := errors.New("database connection lost")
	mockStore.EXPECT().GetChannelRegistry(gomock.Any()).Return(nil, nil, storeErr)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	_, _, err := e.GetChannelRegistry(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, storeErr) {
		t.Errorf("expected error to wrap store error, got: %v", err)
	}
}
