package executor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
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
	"github.com/display-protocol/dp1-feed-v2/internal/fetcher"
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
		Action:     models.IntentActionDelete,
		Target:     models.IntentTarget{Type: targetType, ID: id, Slug: slug},
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

// testPayloadHash is what the mocked PayloadHash returns, so a replace intent can name the document it
// authorizes without the test computing a real digest.
const testPayloadHash = "sha256:deadbeef"

// replaceIntent builds a valid signed replace-intent for a stored resource. Unit tests mock the crypto,
// so the intent only has to be well-formed, fresh, and signed by kid.
func replaceIntent(targetType, id, slug, kid string) *models.SignedIntent {
	r := &models.SignedIntent{
		Action:      models.IntentActionReplace,
		Target:      models.IntentTarget{Type: targetType, ID: id, Slug: slug},
		PayloadHash: testPayloadHash,
		Created:     time.Now().UTC().Format(time.RFC3339),
		Signatures:  []playlist.Signature{testSig(kid)},
	}
	raw, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	r.Raw = raw
	return r
}

// expectIntentOK arms the mocked crypto a valid replace-intent needs: the document digest it names and
// verification of the intent's own signatures.
func expectIntentOK(m *mocks.MockValidatorSigner) {
	m.EXPECT().PayloadHash(gomock.Any()).Return(testPayloadHash, nil).AnyTimes()
	m.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
}

// testItemID is a fixed UUID so signed submissions carry a deterministic item id (the feed no longer
// mints one after signing).
const testItemID = "aaaaaaaa-0000-0000-0000-0000000000a1"

