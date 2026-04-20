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
	dp1playlists "github.com/display-protocol/dp1-go/extension/playlists"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/playlistgroup"
	"github.com/display-protocol/dp1-go/sign"

	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/mocks"
	"github.com/display-protocol/dp1-feed-v2/internal/models"
	"github.com/display-protocol/dp1-feed-v2/internal/store"
	"github.com/display-protocol/dp1-feed-v2/internal/utils"
)

func validCreateReq() *models.PlaylistCreateRequest {
	return &models.PlaylistCreateRequest{
		DPVersion: "1.1.0",
		Title:     "Test playlist",
		Items: []playlist.PlaylistItem{
			{Source: "https://example.com/item"},
		},
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

func TestCreatePlaylist_signError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
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

func TestDeletePlaylist(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().DeletePlaylist(gomock.Any(), "id-1").Return(nil)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	if err := e.DeletePlaylist(context.Background(), "id-1"); err != nil {
		t.Fatal(err)
	}
}

func TestReplacePlaylist_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	existing := []byte(`{"dpVersion":"1.1.0","id":"11111111-1111-1111-1111-111111111111","slug":"keep-me","title":"Old","created":"2020-01-02T03:04:05Z","items":[{"source":"https://old"}]}`)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "keep-me").Return(&store.PlaylistRecord{
		ID:   id,
		Slug: "keep-me",
		Body: mustDecodePlaylist(t, existing),
	}, nil)

	signed := []byte(`{"replaced":true}`)
	parsed := mustDecodePlaylist(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), "keep-me", &parsed).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	req := validCreateReq()
	req.Title = "New title"
	out, err := e.ReplacePlaylist(context.Background(), "keep-me", req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, parsed) {
		t.Fatalf("out mismatch")
	}
}

func TestReplacePlaylist_withSignatures_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	existing := []byte(`{"dpVersion":"1.1.0","id":"11111111-1111-1111-1111-111111111111","slug":"keep-me","title":"Old","created":"2020-01-02T03:04:05Z","items":[{"source":"https://old"}]}`)
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
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), "keep-me", &parsed).Return(nil)

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

func TestPlaylist_replaceAndUpdate_parseDocumentCreatedFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		created string
	}{
		{"empty", ""},
		{"malformed", "not-a-valid-rfc3339"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/replace", func(t *testing.T) {
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
					Items:     []playlist.PlaylistItem{{Source: "https://x"}},
				},
			}, nil)

			e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, "")
			_, err := e.ReplacePlaylist(context.Background(), "pl", validCreateReq())
			if err == nil || !strings.Contains(err.Error(), "parse document created") {
				t.Fatalf("expected parse document created error, got %v", err)
			}
		})
		t.Run(tc.name+"/update", func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			mockStore := mocks.NewMockStore(ctrl)
			id := uuid.MustParse("44444444-4444-4444-4444-444444444444")
			mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{
				ID:   id,
				Slug: "pl",
				Body: playlist.Playlist{
					DPVersion: "1.1.0",
					Title:     "T",
					Created:   tc.created,
					Items:     []playlist.PlaylistItem{{Source: "https://x"}},
				},
			}, nil)

			e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, "")
			title := "New"
			_, err := e.UpdatePlaylist(context.Background(), "pl", &models.PlaylistUpdateRequest{Title: &title})
			if err == nil || !strings.Contains(err.Error(), "parse document created") {
				t.Fatalf("expected parse document created error, got %v", err)
			}
		})
	}
}

func TestUpdatePlaylist_preservesPlaylistLevelNote(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	created := time.Date(2020, 5, 15, 10, 30, 0, 0, time.UTC)
	noteText := "playlist-level note preserved across PATCH"
	itemID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	existingBody := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "Old Title",
		Slug:      "old-slug",
		Created:   created.UTC().Format(time.RFC3339Nano),
		Items:     []playlist.PlaylistItem{{ID: itemID.String(), Source: "https://old.example/item1"}},
		Note:      &dp1playlists.Note{Text: noteText},
	}
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "old-slug").Return(&store.PlaylistRecord{
		ID:        id,
		Slug:      "old-slug",
		Body:      existingBody,
		CreatedAt: created,
	}, nil)

	var preSign []byte
	signed := []byte(`{"dpVersion":"1.1.0"}`)
	parsed := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "Updated Title",
		Slug:      "old-slug",
		Created:   existingBody.Created,
		Items:     existingBody.Items,
		Note:      existingBody.Note,
	}
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).DoAndReturn(func(raw []byte, ts time.Time) ([]byte, error) {
			preSign = append([]byte(nil), raw...)
			return signed, nil
		}),
		mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), "old-slug", &parsed).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	newTitle := "Updated Title"
	req := &models.PlaylistUpdateRequest{Title: &newTitle}
	_, err := e.UpdatePlaylist(context.Background(), "old-slug", req)
	if err != nil {
		t.Fatal(err)
	}
	var check playlist.Playlist
	if err := json.Unmarshal(preSign, &check); err != nil {
		t.Fatalf("pre-sign JSON: %v", err)
	}
	if check.Note == nil || check.Note.Text != noteText {
		t.Fatalf("pre-sign document should keep playlist note: got %+v", check.Note)
	}
}

func TestUpdatePlaylist_success_partialFields(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	created := time.Date(2020, 5, 15, 10, 30, 0, 0, time.UTC)
	existingBody := playlist.Playlist{
		DPVersion:  "1.1.0",
		Title:      "Old Title",
		Slug:       "old-slug",
		Created:    created.UTC().Format(time.RFC3339Nano),
		Summary:    "Old summary",
		CoverImage: "https://old.example/cover.jpg",
		Items: []playlist.PlaylistItem{
			{Source: "https://old.example/item1"},
		},
	}
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "old-slug").Return(&store.PlaylistRecord{
		ID:        id,
		Slug:      "old-slug",
		Body:      existingBody,
		CreatedAt: created,
	}, nil)

	signed := []byte(`{"dpVersion":"1.1.0","title":"Updated Title","slug":"old-slug","summary":"Old summary","items":[{"source":"https://old.example/item1"}]}`)
	parsed := mustDecodePlaylist(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), "old-slug", &parsed).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	newTitle := "Updated Title"
	req := &models.PlaylistUpdateRequest{
		Title: &newTitle,
	}
	out, err := e.UpdatePlaylist(context.Background(), "old-slug", req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, parsed) {
		t.Fatal("out mismatch")
	}
}

func TestUpdatePlaylist_withSignatures_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	created := time.Date(2020, 5, 15, 10, 30, 0, 0, time.UTC)
	kid := "did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"
	existingBody := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "Old Title",
		Slug:      "old-slug",
		Created:   created.UTC().Format(time.RFC3339Nano),
		Summary:   "Old summary",
		Curators:  []identity.Entity{{Key: kid}},
		Items: []playlist.PlaylistItem{
			{Source: "https://old.example/item1"},
		},
	}
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "old-slug").Return(&store.PlaylistRecord{
		ID:        id,
		Slug:      "old-slug",
		Body:      existingBody,
		CreatedAt: created,
	}, nil)

	signed := []byte(`{"dpVersion":"1.1.0","title":"Updated Title","slug":"old-slug"}`)
	parsed := mustDecodePlaylist(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil),
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), "old-slug", &parsed).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	newTitle := "Updated Title"
	req := &models.PlaylistUpdateRequest{
		Title: &newTitle,
		Signatures: []playlist.Signature{
			{Kid: kid, Alg: "ed25519", Sig: "user-sig"},
		},
	}
	out, err := e.UpdatePlaylist(context.Background(), "old-slug", req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, parsed) {
		t.Fatal("out mismatch")
	}
}

func TestUpdatePlaylist_success_multipleFields(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	docCreated := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	existingBody := playlist.Playlist{
		DPVersion:  "1.1.0",
		Title:      "Old",
		Created:    docCreated.UTC().Format(time.RFC3339Nano),
		Summary:    "Old summary",
		CoverImage: "https://old.example/img.jpg",
		Items: []playlist.PlaylistItem{
			{Source: "https://old.example/item"},
		},
	}
	mockStore.EXPECT().GetPlaylist(gomock.Any(), id.String()).Return(&store.PlaylistRecord{
		ID:   id,
		Slug: "test-slug",
		Body: existingBody,
	}, nil)

	signed := []byte(`{"updated":true}`)
	parsed := mustDecodePlaylist(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylist(signed).Return(&parsed, nil),
	)
	mockStore.EXPECT().UpdatePlaylist(gomock.Any(), id.String(), &parsed).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	newTitle := "Updated Title"
	newSummary := "Updated summary"
	newItems := []playlist.PlaylistItem{{Source: "https://new.example/item"}}
	req := &models.PlaylistUpdateRequest{
		Title:   &newTitle,
		Summary: &newSummary,
		Items:   newItems,
	}
	out, err := e.UpdatePlaylist(context.Background(), id.String(), req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("expected non-nil output")
	}
}