// mustJSONRaw marshals a request into the document bytes the client would have signed. Unit tests mock
// signing/validation, so the exact shape only has to be non-empty and identity-consistent.
func mustJSONRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func validCreateReq() *models.PlaylistCreateRequest {
	req := &models.PlaylistCreateRequest{
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
	req.Raw = mustJSONRaw(req)
	return req
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
	mockStore.EXPECT().CreatePlaylist(gomock.Any(), gomock.AssignableToTypeOf(uuid.UUID{}), gomock.Any(), gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	out, err := e.CreatePlaylist(context.Background(), validCreateReq())
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(out.Body, parsed) {
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
	mockStore.EXPECT().CreatePlaylist(gomock.Any(), gomock.AssignableToTypeOf(uuid.UUID{}), gomock.Any(), gomock.Any()).Return(nil)

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
		DoAndReturn(func(_ context.Context, _ uuid.UUID, _ string, _ json.RawMessage) error {
			// The store receives the feed-signed bytes (mocked here), so assert on preSign — the exact
			// document handed to the signer, which is what the client submitted.
			body := mustDecodePlaylist(t, preSign)
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	mockStore.EXPECT().CreatePlaylist(gomock.Any(), gomock.AssignableToTypeOf(uuid.UUID{}), gomock.Any(), gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	out, err := e.CreatePlaylist(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || displayAtValue(out.Body.Items[0]) != "2026-07-21T00:00:00" {
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
	mockStore.EXPECT().CreatePlaylist(gomock.Any(), gomock.AssignableToTypeOf(uuid.UUID{}), gomock.Any(), gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, true, nil, "")
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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

	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), gomock.Any()).Return(nil, store.ErrNotFound).AnyTimes()
	e := executor.New(mockStore, mockDP1, false, nil, "")
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

	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), gomock.Any()).Return(nil, store.ErrNotFound).AnyTimes()
	e := executor.New(mockStore, mockDP1, false, nil, "")
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
	mockStore.EXPECT().CreatePlaylist(gomock.Any(), wantID, gomock.Any(), gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	if out == nil || !reflect.DeepEqual(out.Body, pl) {
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
	if !reflect.DeepEqual(items[0].Body, recs[0].Body) || !reflect.DeepEqual(items[1].Body, recs[1].Body) {
		t.Fatalf("items mismatch: %+v %+v", items[0].Body, items[1].Body)
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
			if tt.wantItems > 0 && items[0].Body.Title != tt.wantFirstTitle {
				t.Fatalf("items[0].Body.Title=%q want %q", items[0].Body.Title, tt.wantFirstTitle)
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", req, nil); !executor.IsInvalidSubmissionError(err) {
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
	mockStore.EXPECT().DeletePlaylist(gomock.Any(), id.String(), gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.IntentTargetPlaylist, id.String(), "id-1", testCuratorKid)
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
	req := deleteReq(models.IntentTargetPlaylist, id.String(), "id-1", "did:key:someoneElse")
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
	req := deleteReq(models.IntentTargetPlaylist, id.String(), "wrong-slug", testCuratorKid)
	err := e.DeletePlaylist(context.Background(), "id-1", req)
	if !executor.IsIntentError(err) {
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

	e := executor.New(mockStore, mockDP1, false, nil, "", executor.WithIntentClockSkew(time.Minute))
	req := deleteReq(models.IntentTargetPlaylist, id.String(), "id-1", testCuratorKid)
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
	req := deleteReq(models.IntentTargetPlaylist, id.String(), "id-1", testCuratorKid)
	req.Action = "nuke"
	if err := e.DeletePlaylist(context.Background(), "id-1", req); !executor.IsIntentError(err) {
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
	req := deleteReq(models.IntentTargetChannel, id.String(), "id-1", testCuratorKid)
	if err := e.DeletePlaylist(context.Background(), "id-1", req); !executor.IsIntentError(err) {
		t.Fatalf("want delete-request error (wrong target type), got %v", err)
	}
}

// A delete intent carries no document, and the route's delete schema forbids payloadHash outright
// (additionalProperties:false, property not listed). Rejecting only a non-empty value would accept three
// spellings the contract forbids, because null, "" and "   " all decode to the empty string — so the
// check is on the member's presence in the received bytes, which is the only place that distinction
// survives decoding.
func TestDeletePlaylist_payloadHashRejected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "null", value: `null`},
		{name: "empty string", value: `""`},
		{name: "whitespace", value: `"   "`},
		{name: "real digest", value: `"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			mockStore := mocks.NewMockStore(ctrl)
			mockDP1 := mocks.NewMockValidatorSigner(ctrl)
			id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
			mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(storedOwnedPlaylist(id), nil)

			req := deleteReq(models.IntentTargetPlaylist, id.String(), "id-1", testCuratorKid)
			// Splice the member into the received bytes: the struct field cannot express "present but null",
			// and Raw is what the presence check and the signatures both read.
			var members map[string]json.RawMessage
			if err := json.Unmarshal(req.Raw, &members); err != nil {
				t.Fatalf("unmarshal intent: %v", err)
			}
			members["payloadHash"] = json.RawMessage(tc.value)
			raw, err := json.Marshal(members)
			if err != nil {
				t.Fatalf("marshal intent: %v", err)
			}
			// Re-decode so the struct field and Raw agree, exactly as bindDeleteRequest leaves them. Without
			// this the case would be unfaithful: null/""/"   " decode to the empty string (which is the whole
			// point), but a real digest decodes to that digest, and a test that skipped the decode would
			// credit the presence check for a rejection the old value check already made.
			var decoded models.SignedDeleteRequest
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatalf("re-decode intent: %v", err)
			}
			decoded.Raw = raw
			req = &decoded

			e := executor.New(mockStore, mockDP1, false, nil, "")
			if err := e.DeletePlaylist(context.Background(), "id-1", req); !executor.IsIntentError(err) {
				t.Fatalf("want intent error for payloadHash %s on a delete, got %v", tc.name, err)
			}
		})
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
	req := deleteReq(models.IntentTargetPlaylist, id.String(), "id-1", testCuratorKid)
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
	req := deleteReq(models.IntentTargetPlaylist, id.String(), "id-1", testCuratorKid)
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

	e := executor.New(mockStore, mockDP1, false, nil, "", executor.WithIntentClockSkew(time.Minute))
	req := deleteReq(models.IntentTargetPlaylist, id.String(), "id-1", testCuratorKid)
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
	req := deleteReq(models.IntentTargetPlaylist, id.String(), "id-1", testCuratorKid)
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
	req := deleteReq(models.IntentTargetPlaylist, id.String(), "id-1", testCuratorKid)
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
		Action:     models.IntentActionDelete,
		Target:     models.IntentTarget{Type: models.IntentTargetPlaylist, ID: id.String(), Slug: "id-1"},
		Created:    time.Now().UTC().Format(time.RFC3339),
		Signatures: []playlist.Signature{testSig(testCuratorKid)},
	}
	if err := e.DeletePlaylist(context.Background(), "id-1", req); !executor.IsIntentError(err) {
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
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(storedPlaylistRecord(t, id, "test-playlist"), nil)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(false, []playlist.Signature{{Kid: "x"}}, nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", validCreateReq(), nil); !executor.IsSignatureVerificationError(err) {
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
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(storedPlaylistRecord(t, id, "test-playlist"), nil)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(false, nil, errors.New("boom"))

	e := executor.New(mockStore, mockDP1, false, nil, "")
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", validCreateReq(), nil); !executor.IsSignatureVerificationError(err) {
		t.Fatalf("want signature-verification error (verify err), got %v", err)
	}
}

func TestReplacePlaylist_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	expectIntentOK(mockDP1)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	existing := []byte(`{"dpVersion":"1.1.0","id":"11111111-1111-1111-1111-111111111111","slug":"test-playlist","title":"Old","created":"2026-01-01T00:00:00Z","curators":[{"key":"did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"}],"items":[{"source":"https://old"}]}`)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(&store.PlaylistRecord{
		ID:   id,
		Slug: "test-playlist",
		Raw:  existing,
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
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), id.String(), gomock.Any(), gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, true, nil, "")
	req := validCreateReq()
	req.Title = "New title"
	req.Items = []playlist.PlaylistItem{
		{ID: testItemID, Source: "https://cdn.example.com/day1.html", DisplayAt: stringPtr("2026-07-21T00:00:00")},
	}
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	out, err := e.ReplacePlaylist(context.Background(), "keep-me", req, replaceIntent(models.IntentTargetPlaylist, id.String(), "test-playlist", testCuratorKid))
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(out.Body, parsed) {
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
	expectIntentOK(mockDP1)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	existing := []byte(`{"dpVersion":"1.1.0","id":"11111111-1111-1111-1111-111111111111","slug":"test-playlist","title":"Old","created":"2026-01-01T00:00:00Z","curators":[{"key":"did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"}],"items":[{"source":"https://old"}]}`)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(&store.PlaylistRecord{
		ID:   id,
		Slug: "test-playlist",
		Raw:  existing,
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
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), id.String(), gomock.Any(), gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := validCreateReq()
	req.Title = "New title"
	req.Signatures = []playlist.Signature{sig}
	req.Curators = []identity.Entity{{Key: kid}}

	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	out, err := e.ReplacePlaylist(context.Background(), "keep-me", req, replaceIntent(models.IntentTargetPlaylist, id.String(), "test-playlist", testCuratorKid))
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(out.Body, parsed) {
		t.Fatalf("out mismatch")
	}
}

// storedPlaylistRecord is the stored playlist the replace-deny tests target: owner is testCuratorKid.
func storedPlaylistRecord(t *testing.T, id uuid.UUID, slug string) *store.PlaylistRecord {
	t.Helper()
	// Identity is immutable on replace, so the stored record must carry exactly what validCreateReq sends.
	existing := []byte(`{"dpVersion":"1.1.0","id":"` + id.String() + `","slug":"` + slug + `","title":"Old","created":"` + testCreatedRFC + `","curators":[{"key":"` + testCuratorKid + `"}],"items":[{"id":"` + testItemID + `","source":"https://old"}]}`)
	return &store.PlaylistRecord{ID: id, Slug: slug, Raw: existing, Body: mustDecodePlaylist(t, existing)}
}

// TestReplacePlaylist_ownerImmutable: changing the curator (owner) set on a PUT is refused with 403,
// before any signature verification.
func TestReplacePlaylist_ownerImmutable(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(storedPlaylistRecord(t, id, "test-playlist"), nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := validCreateReq()
	req.Curators = []identity.Entity{{Key: "did:key:z6MkNewOwnerXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"}}
	req.Signatures = []playlist.Signature{testSig("did:key:z6MkNewOwnerXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX")}

	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	_, err := e.ReplacePlaylist(context.Background(), "keep-me", req, nil)
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
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(storedPlaylistRecord(t, id, "test-playlist"), nil)
	// Signatures verify cryptographically, but none is a stored owner key.
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := validCreateReq() // curators == stored (immutability passes)
	req.Signatures = []playlist.Signature{testSig("did:key:z6MkNotAnOwnerXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX")}

	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	_, err := e.ReplacePlaylist(context.Background(), "keep-me", req, nil)
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
	existing := []byte(`{"id":"` + id.String() + `","slug":"group-title","title":"Old","created":"2026-01-01T00:00:00Z","curator":"` + testCuratorKid + `","playlists":[]}`)
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "group-title").Return(&store.PlaylistGroupRecord{ID: id, Slug: "group-title", Raw: existing, Body: mustDecodeGroup(t, existing)}, nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := validGroupCreateReq()
	req.Curator = "did:key:z6MkNewGroupOwnerXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
	req.Signatures = []playlist.Signature{testSig(req.Curator)}

	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	_, err := e.ReplacePlaylistGroup(context.Background(), "group-title", req, nil)
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
	existing := []byte(`{"id":"` + id.String() + `","slug":"chan","title":"Old","version":"1.0.0","created":"2026-01-01T00:00:00Z","publisher":{"key":"` + testPublisherKid + `"},"playlists":[],"signatures":[{"alg":"ed25519","kid":"` + testPublisherKid + `","ts":"2020-01-02T03:04:05Z","payload_hash":"h","role":"publisher","sig":"s"}]}`)
	mockStore.EXPECT().GetChannel(gomock.Any(), "chan").Return(&store.ChannelRecord{ID: id, Slug: "chan", Raw: existing, Body: mustDecodeChannel(t, existing)}, nil)

	e := executor.New(mockStore, mockDP1, true, nil, "")
	req := validChannelCreateReq("chan")
	req.Publisher = &identity.Entity{Key: "did:key:z6MkNewPublisherXXXXXXXXXXXXXXXXXXXXXXXXXXXX"}
	req.Signatures = []playlist.Signature{testSig(req.Publisher.Key)}

	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	_, err := e.ReplaceChannel(context.Background(), "chan", req, nil)
	if !executor.IsForbiddenError(err) {
		t.Fatalf("want forbidden (channel owner immutable), got %v", err)
	}
}

func TestReplacePlaylist_preservesItemDisplayAtWithCoreValidation(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	expectIntentOK(mockDP1)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	existing := []byte(`{"dpVersion":"1.1.0","id":"11111111-1111-1111-1111-111111111111","slug":"test-playlist","title":"Old","created":"2026-01-01T00:00:00Z","curators":[{"key":"did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"}],"items":[{"source":"https://old"}]}`)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "daily").Return(&store.PlaylistRecord{
		ID:   id,
		Slug: "test-playlist",
		Raw:  existing,
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
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), id.String(), gomock.Any(), gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	out, err := e.ReplacePlaylist(context.Background(), "daily", req, replaceIntent(models.IntentTargetPlaylist, id.String(), "test-playlist", testCuratorKid))
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || displayAtValue(out.Body.Items[0]) != "2026-07-21T00:00:00" {
		t.Fatalf("expected displayAt to be preserved, got %+v", out)
	}
}

func TestReplacePlaylist_notFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "x").Return(nil, store.ErrNotFound)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	_, err := e.ReplacePlaylist(context.Background(), "x", validCreateReq(), nil)
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
			id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
			mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{
				ID:   id,
				Slug: "test-playlist",
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
			_, err := e.ReplacePlaylist(context.Background(), "pl", validCreateReq(), nil)
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
	req := &models.PlaylistGroupCreateRequest{
		Title:      "Group title",
		Slug:       "group-title",
		Playlists:  uris,
		ID:         stringPtr("33333333-3333-3333-3333-333333333333"),
		Created:    stringPtr(testCreatedRFC),
		Curator:    testCuratorKid,
		Signatures: []playlist.Signature{testSig(testCuratorKid)},
	}
	req.Raw = mustJSONRaw(req)
	return req
}

func validChannelCreateReq(slug string, uris ...string) *models.ChannelCreateRequest {
	req := &models.ChannelCreateRequest{
		Title:      "Channel title",
		Slug:       slug,
		Playlists:  uris,
		ID:         stringPtr("44444444-4444-4444-4444-444444444444"),
		Created:    stringPtr(testCreatedRFC),
		Publisher:  &identity.Entity{Name: "Publisher", Key: testPublisherKid},
		Signatures: []playlist.Signature{testSig(testPublisherKid)},
	}
	req.Raw = mustJSONRaw(req)
	return req
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
		Raw:  plBody,
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
		if len(in.Playlists) != 1 || in.Playlists[0].ID != plID || !bytes.Equal(in.Playlists[0].Raw, plBody) {
			t.Fatalf("ingested playlists: %+v", in.Playlists)
		}
		// The group row is stored as the exact signed bytes, not a re-marshaled struct.
		if !bytes.Equal(in.Raw, signed) {
			t.Fatalf("raw: %s want %s", in.Raw, signed)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	out, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(localPlaylistRef("pl-one")))
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(out.Body, wantGroup) {
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	expectGroupSignedAndValid(t, mockDP1)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), gomock.Any()).Return(nil, store.ErrNotFound).AnyTimes()
	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
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
	expectGroupSignedAndValid(t, mockDP1)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), gomock.Any()).Return(nil, store.ErrNotFound).AnyTimes()
	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq("https://elsewhere.test/p.json"))
	if err == nil || !strings.Contains(err.Error(), "fetcher is not configured") {
		t.Fatalf("got %v", err)
	}
}

// Reference count is a fan-out bound: creation is open, and every unstored URI becomes an outbound
// fetch, so the cap has to be enforced before resolution starts rather than discovered during it.
//
// Neither mock declares GetPlaylist or a fetcher. That is the assertion: an over-cap document must be
// refused without a single lookup or fetch, so if the check ever moved after resolution, gomock would
// fail on the unexpected call (and a nil fetcher would panic) rather than quietly passing.
func TestCreatePlaylistGroup_tooManyReferences(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), gomock.Any()).Return(nil, store.ErrNotFound).AnyTimes()
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	expectGroupSignedAndValid(t, mockDP1)

	refs := make([]string, 0, 4)
	for i := range 4 {
		refs = append(refs, localPlaylistRef(fmt.Sprintf("pl-%d", i)))
	}
	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase, executor.WithMaxPlaylistReferences(3))
	_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(refs...))
	if !errors.Is(err, executor.ErrTooManyReferences) {
		t.Fatalf("want ErrTooManyReferences for %d refs over a cap of 3, got %v", len(refs), err)
	}
	// The client chose the reference list, so this is their input being wrong, not an internal fault.
	if !executor.IsInvalidSubmissionError(err) {
		t.Fatalf("want a 400-class submission error, got %v", err)
	}
}

// The cap is a maximum, not a strict bound: a document sitting exactly on it must still be accepted, or
// the limit a deployment configures would silently be one less than it says.
func TestCreatePlaylistGroup_referencesAtCapAllowed(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	expectGroupSignedAndValid(t, mockDP1)

	plID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	plBody := []byte(`{"id":"22222222-2222-2222-2222-222222222222","slug":"pl-one","title":"P"}`)
	plDoc := mustDecodePlaylist(t, plBody)
	// One lookup covers both positions: the cap counts references in the document, while resolution is
	// per distinct URI.
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl-one").Return(&store.PlaylistRecord{
		ID: plID, Slug: "pl-one", Raw: plBody, Body: plDoc,
	}, nil).Times(1)
	mockStore.EXPECT().CreatePlaylistGroup(gomock.Any(), gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase, executor.WithMaxPlaylistReferences(2))
	refs := []string{localPlaylistRef("pl-one"), localPlaylistRef("pl-one")}
	if _, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(refs...)); err != nil {
		t.Fatalf("a group with exactly the maximum reference count must be accepted, got %v", err)
	}
}

// failingFetcher stands in for an origin that is down: every fetch errors.
type failingFetcher struct{ err error }

func (f failingFetcher) FetchPlaylist(_ context.Context, _ string) ([]byte, error) { return nil, f.err }

// A reference this feed has already ingested must resolve while its origin is unreachable.
//
// Ingestion is reference-only: a stored member is never refreshed, so fetching a known reference could
// only ever rediscover an id already recorded here. Doing that made the write depend on the origin being
// up, which meant an upstream outage blocked re-creating or replacing a group whose content could not
// change. The mapping recorded at ingest time answers instead, so the fetcher is never called — asserted
// here by giving it an error that would fail the test if it were consulted.
func TestCreatePlaylistGroup_knownRemoteReferenceResolvesDuringOriginOutage(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	expectGroupSignedAndValid(t, mockDP1)

	const remoteURI = "https://elsewhere.test/p.json"
	plID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	plBody := []byte(`{"id":"22222222-2222-2222-2222-222222222222","slug":"pl-one","title":"P"}`)
	plDoc := mustDecodePlaylist(t, plBody)
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), remoteURI).Return(&store.PlaylistRecord{
		ID: plID, Slug: "pl-one", Raw: plBody, Body: plDoc,
	}, nil)
	mockStore.EXPECT().CreatePlaylistGroup(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.PlaylistGroupInput) {
		if len(in.Playlists) != 1 || in.Playlists[0].ID != plID {
			t.Fatalf("expected the stored playlist to be linked, got %+v", in.Playlists)
		}
		// The mapping must be carried through so it survives for the next ingest of this URI.
		if in.Playlists[0].SourceURI != remoteURI {
			t.Fatalf("resolved reference should carry its source URI, got %q", in.Playlists[0].SourceURI)
		}
	}).Return(nil)

	// Any fetch attempt is a failure of the property under test, not a fallback.
	fetch := failingFetcher{err: errors.New("origin is unreachable")}
	e := executor.New(mockStore, mockDP1, false, fetch, testPublicBase)
	if _, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(remoteURI)); err != nil {
		t.Fatalf("a known remote reference must resolve without contacting its origin, got %v", err)
	}
}

// The reference cap bounds how MANY playlists a mutation resolves; it says nothing about how BIG they
// are, and the two multiply. Every resolved body is retained until the set is ready to persist, so the
// default 1000 references at the 4 MiB fetch cap is ~4 GiB from one unauthenticated request. This pins
// the aggregate budget that actually bounds it: a reference count well inside the cap must still be
// refused once the documents behind it exceed the budget.
func TestCreatePlaylistGroup_resolvedBytesBudget(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	expectGroupSignedAndValid(t, mockDP1)

	// Four stored playlists of 400 bytes each against a 1000-byte budget: only three references, far
	// under any reference cap, yet together over the memory bound.
	big := make([]byte, 400)
	for i := range big {
		big[i] = 'x'
	}
	body := append(append([]byte(`{"id":"22222222-2222-2222-2222-222222222222","slug":"pl","pad":"`), big...), []byte(`"}`)...)
	plID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	plDoc := mustDecodePlaylist(t, body)
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), gomock.Any()).Return(&store.PlaylistRecord{
		ID: plID, Slug: "pl", Raw: body, Body: plDoc,
	}, nil).AnyTimes()

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase,
		executor.WithMaxResolvedBytes(1000))
	refs := []string{"https://a.test/1.json", "https://a.test/2.json", "https://a.test/3.json"}
	_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(refs...))
	if !errors.Is(err, executor.ErrResolvedTooLarge) {
		t.Fatalf("want ErrResolvedTooLarge once resolved bodies exceed the budget, got %v", err)
	}
	// The client chose the reference list, so this is their input being too large, not a server fault.
	if !executor.IsInvalidSubmissionError(err) {
		t.Fatalf("want a 400-class submission error, got %v", err)
	}
}

// A URI whose origin re-points it must resolve to the NEW playlist, not the one it first resolved to.
//
// The cache is consulted only when a fetch fails, so a healthy origin stays authoritative. An earlier
// design read the cache first and skipped the fetch, which pinned a URI to whatever it first resolved to
// — globally and permanently, set by whichever anonymous caller referenced it first, since creation is
// open. A publisher re-pointing their own URL was then never picked up by anyone. This pins the fix: the
// cache holds an old id, and it must not win.
func TestCreatePlaylistGroup_repointedRemoteURIResolvesToCurrentPlaylist(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	expectGroupSignedAndValid(t, mockDP1)

	const remoteURI = "https://elsewhere.test/p.json"
	newID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	newBody := []byte(`{"id":"44444444-4444-4444-4444-444444444444","slug":"pl-new","title":"N"}`)
	newDoc := mustDecodePlaylist(t, newBody)

	// The origin now serves the new playlist, and this feed already holds it.
	mockStore.EXPECT().GetPlaylist(gomock.Any(), newID.String()).Return(&store.PlaylistRecord{
		ID: newID, Slug: "pl-new", Raw: newBody, Body: newDoc,
	}, nil)
	// A stale cache entry exists and would win under the old ordering. Allowed but not required, so the
	// test fails on the resolved id rather than on call counts.
	oldID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), remoteURI).Return(&store.PlaylistRecord{
		ID: oldID, Slug: "pl-old", Raw: []byte(`{"id":"55555555-5555-5555-5555-555555555555"}`),
	}, nil).AnyTimes()

	mockStore.EXPECT().CreatePlaylistGroup(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.PlaylistGroupInput) {
		if len(in.Playlists) != 1 || in.Playlists[0].ID != newID {
			t.Fatalf("re-pointed URI must resolve to the current playlist %s, got %+v", newID, in.Playlists)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, false, staticFetcher{body: newBody}, testPublicBase)
	if _, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(remoteURI)); err != nil {
		t.Fatalf("group create: %v", err)
	}
}

// A reference URI is client input with no length bound of its own beyond the request cap, so it is
// rejected as a client error rather than carried into storage.
func TestCreatePlaylistGroup_overlongReferenceURIRejected(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	expectGroupSignedAndValid(t, mockDP1)

	long := "https://elsewhere.test/" + strings.Repeat("p", 4096) + ".json"
	e := executor.New(mockStore, mockDP1, false, staticFetcher{body: []byte(`{}`)}, testPublicBase)
	_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(long))
	if !errors.Is(err, executor.ErrPlaylistURITooLong) {
		t.Fatalf("want ErrPlaylistURITooLong, got %v", err)
	}
	if !executor.IsInvalidSubmissionError(err) {
		t.Fatalf("want a 400-class submission error, got %v", err)
	}
}

// statusFetcher answers as a reachable origin returning a given HTTP status.
type statusFetcher struct{ code int }

func (f statusFetcher) FetchPlaylist(_ context.Context, _ string) ([]byte, error) {
	return nil, &fetcher.StatusError{Code: f.code}
}

// The cache is a fallback for an origin that cannot be reached, not a way to ignore what a reachable one
// said. A publisher withdrawing a playlist answers 404 or 410; treating that as unavailability would keep
// the withdrawn reference alive indefinitely, since nothing else revisits it. 5xx and 429 are the origin
// failing or deferring rather than deciding, so those still fall back.
func TestCreatePlaylistGroup_cacheFallbackOnlyWhenOriginUnavailable(t *testing.T) {
	t.Parallel()
	const remoteURI = "https://elsewhere.test/p.json"
	cachedID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	cachedBody := []byte(`{"id":"22222222-2222-2222-2222-222222222222","slug":"pl-one","title":"P"}`)

	for _, tc := range []struct {
		name         string
		status       int
		wantFallback bool
	}{
		{name: "withdrawn 404 is authoritative", status: 404, wantFallback: false},
		{name: "gone 410 is authoritative", status: 410, wantFallback: false},
		{name: "forbidden 403 is authoritative", status: 403, wantFallback: false},
		{name: "server error 503 is unavailability", status: 503, wantFallback: true},
		{name: "rate limited 429 is unavailability", status: 429, wantFallback: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			mockStore := mocks.NewMockStore(ctrl)
			mockDP1 := mocks.NewMockValidatorSigner(ctrl)
			mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
			expectGroupSignedAndValid(t, mockDP1)

			plDoc := mustDecodePlaylist(t, cachedBody)
			mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), remoteURI).Return(&store.PlaylistRecord{
				ID: cachedID, Slug: "pl-one", Raw: cachedBody, Body: plDoc,
			}, nil).AnyTimes()
			if tc.wantFallback {
				mockStore.EXPECT().CreatePlaylistGroup(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.PlaylistGroupInput) {
					if len(in.Playlists) != 1 || in.Playlists[0].ID != cachedID {
						t.Fatalf("an unavailable origin should fall back to the cached playlist, got %+v", in.Playlists)
					}
				}).Return(nil)
			}

			e := executor.New(mockStore, mockDP1, false, statusFetcher{code: tc.status}, testPublicBase)
			_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(remoteURI))
			if tc.wantFallback {
				if err != nil {
					t.Fatalf("status %d should fall back to the cache, got %v", tc.status, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("status %d is the origin answering, so the write must fail rather than reuse the cache", tc.status)
			}
			if !strings.Contains(err.Error(), "unexpected status") {
				t.Fatalf("error should report the origin's status, got %v", err)
			}
		})
	}
}

// countingFetcher records how many times each URI was fetched.
type countingFetcher struct {
	mu   sync.Mutex
	body []byte
	hits map[string]int
}

func (f *countingFetcher) FetchPlaylist(_ context.Context, uri string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hits[uri]++
	return f.body, nil
}

// A document may repeat a reference, and every occurrence must mean the same playlist.
//
// Resolving each occurrence independently let one URI answer differently within a single request: two
// fetches straddling a change at the origin produce two ids, so membership rows disagree about what that
// URI means while only one URI→id mapping is recorded. A later replace of the unchanged document would
// then quietly move membership to the mapped winner. Resolving once per distinct URI removes the
// possibility, and the fetch count is the observable proof.
func TestCreatePlaylistGroup_repeatedReferenceResolvesOnce(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	expectGroupSignedAndValid(t, mockDP1)

	const remoteURI = "https://elsewhere.test/p.json"
	plID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	plBody := []byte(`{"id":"22222222-2222-2222-2222-222222222222","slug":"pl-one","title":"P"}`)
	plDoc := mustDecodePlaylist(t, plBody)

	// No GetPlaylistBySourceURI expectation: the cache is a fallback for a failed fetch, so a healthy
	// origin must not consult it at all.
	mockStore.EXPECT().GetPlaylist(gomock.Any(), plID.String()).Return(&store.PlaylistRecord{
		ID: plID, Slug: "pl-one", Raw: plBody, Body: plDoc,
	}, nil)
	mockStore.EXPECT().CreatePlaylistGroup(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.PlaylistGroupInput) {
		if len(in.Playlists) != 3 {
			t.Fatalf("every position must still be present, got %d", len(in.Playlists))
		}
		for i, p := range in.Playlists {
			if p.ID != plID {
				t.Fatalf("position %d resolved to %s, want the single shared id %s", i, p.ID, plID)
			}
		}
	}).Return(nil)

	fetch := &countingFetcher{body: plBody, hits: map[string]int{}}
	e := executor.New(mockStore, mockDP1, false, fetch, testPublicBase)
	if _, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(remoteURI, remoteURI, remoteURI)); err != nil {
		t.Fatalf("group create: %v", err)
	}
	if got := fetch.hits[remoteURI]; got != 1 {
		t.Fatalf("a repeated reference must be resolved once, fetched %d times", got)
	}
}

// expectGroupSignedAndValid satisfies the sign/validate pair that group creation now performs BEFORE it
// resolves playlist references, so tests exercising resolution behavior reach the code they are about.
// The ordering is deliberate (see CreatePlaylistGroup): a document that cannot be stored must not first
// cost outbound fetches.
func expectGroupSignedAndValid(t *testing.T, m *mocks.MockValidatorSigner) {
	t.Helper()
	signed := []byte(`{"kind":"signed-group"}`)
	group := mustDecodeGroup(t, signed)
	m.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil).AnyTimes()
	m.EXPECT().ValidatePlaylistGroup(gomock.Any()).Return(&group, nil).AnyTimes()
}

// staticFetcher serves one fixed body for any remote playlist URI.
type staticFetcher struct{ body []byte }

func (f staticFetcher) FetchPlaylist(_ context.Context, _ string) ([]byte, error) { return f.body, nil }

// A remote playlist this feed does not hold is created by the ingest, so it must clear the same bar as
// POST. An unsigned (or badly signed) remote document must fail the whole group mutation rather than be
// published here under the referencing party's request.
func TestCreatePlaylistGroup_remotePlaylistMustBeSelfSigned(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil)
	expectGroupSignedAndValid(t, mockDP1)
	remote := &playlist.Playlist{ID: "77777777-7777-4777-8777-777777777777", Slug: "remote"}
	mockDP1.EXPECT().ValidatePlaylist(gomock.Any()).Return(remote, nil)
	// Signatures verify cryptographically, but none matches a declared curator (there are none).
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil)

	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), gomock.Any()).Return(nil, store.ErrNotFound).AnyTimes()
	e := executor.New(mockStore, mockDP1, false, staticFetcher{body: []byte(`{"remote":true}`)}, testPublicBase)
	_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq("https://elsewhere.test/p.json"))
	if !executor.IsSignatureVerificationError(err) {
		t.Fatalf("want signature verification error for unsigned remote playlist, got %v", err)
	}
}

// Materializing a remote playlist is a create, so it must satisfy POST's identity rules too. The feed used
// to repair these — synthesizing a missing slug, slugifying a supplied one — which left the row's routing
// slug disagreeing with the slug inside the signed document, so the document served at that URL
// contradicted its own address and could never be replaced (replace requires identity equality).
func TestCreatePlaylistGroup_remotePlaylistMustSatisfyPOSTIdentityRules(t *testing.T) {
	t.Parallel()
	const remoteID = "77777777-7777-4777-8777-777777777777"

	cases := []struct {
		name   string
		remote *playlist.Playlist
	}{
		{
			name:   "missing slug is rejected rather than synthesized",
			remote: &playlist.Playlist{ID: remoteID, Created: testCreatedRFC},
		},
		{
			name:   "item without a UUID id",
			remote: &playlist.Playlist{ID: remoteID, Slug: "remote", Created: testCreatedRFC, Items: []playlist.PlaylistItem{{Source: "https://x"}}},
		},
		{
			name:   "created in the future",
			remote: &playlist.Playlist{ID: remoteID, Slug: "remote", Created: "2999-01-01T00:00:00Z"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			mockDP1 := mocks.NewMockValidatorSigner(ctrl)
			mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil)
			expectGroupSignedAndValid(t, mockDP1)
			tc.remote.Curators = []identity.Entity{{Key: testCuratorKid}}
			tc.remote.Signatures = []playlist.Signature{testSig(testCuratorKid)}
			mockDP1.EXPECT().ValidatePlaylist(gomock.Any()).Return(tc.remote, nil)
			mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil)

			mockStore := mocks.NewMockStore(ctrl)
			mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), gomock.Any()).Return(nil, store.ErrNotFound).AnyTimes()
			e := executor.New(mockStore, mockDP1, false, staticFetcher{body: []byte(`{"remote":true}`)}, testPublicBase)
			_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq("https://elsewhere.test/p.json"))
			if err == nil {
				t.Fatal("want the group mutation to fail")
			}
			if !executor.IsInvalidSubmissionError(err) && !executor.IsInvalidTimestampError(err) {
				t.Fatalf("want a client-correctable identity error, got %v", err)
			}
		})
	}
}

func TestCreatePlaylistGroup_remotePlaylistFailingCryptoIsRejected(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil)
	expectGroupSignedAndValid(t, mockDP1)
	remote := &playlist.Playlist{
		ID:         "77777777-7777-4777-8777-777777777777",
		Slug:       "remote",
		Curators:   []identity.Entity{{Key: testCuratorKid}},
		Signatures: []playlist.Signature{testSig(testCuratorKid)},
	}
	mockDP1.EXPECT().ValidatePlaylist(gomock.Any()).Return(remote, nil)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(false, []playlist.Signature{{Kid: testCuratorKid}}, nil)

	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), gomock.Any()).Return(nil, store.ErrNotFound).AnyTimes()
	e := executor.New(mockStore, mockDP1, false, staticFetcher{body: []byte(`{"remote":true}`)}, testPublicBase)
	_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq("https://elsewhere.test/p.json"))
	if !executor.IsSignatureVerificationError(err) {
		t.Fatalf("want signature verification error, got %v", err)
	}
}

func TestCreatePlaylistGroup_localPlaylistNotFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), gomock.Any()).Return(nil, store.ErrNotFound).AnyTimes()
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), gomock.Any()).Return(nil, store.ErrNotFound).AnyTimes()
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), gomock.Any()).Return(nil, store.ErrNotFound).AnyTimes()
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "missing").Return(nil, store.ErrNotFound)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil)
	expectGroupSignedAndValid(t, mockDP1)

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
	dailyBody := []byte(`{"id":"` + plID.String() + `","slug":"daily","title":"Daily","items":[{"source":"https://cdn.example.com/day1.html","displayAt":"2026-07-21T00:00:00"}]}`)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "daily").Return(&store.PlaylistRecord{
		ID:   plID,
		Slug: "daily",
		Raw:  dailyBody,
		Body: mustDecodePlaylist(t, dailyBody),
	}, nil)
	signed := []byte(`{"signed":true}`)
	parsedGroup := mustDecodeGroup(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylistGroup(signed).Return(&parsedGroup, nil),
	)
	mockStore.EXPECT().CreatePlaylistGroup(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.PlaylistGroupInput) {
		if len(in.Playlists) != 1 || displayAtValue(mustDecodePlaylist(t, in.Playlists[0].Raw).Items[0]) != "2026-07-21T00:00:00" {
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
	// Once, not twice: a repeated URI is resolved a single time and copied to every position it occupies,
	// so one URI cannot mean two different playlists within a request.
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "same").Return(&store.PlaylistRecord{
		ID: plID, Slug: "same", Body: plDoc,
	}, nil).Times(1)

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
	// No GetPlaylist expectation on purpose: signing precedes reference resolution, so a signing failure
	// must abandon the request before any lookup or fetch happens. If that ordering regressed, resolution
	// would call GetPlaylist and gomock would fail this test on the unexpected call.
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
	// No GetPlaylist expectation: post-sign validation runs before reference resolution, so a document
	// rejected there must never reach a lookup or fetch (see TestCreatePlaylistGroup_signError).
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
	if out == nil || !reflect.DeepEqual(out.Body, g) {
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
	if !reflect.DeepEqual(items[0].Body, recs[0].Body) {
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
	mockStore.EXPECT().DeletePlaylistGroup(gomock.Any(), id.String(), gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.IntentTargetPlaylistGroup, id.String(), "gid", testCuratorKid)
	if err := e.DeletePlaylistGroup(context.Background(), "gid", req); err != nil {
		t.Fatal(err)
	}
}

func TestReplacePlaylistGroup_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	expectIntentOK(mockDP1)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	gid := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	created := time.Date(2019, 6, 1, 12, 0, 0, 0, time.UTC)
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "keep-g").Return(&store.PlaylistGroupRecord{
		ID:   gid,
		Slug: "group-title",
		Body: playlistgroup.Group{
			Created: testCreatedRFC,
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
	mockStore.EXPECT().UpdatePlaylistGroup(gomock.Any(), gid.String(), gomock.Any(), gomock.Any()).Do(func(_ context.Context, _ string, in *store.PlaylistGroupInput, _ time.Time) {
		if in.ID != uuid.Nil || in.Slug != "" {
			t.Fatalf("update input should not set row id/slug (store resolves from idOrSlug): id=%v slug=%q", in.ID, in.Slug)
		}
		if len(in.Playlists) != 1 || in.Playlists[0].ID != plID {
			t.Fatalf("playlists: %+v", in.Playlists)
		}
		if !bytes.Equal(in.Raw, signed) {
			t.Fatalf("raw: %s want %s", in.Raw, signed)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	req := validGroupCreateReq(localPlaylistRef("pl"))
	req.Title = "New group title"
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	out, err := e.ReplacePlaylistGroup(context.Background(), "keep-g", req, replaceIntent(models.IntentTargetPlaylistGroup, gid.String(), "group-title", testCuratorKid))
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(out.Body, parsedGroup) {
		t.Fatal("out mismatch")
	}
}

func TestReplacePlaylistGroup_withSignatures_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	expectIntentOK(mockDP1)

	gid := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	created := time.Date(2019, 6, 1, 12, 0, 0, 0, time.UTC)
	curatorKid := "did:key:groupCuratorTest"
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "keep-g").Return(&store.PlaylistGroupRecord{
		ID:   gid,
		Slug: "group-title",
		Body: playlistgroup.Group{
			Created: testCreatedRFC,
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
	mockStore.EXPECT().UpdatePlaylistGroup(gomock.Any(), gid.String(), gomock.Any(), gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	req := validGroupCreateReq(localPlaylistRef("pl"))
	req.Title = "New group title"
	req.Curator = curatorKid
	req.Signatures = []playlist.Signature{{Kid: curatorKid, Alg: "ed25519", Sig: "sig"}}

	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	out, err := e.ReplacePlaylistGroup(context.Background(), "keep-g", req, replaceIntent(models.IntentTargetPlaylistGroup, gid.String(), "group-title", curatorKid))
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(out.Body, parsedGroup) {
		t.Fatal("out mismatch")
	}
}

func TestReplacePlaylistGroup_notFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "x").Return(nil, store.ErrNotFound)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, testPublicBase)
	_, err := e.ReplacePlaylistGroup(context.Background(), "x", validGroupCreateReq(localPlaylistRef("y")), nil)
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
		Body: playlistgroup.Group{ID: id.String(), Slug: slug, Curator: testCuratorKid, Created: testCreatedRFC},
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
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "gid").Return(storedOwnedGroup(id, "group-title"), nil)
	ref := memberPlaylistExpect(t, mockStore)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	req := validGroupCreateReq(ref) // curator == stored (immutability passes)
	req.Signatures = []playlist.Signature{testSig("did:key:notGroupOwner")}
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	if _, err := e.ReplacePlaylistGroup(context.Background(), "gid", req, nil); !executor.IsForbiddenError(err) {
		t.Fatalf("want forbidden (not owner), got %v", err)
	}
}

func TestReplacePlaylistGroup_verifyCryptoFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "gid").Return(storedOwnedGroup(id, "group-title"), nil)
	ref := memberPlaylistExpect(t, mockStore)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(false, []playlist.Signature{{Kid: "x"}}, nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	if _, err := e.ReplacePlaylistGroup(context.Background(), "gid", validGroupCreateReq(ref), nil); !executor.IsSignatureVerificationError(err) {
		t.Fatalf("want signature-verification error (ok=false), got %v", err)
	}
}

func TestReplacePlaylistGroup_verifyCryptoError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "gid").Return(storedOwnedGroup(id, "group-title"), nil)
	ref := memberPlaylistExpect(t, mockStore)
	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(false, nil, errors.New("boom"))

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	if _, err := e.ReplacePlaylistGroup(context.Background(), "gid", validGroupCreateReq(ref), nil); !executor.IsSignatureVerificationError(err) {
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
	req := deleteReq(models.IntentTargetPlaylistGroup, id.String(), "gid", "did:key:notGroupOwner")
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
	req := deleteReq(models.IntentTargetPlaylistGroup, id.String(), "wrong-slug", testCuratorKid)
	if err := e.DeletePlaylistGroup(context.Background(), "gid", req); !executor.IsIntentError(err) {
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
			Created:   testCreatedRFC,
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	if _, err := e.ReplaceChannel(context.Background(), "cid", req, nil); !executor.IsForbiddenError(err) {
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
	if _, err := e.ReplaceChannel(context.Background(), "cid", validChannelCreateReq("cid", ref), nil); !executor.IsSignatureVerificationError(err) {
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
	if _, err := e.ReplaceChannel(context.Background(), "cid", validChannelCreateReq("cid", ref), nil); !executor.IsSignatureVerificationError(err) {
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
	req := deleteReq(models.IntentTargetChannel, id.String(), "cid", "did:key:notPublisher")
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
	req := deleteReq(models.IntentTargetChannel, id.String(), "wrong-slug", testPublisherKid)
	if err := e.DeleteChannel(context.Background(), "cid", req); !executor.IsIntentError(err) {
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
	out, err := e.ReplaceChannel(context.Background(), "c", validChannelCreateReq("c", localPlaylistRef("p")), nil)
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
	err := e.DeleteChannel(context.Background(), "c", deleteReq(models.IntentTargetChannel, "id", "c", testPublisherKid))
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
	req.Raw = mustJSONRaw(req)                                 // document bytes must reflect the final request
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
		if !bytes.Equal(in.Raw, signed) {
			t.Fatalf("raw: %s want %s", in.Raw, signed)
		}
	}).Return(nil)

	notifications := &recordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	out, err := e.CreateChannel(notifiedMutationContext(t), validChannelCreateReq("My Channel", localPlaylistRef("pl-ch")))
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(out.Body, wantCh) {
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	if out == nil || !reflect.DeepEqual(out.Body, ch) {
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
	if !reflect.DeepEqual(items[0].Body, recs[0].Body) {
		t.Fatalf("body mismatch: %+v", items[0].Body)
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
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), gomock.Any()).Return(nil, store.ErrNotFound).AnyTimes()
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	// A referenced remote playlist this feed does not hold is created by the ingest, so it must be
	// self-signed by a declared curator (same bar as POST) — see resolveOnePlaylistRef.
	// It must also satisfy POST's identity rules: a verbatim slug, item UUIDs, and a non-future created.
	remotePlaylist := &playlist.Playlist{
		ID:         "77777777-7777-4777-8777-777777777777",
		Slug:       "remote",
		Created:    testCreatedRFC,
		Curators:   []identity.Entity{{Key: testCuratorKid}},
		Signatures: []playlist.Signature{testSig(testCuratorKid)},
	}
	mockDP1.EXPECT().ValidatePlaylistWithExtension(gomock.Any()).Return(remotePlaylist, nil).Times(9)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
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
	chReq := &models.ChannelCreateRequest{
		Title:      "Second fetch batch",
		Slug:       "second-fetch-batch",
		Playlists:  playlists,
		ID:         stringPtr("44444444-4444-4444-4444-444444444444"),
		Created:    stringPtr(testCreatedRFC),
		Publisher:  &identity.Entity{Key: testPublisherKid},
		Signatures: []playlist.Signature{testSig(testPublisherKid)},
	}
	chReq.Raw = mustJSONRaw(chReq)
	_, err := e.CreateChannel(ctx, chReq)
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
	mockStore.EXPECT().DeleteChannel(gomock.Any(), cid.String(), gomock.Any()).Return(nil)

	notifications := &recordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	req := deleteReq(models.IntentTargetChannel, cid.String(), "cid", testPublisherKid)
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
	mockStore.EXPECT().DeleteChannel(gomock.Any(), cid.String(), gomock.Any()).DoAndReturn(func(mutationCtx context.Context, _ string, _ time.Time) error {
		cancel() // The row committed just before the HTTP request context was canceled.
		if err := mutationCtx.Err(); err != nil {
			t.Fatalf("mutation context error after request cancellation = %v, want detached context", err)
		}
		return nil
	})

	notifications := &contextRecordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	req := deleteReq(models.IntentTargetChannel, cid.String(), "cid", testPublisherKid)
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
	req := deleteReq(models.IntentTargetChannel, cid.String(), "cid", testPublisherKid)
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
	mockStore.EXPECT().DeleteChannel(gomock.Any(), cid.String(), gomock.Any()).DoAndReturn(func(mutationCtx context.Context, _ string, _ time.Time) error {
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
	req := deleteReq(models.IntentTargetChannel, cid.String(), "cid", testPublisherKid)
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
	expectIntentOK(mockDP1)
	mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	cid := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	created := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	mockStore.EXPECT().GetChannel(gomock.Any(), "ch-slug").Return(&store.ChannelRecord{
		ID:   cid,
		Slug: "ch-slug",
		Body: channels.Channel{
			Created:   testCreatedRFC,
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
	mockStore.EXPECT().UpdateChannel(gomock.Any(), cid.String(), gomock.Any(), gomock.Any()).Do(func(_ context.Context, _ string, in *store.ChannelInput, _ time.Time) {
		if in.ID != uuid.Nil || in.Slug != "" {
			t.Fatalf("update input should not set row id/slug: id=%v slug=%q", in.ID, in.Slug)
		}
		if len(in.Playlists) != 1 || in.Playlists[0].ID != plID {
			t.Fatalf("playlists: %+v", in.Playlists)
		}
		if !bytes.Equal(in.Raw, signed) {
			t.Fatalf("raw: %s want %s", in.Raw, signed)
		}
	}).Return(nil)

	notifications := &recordingNotificationClient{}
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase, executor.WithNotificationClient(notifications))
	req := validChannelCreateReq("ch-slug", localPlaylistRef("pl2"))
	req.Title = "New title"
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	out, err := e.ReplaceChannel(notifiedMutationContext(t), "ch-slug", req, replaceIntent(models.IntentTargetChannel, cid.String(), "ch-slug", testPublisherKid))
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(out.Body, parsedCh) {
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
	expectIntentOK(mockDP1)

	cid := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	created := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	pubKid := "did:key:channelPublisherTest"
	mockStore.EXPECT().GetChannel(gomock.Any(), "ch-slug").Return(&store.ChannelRecord{
		ID:   cid,
		Slug: "ch-slug",
		Body: channels.Channel{
			Created:   testCreatedRFC,
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
	mockStore.EXPECT().UpdateChannel(gomock.Any(), cid.String(), gomock.Any(), gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	req := validChannelCreateReq("ch-slug", localPlaylistRef("pl2"))
	req.Title = "New title"
	req.Publisher = &identity.Entity{Key: pubKid}
	req.Signatures = []playlist.Signature{{Kid: pubKid, Alg: "ed25519", Sig: "sig"}}

	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	out, err := e.ReplaceChannel(context.Background(), "ch-slug", req, replaceIntent(models.IntentTargetChannel, cid.String(), "ch-slug", pubKid))
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(out.Body, parsedCh) {
		t.Fatal("out mismatch")
	}
}

func TestReplaceChannel_notFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetChannel(gomock.Any(), "missing").Return(nil, store.ErrNotFound)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), true, nil, testPublicBase)
	_, err := e.ReplaceChannel(context.Background(), "missing", validChannelCreateReq("x", localPlaylistRef("p")), nil)
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", req, nil); !executor.IsSignaturesRequiredError(err) {
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
	mockStore.EXPECT().DeletePlaylist(gomock.Any(), id.String(), gomock.Any()).Return(errors.New("db down"))

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.IntentTargetPlaylist, id.String(), "id-1", testCuratorKid)
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	if _, err := e.ReplacePlaylistGroup(context.Background(), "gid", req, nil); !executor.IsSignaturesRequiredError(err) {
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
	mockStore.EXPECT().DeletePlaylistGroup(gomock.Any(), id.String(), gomock.Any()).Return(errors.New("db down"))

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.IntentTargetPlaylistGroup, id.String(), "gid", testCuratorKid)
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
	if _, err := e.ReplaceChannel(context.Background(), "cid", req, nil); !executor.IsSignaturesRequiredError(err) {
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
	req := deleteReq(models.IntentTargetChannel, id.String(), "cid", testPublisherKid)
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
	expectIntentOK(mockDP1)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(storedPlaylistRecord(t, id, "test-playlist"), nil)
	signed := []byte(`{"replaced":true}`)
	parsed := mustDecodePlaylist(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil),
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), id.String(), gomock.Any(), gomock.Any()).Return(errors.New("db down"))

	e := executor.New(mockStore, mockDP1, false, nil, "")
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", validCreateReq(), replaceIntent(models.IntentTargetPlaylist, id.String(), "test-playlist", testCuratorKid)); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("got %v", err)
	}
}

func TestReplacePlaylistGroup_storeError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	expectIntentOK(mockDP1)
	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "gid").Return(storedOwnedGroup(id, "group-title"), nil)
	ref := memberPlaylistExpect(t, mockStore)
	signed := []byte(`{"g":true}`)
	parsedGroup := mustDecodeGroup(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil),
		mockDP1.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylistGroup(signed).Return(&parsedGroup, nil),
	)
	mockStore.EXPECT().UpdatePlaylistGroup(gomock.Any(), id.String(), gomock.Any(), gomock.Any()).Return(errors.New("db down"))

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	if _, err := e.ReplacePlaylistGroup(context.Background(), "gid", validGroupCreateReq(ref), replaceIntent(models.IntentTargetPlaylistGroup, id.String(), "group-title", testCuratorKid)); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("got %v", err)
	}
}

func TestReplaceChannel_storeError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	expectIntentOK(mockDP1)
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
	mockStore.EXPECT().UpdateChannel(gomock.Any(), id.String(), gomock.Any(), gomock.Any()).Return(errors.New("db down"))

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	if _, err := e.ReplaceChannel(context.Background(), "cid", validChannelCreateReq("cid", ref), replaceIntent(models.IntentTargetChannel, id.String(), "cid", testPublisherKid)); err == nil || !strings.Contains(err.Error(), "db down") {
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
	mockStore.EXPECT().CreatePlaylist(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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
	req.Raw = mustJSONRaw(req) // document bytes must reflect the final request
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

// The executor authorizes a request against the record it read, then writes. These tests pin that the
// generation observed at authorization (UpdatedAt) is the one carried into the write, and that the
// store's refusal surfaces unchanged — that pairing is what stops a decision made about one document
// from applying to a different one that replaced it under the same client-chosen id.
func TestReplacePlaylist_forwardsAuthorizedGeneration(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	expectIntentOK(mockDP1)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	authorizedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	rec := storedPlaylistRecord(t, id, "test-playlist")
	rec.UpdatedAt = authorizedAt
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(rec, nil)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil)

	signed := []byte(`{"replaced":true}`)
	parsed := mustDecodePlaylist(t, signed)
	mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil)
	mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil)
	// Exact value, not gomock.Any(): the write must be bound to the generation just authorized.
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), id.String(), gomock.Any(), authorizedAt).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := validCreateReq()
	req.Title = "New title"
	req.Raw = mustJSONRaw(req)
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", req, replaceIntent(models.IntentTargetPlaylist, id.String(), "test-playlist", testCuratorKid)); err != nil {
		t.Fatal(err)
	}
}

func TestReplacePlaylist_concurrentModification(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	expectIntentOK(mockDP1)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(storedPlaylistRecord(t, id, "test-playlist"), nil)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil)

	signed := []byte(`{"replaced":true}`)
	parsed := mustDecodePlaylist(t, signed)
	mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil)
	mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil)
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), id.String(), gomock.Any(), gomock.Any()).
		Return(store.ErrConcurrentModification)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := validCreateReq()
	req.Raw = mustJSONRaw(req)
	_, err := e.ReplacePlaylist(context.Background(), "keep-me", req, replaceIntent(models.IntentTargetPlaylist, id.String(), "test-playlist", testCuratorKid))
	if !errors.Is(err, store.ErrConcurrentModification) {
		t.Fatalf("want ErrConcurrentModification, got %v", err)
	}
}

func TestDeletePlaylist_forwardsAuthorizedGeneration(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	authorizedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	rec := storedOwnedPlaylist(id)
	rec.UpdatedAt = authorizedAt
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(rec, nil)
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil)
	mockStore.EXPECT().DeletePlaylist(gomock.Any(), id.String(), authorizedAt).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.IntentTargetPlaylist, id.String(), "id-1", testCuratorKid)
	if err := e.DeletePlaylist(context.Background(), "id-1", req); err != nil {
		t.Fatal(err)
	}
}

func TestDeletePlaylist_concurrentModification(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "id-1").Return(storedOwnedPlaylist(id), nil)
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil)
	mockStore.EXPECT().DeletePlaylist(gomock.Any(), id.String(), gomock.Any()).
		Return(store.ErrConcurrentModification)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := deleteReq(models.IntentTargetPlaylist, id.String(), "id-1", testCuratorKid)
	err := e.DeletePlaylist(context.Background(), "id-1", req)
	if !errors.Is(err, store.ErrConcurrentModification) {
		t.Fatalf("want ErrConcurrentModification, got %v", err)
	}
}

// --- PUT signed mutation-intent (replay protection) -------------------------------------------------
//
// The document's own signatures prove the owner authored the content; the intent proves the owner is
// asking for it to replace THIS resource NOW. Each of these cases is a way that second proof can be
// missing or wrong, and every one of them is a replay the document signatures alone would have allowed.

// replaceIntentFixture wires a replace that passes every document-level check, so the only thing left to
// vary is the intent.
func replaceIntentFixture(t *testing.T, ctrl *gomock.Controller) (*mocks.MockStore, *mocks.MockValidatorSigner, uuid.UUID, *models.PlaylistReplaceRequest) {
	t.Helper()
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(storedPlaylistRecord(t, id, "test-playlist"), nil)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()
	return mockStore, mockDP1, id, validCreateReq()
}

func TestReplacePlaylist_intentStaleCreated(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore, mockDP1, id, req := replaceIntentFixture(t, ctrl)
	mockDP1.EXPECT().PayloadHash(gomock.Any()).Return(testPayloadHash, nil).AnyTimes()

	intent := replaceIntent(models.IntentTargetPlaylist, id.String(), "test-playlist", testCuratorKid)
	intent.Created = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	intent.Raw = mustJSONRaw(intent)

	e := executor.New(mockStore, mockDP1, false, nil, "", executor.WithIntentClockSkew(time.Minute))
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", req, intent); !executor.IsInvalidTimestampError(err) {
		t.Fatalf("want invalid-timestamp (stale intent), got %v", err)
	}
}

func TestReplacePlaylist_intentNotOwner(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore, mockDP1, id, req := replaceIntentFixture(t, ctrl)
	mockDP1.EXPECT().PayloadHash(gomock.Any()).Return(testPayloadHash, nil).AnyTimes()
	mockDP1.EXPECT().VerifySignatures(gomock.Any()).Return(true, nil, nil).AnyTimes()

	// The intent verifies cryptographically but is signed by a key the stored playlist does not own.
	intent := replaceIntent(models.IntentTargetPlaylist, id.String(), "test-playlist", "did:key:z6MkNotAnOwnerXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX")

	e := executor.New(mockStore, mockDP1, false, nil, "")
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", req, intent); !executor.IsForbiddenError(err) {
		t.Fatalf("want forbidden (intent not signed by an owner), got %v", err)
	}
}

func TestReplacePlaylist_intentPayloadHashMismatch(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore, mockDP1, id, req := replaceIntentFixture(t, ctrl)
	// The document hashes to testPayloadHash, but the intent names a different document.
	mockDP1.EXPECT().PayloadHash(gomock.Any()).Return(testPayloadHash, nil).AnyTimes()

	intent := replaceIntent(models.IntentTargetPlaylist, id.String(), "test-playlist", testCuratorKid)
	intent.PayloadHash = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	intent.Raw = mustJSONRaw(intent)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", req, intent); !executor.IsIntentError(err) {
		t.Fatalf("want intent error (payloadHash does not bind this document), got %v", err)
	}
}

func TestReplacePlaylist_intentWrongAction(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore, mockDP1, id, req := replaceIntentFixture(t, ctrl)

	// A delete-intent must not authorize a replace, even signed by the owner.
	intent := replaceIntent(models.IntentTargetPlaylist, id.String(), "test-playlist", testCuratorKid)
	intent.Action = models.IntentActionDelete
	intent.Raw = mustJSONRaw(intent)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", req, intent); !executor.IsIntentError(err) {
		t.Fatalf("want intent error (wrong action), got %v", err)
	}
}

func TestReplacePlaylist_intentWrongTarget(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore, mockDP1, id, req := replaceIntentFixture(t, ctrl)

	// An intent signed for a different resource must not authorize this one.
	intent := replaceIntent(models.IntentTargetPlaylist, "22222222-2222-2222-2222-222222222222", "test-playlist", testCuratorKid)
	_ = id

	e := executor.New(mockStore, mockDP1, false, nil, "")
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", req, intent); !executor.IsIntentError(err) {
		t.Fatalf("want intent error (target id does not match), got %v", err)
	}
}

func TestReplacePlaylist_intentMissing(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore, mockDP1, _, req := replaceIntentFixture(t, ctrl)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	if _, err := e.ReplacePlaylist(context.Background(), "keep-me", req, nil); !executor.IsIntentError(err) {
		t.Fatalf("want intent error (no authorization), got %v", err)
	}
}

// Reference-only ingestion: a playlist this feed already holds is linked, not re-judged.
//
// The contract says ingestion never rewrites a stored playlist, so the remote representation of an
// already-stored member is irrelevant — and must not be able to fail the mutation. Before this, resolution
// validated and signature-verified the fetched body first, so a member whose origin later rotted, rotated
// keys, or served junk would break every new group referencing that URL even though nothing about the
// stored playlist needed to change.
//
// The mock is the assertion: ValidatePlaylist and VerifyPlaylistSignatures are never EXPECTed, so gomock
// fails the test if resolution touches the remote body at all.
func TestCreatePlaylistGroup_storedMemberIgnoresRemoteBody(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), gomock.Any()).Return(nil, store.ErrNotFound).AnyTimes()

	storedID := uuid.MustParse("77777777-7777-4777-8777-777777777777")
	storedRaw := json.RawMessage(`{"dpVersion":"1.1.0","id":"77777777-7777-4777-8777-777777777777","slug":"stored-one","title":"trusted"}`)

	// The origin now serves a body that could never clear the create bar: no signatures, no curators, not
	// even schema-valid. It does still name the id, which is all identity resolution needs.
	rotted := []byte(`{"id":"77777777-7777-4777-8777-777777777777","garbage":true}`)

	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), storedID.String()).
		Return(&store.PlaylistRecord{ID: storedID, Slug: "stored-one", Raw: storedRaw}, nil)

	signed := []byte(`{"kind":"signed-group-stored-member"}`)
	wantGroup := mustDecodeGroup(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylistGroup(signed).Return(&wantGroup, nil),
	)
	mockStore.EXPECT().CreatePlaylistGroup(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.PlaylistGroupInput) {
		if len(in.Playlists) != 1 {
			t.Fatalf("want one member, got %d", len(in.Playlists))
		}
		got := in.Playlists[0]
		if got.ID != storedID || got.Slug != "stored-one" {
			t.Fatalf("member identity came from the remote body, not the store: %+v", got)
		}
		// The stored bytes must be linked untouched — linking the fetched body would silently republish
		// content whose signatures were never checked.
		if string(got.Raw) != string(storedRaw) {
			t.Fatalf("member raw is not the stored bytes:\n got %s\nwant %s", got.Raw, storedRaw)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, false, staticFetcher{body: rotted}, testPublicBase)
	req := validGroupCreateReq("https://elsewhere.test/p.json")
	req.Raw = mustJSONRaw(req)
	if _, err := e.CreatePlaylistGroup(context.Background(), req); err != nil {
		t.Fatalf("group creation must survive a member whose origin no longer validates: %v", err)
	}
}

// The complement: an id this feed does not hold is *created* by the ingest, so the full create bar still
// applies. Without this, the store lookup above would be a hole rather than a shortcut.
func TestCreatePlaylistGroup_unknownRemoteIDStillFullyValidated(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylistBySourceURI(gomock.Any(), gomock.Any()).Return(nil, store.ErrNotFound).AnyTimes()

	newID := uuid.MustParse("88888888-8888-4888-8888-888888888888")
	body := []byte(`{"id":"88888888-8888-4888-8888-888888888888","unsigned":true}`)

	mockDP1.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil)
	expectGroupSignedAndValid(t, mockDP1)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), newID.String()).Return(nil, store.ErrNotFound)

	// Not held here, so resolution must fall through to validation — and this body carries no curator
	// signature, so the mutation fails rather than publishing it.
	remote := &playlist.Playlist{ID: newID.String(), Slug: "remote"}
	mockDP1.EXPECT().ValidatePlaylist(gomock.Any()).Return(remote, nil)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil)

	e := executor.New(mockStore, mockDP1, false, staticFetcher{body: body}, testPublicBase)
	_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq("https://elsewhere.test/p.json"))
	if !executor.IsSignatureVerificationError(err) {
		t.Fatalf("an unheld remote id must still face full verification, got %v", err)
	}
}