func TestUpdatePlaylist_notFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "missing").Return(nil, store.ErrNotFound)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	newTitle := "New"
	_, err := e.UpdatePlaylist(context.Background(), "missing", &models.PlaylistUpdateRequest{Title: &newTitle})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestUpdatePlaylist_signError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	signCreated := time.Date(2018, 2, 2, 0, 0, 0, 0, time.UTC)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), id.String()).Return(&store.PlaylistRecord{
		ID:   id,
		Slug: "test",
		Body: playlist.Playlist{
			DPVersion: "1.1.0",
			Title:     "Old",
			Created:   signCreated.UTC().Format(time.RFC3339Nano),
			Items:     []playlist.PlaylistItem{{Source: "https://x"}},
		},
	}, nil)

	signErr := errors.New("sign failed")
	mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(nil, signErr)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	newTitle := "New"
	_, err := e.UpdatePlaylist(context.Background(), id.String(), &models.PlaylistUpdateRequest{Title: &newTitle})
	if err == nil || !strings.Contains(err.Error(), "sign") {
		t.Fatalf("expected sign error, got %v", err)
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
		Title:     "Group title",
		Playlists: uris,
	}
}

func validChannelCreateReq(slug string, uris ...string) *models.ChannelCreateRequest {
	return &models.ChannelCreateRequest{
		Title:     "Channel title",
		Slug:      slug,
		Playlists: uris,
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

func TestCreatePlaylistGroup_emptyPlaylists(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, testPublicBase)
	_, err := e.CreatePlaylistGroup(context.Background(), &models.PlaylistGroupCreateRequest{
		Title:     "x",
		Playlists: nil,
	})
	if err == nil || !strings.Contains(err.Error(), "playlists must be non-empty") {
		t.Fatalf("got %v", err)
	}
}

func TestCreatePlaylistGroup_externalURINoFetcher(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, testPublicBase)
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

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, testPublicBase)
	_, err := e.CreatePlaylistGroup(context.Background(), validGroupCreateReq(localPlaylistRef("missing")))
	if err == nil || !strings.Contains(err.Error(), "local playlist") {
		t.Fatalf("got %v", err)
	}
}

func TestCreatePlaylistGroup_repeatedURIPreservesOrder(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
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
	mockStore.EXPECT().DeletePlaylistGroup(gomock.Any(), "gid").Return(nil)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, "")
	if err := e.DeletePlaylistGroup(context.Background(), "gid"); err != nil {
		t.Fatal(err)
	}
}

func TestReplacePlaylistGroup_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	gid := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	created := time.Date(2019, 6, 1, 12, 0, 0, 0, time.UTC)
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "keep-g").Return(&store.PlaylistGroupRecord{
		ID:   gid,
		Slug: "keep-g",
		Body: playlistgroup.Group{
			Created: created.UTC().Format(time.RFC3339Nano),
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
	mockStore.EXPECT().UpdatePlaylistGroup(gomock.Any(), "keep-g", gomock.Any()).Do(func(_ context.Context, _ string, in *store.PlaylistGroupInput) {
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
	mockStore.EXPECT().UpdatePlaylistGroup(gomock.Any(), "keep-g", gomock.Any()).Return(nil)

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

func TestUpdatePlaylistGroup_success_partialFields(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	gid := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	created := time.Date(2021, 3, 10, 14, 25, 0, 0, time.UTC)
	existingBody := playlistgroup.Group{
		Title:      "Old Group Title",
		Slug:       "old-group",
		Created:    created.UTC().Format(time.RFC3339Nano),
		Playlists:  []string{localPlaylistRef("pl1")},
		Curator:    "Old Curator",
		Summary:    "Old summary",
		CoverImage: "https://old.example/cover.jpg",
	}
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "old-group").Return(&store.PlaylistGroupRecord{
		ID:        gid,
		Slug:      "old-group",
		Body:      existingBody,
		CreatedAt: created,
	}, nil)

	plID := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl1").Return(&store.PlaylistRecord{
		ID: plID, Slug: "pl1", Body: mustDecodePlaylist(t, []byte(`{"id":"ffffffff-ffff-ffff-ffff-ffffffffffff"}`)),
	}, nil)

	signed := []byte(`{"title":"Updated Group Title","playlists":["` + localPlaylistRef("pl1") + `"]}`)
	parsedGroup := mustDecodeGroup(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylistGroup(signed).Return(&parsedGroup, nil),
	)
	mockStore.EXPECT().UpdatePlaylistGroup(gomock.Any(), "old-group", gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	newTitle := "Updated Group Title"
	req := &models.PlaylistGroupUpdateRequest{
		Title: &newTitle,
	}
	out, err := e.UpdatePlaylistGroup(context.Background(), "old-group", req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("expected non-nil output")
	}
}

func TestUpdatePlaylistGroup_success_updatePlaylists(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	gid := uuid.MustParse("12121212-1212-1212-1212-121212121212")
	groupCreated := time.Date(2020, 8, 8, 8, 0, 0, 0, time.UTC)
	existingBody := playlistgroup.Group{
		Title:     "Group",
		Created:   groupCreated.UTC().Format(time.RFC3339Nano),
		Playlists: []string{localPlaylistRef("old-pl")},
	}
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), gid.String()).Return(&store.PlaylistGroupRecord{
		ID:   gid,
		Slug: "group-slug",
		Body: existingBody,
	}, nil)

	// New playlist to be added
	newPlID := uuid.MustParse("13131313-1313-1313-1313-131313131313")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "new-pl").Return(&store.PlaylistRecord{
		ID: newPlID, Slug: "new-pl", Body: mustDecodePlaylist(t, []byte(`{"id":"13131313-1313-1313-1313-131313131313"}`)),
	}, nil)

	signed := []byte(`{"updated":true}`)
	parsedGroup := mustDecodeGroup(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidatePlaylistGroup(signed).Return(&parsedGroup, nil),
	)
	mockStore.EXPECT().UpdatePlaylistGroup(gomock.Any(), gid.String(), gomock.Any()).Do(func(_ context.Context, _ string, in *store.PlaylistGroupInput) {
		if len(in.Playlists) != 1 || in.Playlists[0].ID != newPlID {
			t.Fatalf("expected new playlist, got: %+v", in.Playlists)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
	newPlaylists := []string{localPlaylistRef("new-pl")}
	req := &models.PlaylistGroupUpdateRequest{
		Playlists: newPlaylists,
	}
	out, err := e.UpdatePlaylistGroup(context.Background(), gid.String(), req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("expected non-nil output")
	}
}

func TestUpdatePlaylistGroup_notFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "missing").Return(nil, store.ErrNotFound)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, testPublicBase)
	newTitle := "New Title"
	_, err := e.UpdatePlaylistGroup(context.Background(), "missing", &models.PlaylistGroupUpdateRequest{Title: &newTitle})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestUpdatePlaylistGroup_playlistResolveFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)

	gid := uuid.MustParse("14141414-1414-1414-1414-141414141414")
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), gid.String()).Return(&store.PlaylistGroupRecord{
		ID:   gid,
		Slug: "g",
		Body: playlistgroup.Group{Title: "G", Playlists: []string{localPlaylistRef("old")}},
	}, nil)

	// Playlist resolution fails
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "nonexistent").Return(nil, store.ErrNotFound)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), false, nil, testPublicBase)
	newPlaylists := []string{localPlaylistRef("nonexistent")}
	_, err := e.UpdatePlaylistGroup(context.Background(), gid.String(), &models.PlaylistGroupUpdateRequest{Playlists: newPlaylists})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected not found from playlist resolution, got %v", err)
	}
}

// =============================================================================
// Channel Tests
// =============================================================================

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
	err := e.DeleteChannel(context.Background(), "c")
	if !executor.IsExtensionsDisabled(err) {
		t.Fatalf("got %v", err)
	}
}

func TestCreateChannel_whitespaceSlugFallsBackToAuto(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

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
		if in.Slug == "" || !strings.HasPrefix(in.Slug, "t-") {
			t.Fatalf("expected auto slug from title \"T\", got %q", in.Slug)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	_, err := e.CreateChannel(context.Background(), &models.ChannelCreateRequest{
		Title:     "T",
		Slug:      "   ",
		Playlists: []string{localPlaylistRef("p")},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreateChannel_unslugifiableClientSlugFallsBackToTitle(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	plID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "p").Return(&store.PlaylistRecord{
		ID: plID, Slug: "p", Body: mustDecodePlaylist(t, []byte(`{"id":"66666666-6666-6666-6666-666666666666"}`)),
	}, nil)

	signed := []byte(`{"kind":"signed-channel"}`)
	wantCh := mustDecodeChannel(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidateChannel(signed).Return(&wantCh, nil),
	)
	mockStore.EXPECT().CreateChannel(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.ChannelInput) {
		if in.Slug == "" || !strings.HasPrefix(in.Slug, "derive-me-") {
			t.Fatalf("expected auto slug from title, got %q", in.Slug)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	_, err := e.CreateChannel(context.Background(), &models.ChannelCreateRequest{
		Title:     "Derive me",
		Slug:      "@@@",
		Playlists: []string{localPlaylistRef("p")},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreateChannel_unslugifiableTitleUsesChannelPrefix(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	plID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "p").Return(&store.PlaylistRecord{
		ID: plID, Slug: "p", Body: mustDecodePlaylist(t, []byte(`{"id":"55555555-5555-5555-5555-555555555555"}`)),
	}, nil)

	signed := []byte(`{"kind":"signed-channel"}`)
	wantCh := mustDecodeChannel(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidateChannel(signed).Return(&wantCh, nil),
	)
	mockStore.EXPECT().CreateChannel(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.ChannelInput) {
		if in.Slug == "" || !strings.HasPrefix(in.Slug, "channel-") {
			t.Fatalf("expected channel- prefix slug, got %q", in.Slug)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	_, err := e.CreateChannel(context.Background(), &models.ChannelCreateRequest{
		Title:     "###",
		Playlists: []string{localPlaylistRef("p")},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCreateChannel_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

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
	mockStore.EXPECT().CreateChannel(gomock.Any(), gomock.Any()).Do(func(_ context.Context, in *store.ChannelInput) {
		if in.ID == uuid.Nil || in.Slug != "my-channel" {
			t.Fatalf("create expects id and slugified slug, id=%v slug=%q", in.ID, in.Slug)
		}
		if len(in.Playlists) != 1 || in.Playlists[0].ID != plID {
			t.Fatalf("playlists: %+v", in.Playlists)
		}
		if !reflect.DeepEqual(in.Body, wantCh) {
			t.Fatalf("body %+v", in.Body)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	out, err := e.CreateChannel(context.Background(), validChannelCreateReq("My Channel", localPlaylistRef("pl-ch")))
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, wantCh) {
		t.Fatal("response mismatch")
	}
}

func TestCreateChannel_storeError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
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

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	_, err := e.CreateChannel(context.Background(), validChannelCreateReq("slug", localPlaylistRef("p")))
	if err == nil || !strings.Contains(err.Error(), "store: db") {
		t.Fatalf("got %v", err)
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

func TestDeleteChannel(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().DeleteChannel(gomock.Any(), "cid").Return(nil)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), true, nil, "")
	if err := e.DeleteChannel(context.Background(), "cid"); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceChannel_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	cid := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	created := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	mockStore.EXPECT().GetChannel(gomock.Any(), "ch-slug").Return(&store.ChannelRecord{
		ID:   cid,
		Slug: "ch-slug",
		Body: channels.Channel{
			Created: created.UTC().Format(time.RFC3339Nano),
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
	mockStore.EXPECT().UpdateChannel(gomock.Any(), "ch-slug", gomock.Any()).Do(func(_ context.Context, _ string, in *store.ChannelInput) {
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

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	req := validChannelCreateReq("ignored-on-replace", localPlaylistRef("pl2"))
	req.Title = "New title"
	out, err := e.ReplaceChannel(context.Background(), "ch-slug", req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !reflect.DeepEqual(*out, parsedCh) {
		t.Fatal("out mismatch")
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
			Created: created.UTC().Format(time.RFC3339Nano),
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
	mockStore.EXPECT().UpdateChannel(gomock.Any(), "ch-slug", gomock.Any()).Return(nil)

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

func TestUpdateChannel_extensionsDisabled(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, testPublicBase)
	newTitle := "New"
	_, err := e.UpdateChannel(context.Background(), "ch", &models.ChannelUpdateRequest{Title: &newTitle})
	if !executor.IsExtensionsDisabled(err) {
		t.Fatalf("expected extensions disabled, got %v", err)
	}
}

func TestUpdateChannel_success_partialFields(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	cid := uuid.MustParse("15151515-1515-1515-1515-151515151515")
	created := time.Date(2022, 7, 20, 9, 15, 0, 0, time.UTC)
	existingBody := channels.Channel{
		Title:      "Old Channel",
		Slug:       "old-ch",
		Version:    "1.0.0",
		Created:    created.UTC().Format(time.RFC3339Nano),
		Playlists:  []string{localPlaylistRef("ch-pl1")},
		Summary:    "Old channel summary",
		CoverImage: "https://old.example/ch-cover.jpg",
	}
	mockStore.EXPECT().GetChannel(gomock.Any(), "old-ch").Return(&store.ChannelRecord{
		ID:        cid,
		Slug:      "old-ch",
		Body:      existingBody,
		CreatedAt: created,
	}, nil)

	plID := uuid.MustParse("16161616-1616-1616-1616-161616161616")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "ch-pl1").Return(&store.PlaylistRecord{
		ID: plID, Slug: "ch-pl1", Body: mustDecodePlaylist(t, []byte(`{"id":"16161616-1616-1616-1616-161616161616"}`)),
	}, nil)

	signed := []byte(`{"title":"Updated Channel","slug":"old-ch","version":"1.0.0"}`)
	parsedCh := mustDecodeChannel(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidateChannel(signed).Return(&parsedCh, nil),
	)
	mockStore.EXPECT().UpdateChannel(gomock.Any(), "old-ch", gomock.Any()).Return(nil)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	newTitle := "Updated Channel"
	req := &models.ChannelUpdateRequest{
		Title: &newTitle,
	}
	out, err := e.UpdateChannel(context.Background(), "old-ch", req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("expected non-nil output")
	}
}

func TestUpdateChannel_success_updateMultipleFields(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	cid := uuid.MustParse("17171717-1717-1717-1717-171717171717")
	chCreated := time.Date(2021, 4, 4, 4, 0, 0, 0, time.UTC)
	existingBody := channels.Channel{
		Title:     "Old",
		Slug:      "ch",
		Version:   "1.0.0",
		Created:   chCreated.UTC().Format(time.RFC3339Nano),
		Playlists: []string{localPlaylistRef("old-pl")},
		Summary:   "Old summary",
	}
	mockStore.EXPECT().GetChannel(gomock.Any(), cid.String()).Return(&store.ChannelRecord{
		ID:   cid,
		Slug: "ch",
		Body: existingBody,
	}, nil)

	// New playlists
	newPlID1 := uuid.MustParse("18181818-1818-1818-1818-181818181818")
	newPlID2 := uuid.MustParse("19191919-1919-1919-1919-191919191919")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "new-pl1").Return(&store.PlaylistRecord{
		ID: newPlID1, Slug: "new-pl1", Body: mustDecodePlaylist(t, []byte(`{"id":"18181818-1818-1818-1818-181818181818"}`)),
	}, nil)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "new-pl2").Return(&store.PlaylistRecord{
		ID: newPlID2, Slug: "new-pl2", Body: mustDecodePlaylist(t, []byte(`{"id":"19191919-1919-1919-1919-191919191919"}`)),
	}, nil)

	signed := []byte(`{"channelUpdated":true}`)
	parsedCh := mustDecodeChannel(t, signed)
	gomock.InOrder(
		mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidateChannel(signed).Return(&parsedCh, nil),
	)
	mockStore.EXPECT().UpdateChannel(gomock.Any(), cid.String(), gomock.Any()).Do(func(_ context.Context, _ string, in *store.ChannelInput) {
		if len(in.Playlists) != 2 {
			t.Fatalf("expected 2 playlists, got: %+v", in.Playlists)
		}
		if in.Playlists[0].ID != newPlID1 || in.Playlists[1].ID != newPlID2 {
			t.Fatalf("playlist IDs mismatch: %+v", in.Playlists)
		}
	}).Return(nil)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	newTitle := "Updated Channel"
	newSummary := "Updated summary"
	newVersion := "2.0.0"
	newPlaylists := []string{localPlaylistRef("new-pl1"), localPlaylistRef("new-pl2")}
	req := &models.ChannelUpdateRequest{
		Title:     &newTitle,
		Summary:   &newSummary,
		Version:   &newVersion,
		Playlists: newPlaylists,
	}
	out, err := e.UpdateChannel(context.Background(), cid.String(), req)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil {
		t.Fatal("expected non-nil output")
	}
}

func TestUpdateChannel_notFound(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockStore.EXPECT().GetChannel(gomock.Any(), "missing").Return(nil, store.ErrNotFound)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), true, nil, testPublicBase)
	newTitle := "New"
	_, err := e.UpdateChannel(context.Background(), "missing", &models.ChannelUpdateRequest{Title: &newTitle})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestUpdateChannel_playlistResolveFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)

	cid := uuid.MustParse("20202020-2020-2020-2020-202020202020")
	mockStore.EXPECT().GetChannel(gomock.Any(), cid.String()).Return(&store.ChannelRecord{
		ID:   cid,
		Slug: "ch",
		Body: channels.Channel{Title: "Ch", Slug: "ch", Playlists: []string{localPlaylistRef("old")}},
	}, nil)

	// Playlist resolution fails
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "bad-pl").Return(nil, store.ErrNotFound)

	e := executor.New(mockStore, mocks.NewMockValidatorSigner(ctrl), true, nil, testPublicBase)
	newPlaylists := []string{localPlaylistRef("bad-pl")}
	_, err := e.UpdateChannel(context.Background(), cid.String(), &models.ChannelUpdateRequest{Playlists: newPlaylists})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected not found from playlist resolution, got %v", err)
	}
}

func TestUpdateChannel_validationFails(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	cid := uuid.MustParse("21212121-2121-2121-2121-212121212121")
	valCreated := time.Date(2020, 3, 3, 0, 0, 0, 0, time.UTC)
	mockStore.EXPECT().GetChannel(gomock.Any(), cid.String()).Return(&store.ChannelRecord{
		ID:   cid,
		Slug: "ch",
		Body: channels.Channel{
			Title:     "Ch",
			Slug:      "ch",
			Created:   valCreated.UTC().Format(time.RFC3339Nano),
			Playlists: []string{localPlaylistRef("pl")},
		},
	}, nil)

	plID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{
		ID: plID, Slug: "pl", Body: mustDecodePlaylist(t, []byte(`{"id":"22222222-2222-2222-2222-222222222222"}`)),
	}, nil)

	signed := []byte(`{"invalid":true}`)
	validationErr := errors.New("validation failed")
	gomock.InOrder(
		mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil),
		mockDP1.EXPECT().ValidateChannel(signed).Return(nil, validationErr),
	)

	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
	newTitle := "New"
	_, err := e.UpdateChannel(context.Background(), cid.String(), &models.ChannelUpdateRequest{Title: &newTitle})
	if err == nil || !strings.Contains(err.Error(), "validation") {
		t.Fatalf("expected validation error, got %v", err)
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
		Items:      []playlist.PlaylistItem{{Source: "https://example.com"}},
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
		Items:      []playlist.PlaylistItem{{Source: "https://example.com"}},
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
			Kind:        store.RegistryChannelKindStatic,
			Position:    0,
		},
		{
			ID:          uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
			PublisherID: pub2ID,
			ChannelURL:  "https://example.com/api/v1/channels/bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
			Kind:        store.RegistryChannelKindStatic,
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

func TestReplaceChannelRegistry_success(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	req := models.ChannelRegistry{
		Publishers: []models.ChannelRegistryPublisher{
			{
				Name: "Test Publisher",
				Static: []string{
					"https://example.com/api/v1/channels/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
					"https://example.com/api/v1/channels/bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
				},
				Living: []string{},
			},
			{
				Name: "Another Publisher",
				Static: []string{
					"https://example.com/api/v1/channels/cccccccc-cccc-cccc-cccc-cccccccccccc",
				},
				Living: []string{},
			},
		},
	}

	// Expect store to be called with 2 publishers and 3 channels
	mockStore.EXPECT().ReplaceChannelRegistry(
		gomock.Any(),
		gomock.AssignableToTypeOf([]store.RegistryPublisher{}),
		gomock.AssignableToTypeOf([]store.RegistryPublisherChannel{}),
	).DoAndReturn(func(ctx context.Context, pubs []store.RegistryPublisher, chans []store.RegistryPublisherChannel) error {
		// Verify we got 2 publishers
		if len(pubs) != 2 {
			t.Errorf("expected 2 publishers, got %d", len(pubs))
		}
		// Verify first publisher
		if pubs[0].Name != "Test Publisher" || pubs[0].Position != 0 {
			t.Errorf("publisher 0: want name='Test Publisher' pos=0, got name=%q pos=%d", pubs[0].Name, pubs[0].Position)
		}
		// Verify second publisher
		if pubs[1].Name != "Another Publisher" || pubs[1].Position != 1 {
			t.Errorf("publisher 1: want name='Another Publisher' pos=1, got name=%q pos=%d", pubs[1].Name, pubs[1].Position)
		}

		// Verify we got 3 channels total
		if len(chans) != 3 {
			t.Errorf("expected 3 channels, got %d", len(chans))
		}

		// Verify first publisher has 2 static channels
		pub1Channels := 0
		for _, ch := range chans {
			if ch.PublisherID == pubs[0].ID {
				pub1Channels++
				if ch.Kind != store.RegistryChannelKindStatic {
					t.Errorf("expected static kind for pub1 channel, got %q", ch.Kind)
				}
			}
		}
		if pub1Channels != 2 {
			t.Errorf("expected first publisher to have 2 channels, got %d", pub1Channels)
		}

		// Verify positions are set correctly
		for i, ch := range chans {
			if ch.Position < 0 {
				t.Errorf("channel %d has negative position: %d", i, ch.Position)
			}
		}

		return nil
	})

	e := executor.New(mockStore, mockDP1, false, nil, "")
	totalChannels, err := e.ReplaceChannelRegistry(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if totalChannels != 3 {
		t.Errorf("expected total channels 3, got %d", totalChannels)
	}
}

func TestReplaceChannelRegistry_emptyRequest(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	req := models.ChannelRegistry{}

	// Store should still be called even with empty registry (to clear DB) when invoked directly.
	mockStore.EXPECT().ReplaceChannelRegistry(
		gomock.Any(),
		gomock.AssignableToTypeOf([]store.RegistryPublisher{}),
		gomock.AssignableToTypeOf([]store.RegistryPublisherChannel{}),
	).DoAndReturn(func(ctx context.Context, pubs []store.RegistryPublisher, chans []store.RegistryPublisherChannel) error {
		if len(pubs) != 0 {
			t.Errorf("expected 0 publishers, got %d", len(pubs))
		}
		if len(chans) != 0 {
			t.Errorf("expected 0 channels, got %d", len(chans))
		}
		return nil
	})

	e := executor.New(mockStore, mockDP1, false, nil, "")
	totalChannels, err := e.ReplaceChannelRegistry(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if totalChannels != 0 {
		t.Errorf("expected total channels 0, got %d", totalChannels)
	}
}

func TestReplaceChannelRegistry_storeError(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	req := models.ChannelRegistry{
		Publishers: []models.ChannelRegistryPublisher{
			{
				Name:   "Test Publisher",
				Static: []string{"https://example.com/api/v1/channels/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"},
				Living: []string{},
			},
		},
	}

	storeErr := errors.New("constraint violation")
	mockStore.EXPECT().ReplaceChannelRegistry(
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
	).Return(storeErr)

	e := executor.New(mockStore, mockDP1, false, nil, "")
	_, err := e.ReplaceChannelRegistry(context.Background(), req)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Error should be wrapped
	if !strings.Contains(err.Error(), "replace channel registry") {
		t.Errorf("expected error to mention 'replace channel registry', got: %v", err)
	}
	if !errors.Is(err, storeErr) {
		t.Errorf("expected error to wrap store error")
	}
}

func TestReplaceChannelRegistry_positionAssignment(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)

	req := models.ChannelRegistry{
		Publishers: []models.ChannelRegistryPublisher{
			{
				Name: "First",
				Static: []string{
					"https://example.com/api/v1/channels/11111111-1111-1111-1111-111111111111",
					"https://example.com/api/v1/channels/22222222-2222-2222-2222-222222222222",
				},
				Living: []string{},
			},
			{
				Name: "Second",
				Static: []string{
					"https://example.com/api/v1/channels/33333333-3333-3333-3333-333333333333",
				},
				Living: []string{},
			},
		},
	}

	mockStore.EXPECT().ReplaceChannelRegistry(
		gomock.Any(),
		gomock.Any(),
		gomock.Any(),
	).DoAndReturn(func(ctx context.Context, pubs []store.RegistryPublisher, chans []store.RegistryPublisherChannel) error {
		// Verify publisher positions match array order
		if pubs[0].Position != 0 {
			t.Errorf("first publisher position: want 0, got %d", pubs[0].Position)
		}
		if pubs[1].Position != 1 {
			t.Errorf("second publisher position: want 1, got %d", pubs[1].Position)
		}

		// Verify channel positions within each publisher
		pub1ID := pubs[0].ID
		pub1Chans := []store.RegistryPublisherChannel{}
		pub2Chans := []store.RegistryPublisherChannel{}
		for _, ch := range chans {
			if ch.PublisherID == pub1ID {
				pub1Chans = append(pub1Chans, ch)
			} else {
				pub2Chans = append(pub2Chans, ch)
			}
		}

		// First publisher should have 2 channels at positions 0, 1
		if len(pub1Chans) != 2 {
			t.Errorf("first publisher channels: want 2, got %d", len(pub1Chans))
		}
		// Second publisher should have 1 channel at position 0
		if len(pub2Chans) != 1 {
			t.Errorf("second publisher channels: want 1, got %d", len(pub2Chans))
		}

		return nil
	})

	e := executor.New(mockStore, mockDP1, false, nil, "")
	_, err := e.ReplaceChannelRegistry(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
