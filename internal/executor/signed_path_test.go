package executor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/extension/identity"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/playlistgroup"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/mocks"
	"github.com/display-protocol/dp1-feed-v2/internal/models"
	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// Error branches of the signed-document path that the real-Postgres round-trips in
// internal/httpserver do not reach. Every case must fail before any signing or storage call, which
// the mocks enforce by having no expectations.

const (
	signedTestKid  = "did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK"
	signedTestItem = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"
)

func signedPlaylistReq(mutate func(*models.PlaylistCreateRequest)) *models.PlaylistCreateRequest {
	id := uuid.New().String()
	created := time.Now().Add(-time.Minute).Format(time.RFC3339)
	req := &models.PlaylistCreateRequest{
		DPVersion:  "1.1.0",
		Title:      "t",
		Slug:       "t",
		Items:      []playlist.PlaylistItem{{ID: signedTestItem, Source: "https://x"}},
		Curators:   []identity.Entity{{Key: signedTestKid}},
		ID:         &id,
		Created:    &created,
		Signatures: []playlist.Signature{{Kid: signedTestKid, Alg: "ed25519", Sig: "s"}},
		Raw:        []byte(`{"title":"t"}`),
	}
	if mutate != nil {
		mutate(req)
	}
	return req
}

func TestCreateSignedPlaylist_identityErrors(t *testing.T) {
	t.Parallel()
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	cases := []struct {
		name   string
		mutate func(*models.PlaylistCreateRequest)
		want   error
	}{
		{"missing raw body is an internal error", func(r *models.PlaylistCreateRequest) { r.Raw = nil }, nil},
		{"malformed id", func(r *models.PlaylistCreateRequest) { bad := "not-a-uuid"; r.ID = &bad }, executor.ErrInvalidID},
		{"missing id", func(r *models.PlaylistCreateRequest) { r.ID = nil }, executor.ErrInvalidID},
		{"future created", func(r *models.PlaylistCreateRequest) { r.Created = &future }, executor.ErrInvalidTimestamp},
		{"missing created", func(r *models.PlaylistCreateRequest) { r.Created = nil }, executor.ErrInvalidTimestamp},
		{"blank slug", func(r *models.PlaylistCreateRequest) { r.Slug = "   " }, executor.ErrSlugRequired},
		{"item without id", func(r *models.PlaylistCreateRequest) { r.Items[0].ID = "" }, executor.ErrSignedItemIDRequired},
		{"item with non-uuid id", func(r *models.PlaylistCreateRequest) { r.Items[0].ID = "item-1" }, executor.ErrSignedItemIDRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			e := executor.New(mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), false, nil, "")
			_, err := e.CreatePlaylist(context.Background(), signedPlaylistReq(tc.mutate))
			if err == nil {
				t.Fatal("want error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if tc.want == nil && executor.IsSignedSubmissionError(err) {
				t.Fatalf("a missing raw body is a server defect, not a client error: %v", err)
			}
		})
	}
}

// A signed playlist whose signatures verify but none of which belongs to a listed curator is refused.
func TestCreateSignedPlaylist_noCuratorMatch(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil)
	e := executor.New(mocks.NewMockStore(ctrl), mockDP1, false, nil, "")
	req := signedPlaylistReq(func(r *models.PlaylistCreateRequest) { r.Curators = []identity.Entity{{Key: "did:key:someone-else"}} })
	_, err := e.CreatePlaylist(context.Background(), req)
	if !errors.Is(err, executor.ErrNoValidCuratorSignature) {
		t.Fatalf("want ErrNoValidCuratorSignature, got %v", err)
	}
}

func TestCreateSignedPlaylistGroup_verification(t *testing.T) {
	t.Parallel()
	id := uuid.New().String()
	created := time.Now().Add(-time.Minute).Format(time.RFC3339)
	member := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	memberRaw := []byte(`{"id":"22222222-2222-2222-2222-222222222222","slug":"pl","title":"P"}`)

	cases := []struct {
		name    string
		curator string
		verify  func(m *mocks.MockValidatorSigner)
		want    error
	}{
		{"crypto failure", signedTestKid, func(m *mocks.MockValidatorSigner) {
			m.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(false, []playlist.Signature{{Kid: signedTestKid}}, nil)
		}, executor.ErrSignatureVerificationFailed},
		{"verifier error", signedTestKid, func(m *mocks.MockValidatorSigner) {
			m.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(false, nil, errors.New("boom"))
		}, executor.ErrSignatureVerificationFailed},
		{"no curator match", "did:key:other", func(m *mocks.MockValidatorSigner) {
			m.EXPECT().VerifyPlaylistGroupSignatures(gomock.Any()).Return(true, nil, nil)
		}, executor.ErrNoValidCuratorSignature},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			mockStore := mocks.NewMockStore(ctrl)
			mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{ID: member, Slug: "pl", Raw: memberRaw}, nil)
			mockDP1 := mocks.NewMockValidatorSigner(ctrl)
			tc.verify(mockDP1)
			e := executor.New(mockStore, mockDP1, false, nil, testPublicBase)
			req := &models.PlaylistGroupCreateRequest{
				Title: "g", Slug: "g", Playlists: []string{localPlaylistRef("pl")}, Curator: tc.curator,
				ID: &id, Created: &created,
				Signatures: []playlist.Signature{{Kid: signedTestKid, Alg: "ed25519", Sig: "s"}},
				Raw:        []byte(`{"title":"g"}`),
			}
			_, err := e.CreatePlaylistGroup(context.Background(), req)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func TestCreateSignedChannel_verification(t *testing.T) {
	t.Parallel()
	id := uuid.New().String()
	created := time.Now().Add(-time.Minute).Format(time.RFC3339)
	member := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	memberRaw := []byte(`{"id":"33333333-3333-3333-3333-333333333333","slug":"pl","title":"P"}`)

	cases := []struct {
		name      string
		publisher *identity.Entity
		verify    func(m *mocks.MockValidatorSigner)
		want      error
	}{
		{"crypto failure", &identity.Entity{Key: signedTestKid}, func(m *mocks.MockValidatorSigner) {
			m.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(false, []playlist.Signature{{Kid: signedTestKid}}, nil)
		}, executor.ErrSignatureVerificationFailed},
		{"no publisher match", &identity.Entity{Key: "did:key:other"}, func(m *mocks.MockValidatorSigner) {
			m.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil)
		}, executor.ErrNoValidPublisherSignature},
		{"no publisher at all", nil, func(m *mocks.MockValidatorSigner) {
			m.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil)
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)
			mockStore := mocks.NewMockStore(ctrl)
			mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{ID: member, Slug: "pl", Raw: memberRaw}, nil)
			mockDP1 := mocks.NewMockValidatorSigner(ctrl)
			tc.verify(mockDP1)
			e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)
			req := &models.ChannelCreateRequest{
				Title: "c", Slug: "c", Playlists: []string{localPlaylistRef("pl")}, Publisher: tc.publisher,
				ID: &id, Created: &created,
				Signatures: []playlist.Signature{{Kid: signedTestKid, Alg: "ed25519", Sig: "s"}},
				Raw:        []byte(`{"title":"c"}`),
			}
			_, err := e.CreateChannel(context.Background(), req)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

func playlistgroupWithCreated(created time.Time) playlistgroup.Group {
	return playlistgroup.Group{Created: created.UTC().Format(time.RFC3339Nano)}
}

func channelWithCreated(created time.Time) channels.Channel {
	return channels.Channel{Created: created.UTC().Format(time.RFC3339Nano)}
}

// Replace on the signed path applies the same identity rules for groups and channels as for playlists.
func TestReplaceSignedGroupAndChannel_identityMismatch(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	rowID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	created := time.Date(2020, 5, 15, 10, 30, 0, 0, time.UTC)
	member := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	memberRaw := []byte(`{"id":"55555555-5555-5555-5555-555555555555"}`)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{ID: member, Slug: "pl", Raw: memberRaw}, nil).AnyTimes()
	mockStore.EXPECT().GetPlaylistGroup(gomock.Any(), "g").Return(&store.PlaylistGroupRecord{ID: rowID, Slug: "g", Body: playlistgroupWithCreated(created)}, nil)
	mockStore.EXPECT().GetChannel(gomock.Any(), "c").Return(&store.ChannelRecord{ID: rowID, Slug: "c", Body: channelWithCreated(created)}, nil)
	e := executor.New(mockStore, mockDP1, true, nil, testPublicBase)

	other := uuid.New().String()
	createdStr := created.Format(time.RFC3339)
	sigs := []playlist.Signature{{Kid: signedTestKid, Alg: "ed25519", Sig: "s"}}

	_, err := e.ReplacePlaylistGroup(context.Background(), "g", &models.PlaylistGroupReplaceRequest{
		Title: "g", Slug: "g", Playlists: []string{localPlaylistRef("pl")}, ID: &other, Created: &createdStr, Signatures: sigs, Raw: []byte(`{}`),
	})
	if !errors.Is(err, executor.ErrSignedDocumentMismatch) {
		t.Fatalf("group: want ErrSignedDocumentMismatch, got %v", err)
	}
	_, err = e.ReplaceChannel(context.Background(), "c", &models.ChannelReplaceRequest{
		Title: "c", Slug: "other", Playlists: []string{localPlaylistRef("pl")}, ID: stringPtr(rowID.String()), Created: &createdStr, Signatures: sigs, Raw: []byte(`{}`),
	})
	if !errors.Is(err, executor.ErrSignedDocumentMismatch) {
		t.Fatalf("channel: want ErrSignedDocumentMismatch, got %v", err)
	}
}

// A stored document carrying a legacy v1.0.x top-level "signature" is foreign by definition: the feed
// never produces one and the API-key builders would drop it. PATCH and API-key PUT must refuse it.
func TestUpdatePlaylist_legacySignature_isImmutable(t *testing.T) {
	t.Parallel()
	ctrl := gomock.NewController(t)
	mockStore := mocks.NewMockStore(ctrl)
	mockDP1 := mocks.NewMockValidatorSigner(ctrl)
	mockStore.EXPECT().GetPlaylist(gomock.Any(), "legacy").Return(&store.PlaylistRecord{
		ID: uuid.New(), Slug: "legacy",
		Body: playlist.Playlist{Created: "2020-01-01T00:00:00Z", Signature: "ed25519:abcd"},
	}, nil).Times(2)
	e := executor.New(mockStore, mockDP1, false, nil, "")
	title := "x"
	if _, err := e.UpdatePlaylist(context.Background(), "legacy", &models.PlaylistUpdateRequest{Title: &title}); !errors.Is(err, executor.ErrDocumentImmutable) {
		t.Fatalf("PATCH: want ErrDocumentImmutable, got %v", err)
	}
	if _, err := e.ReplacePlaylist(context.Background(), "legacy", &models.PlaylistReplaceRequest{DPVersion: "1.1.0", Title: "x", Items: []playlist.PlaylistItem{{Source: "https://x"}}}); !errors.Is(err, executor.ErrDocumentImmutable) {
		t.Fatalf("PUT: want ErrDocumentImmutable, got %v", err)
	}
}

// Slug-targeted writes must persist by the ID resolved from the read, never by re-resolving the slug:
// with slug moves persisted, a concurrent move-and-reuse could otherwise redirect the write — built
// and authorized for the row that was read — onto a different row.
func TestSlugTargetedWrites_useResolvedID(t *testing.T) {
	t.Parallel()
	rowID := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	created := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	title := "x"

	t.Run("playlist", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		st, m := mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl)
		st.EXPECT().GetPlaylist(gomock.Any(), "moving").Return(&store.PlaylistRecord{ID: rowID, Slug: "moving", Body: playlist.Playlist{Created: created}}, nil)
		signed := []byte(`{"title":"x"}`)
		m.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil)
		m.EXPECT().ValidatePlaylist(signed).Return(&playlist.Playlist{Title: "x"}, nil)
		st.EXPECT().UpdatePlaylist(gomock.Any(), rowID.String(), gomock.Any(), gomock.Any()).Return(nil, nil)
		e := executor.New(st, m, false, nil, "")
		if _, err := e.UpdatePlaylist(context.Background(), "moving", &models.PlaylistUpdateRequest{Title: &title}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("playlist-group", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		st, m := mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl)
		member := uuid.MustParse("aaaaaaaa-1111-1111-1111-111111111111")
		st.EXPECT().GetPlaylistGroup(gomock.Any(), "moving").Return(&store.PlaylistGroupRecord{ID: rowID, Slug: "moving", Body: playlistgroup.Group{Created: created, Playlists: []string{localPlaylistRef("pl")}}}, nil)
		st.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{ID: member, Slug: "pl", Raw: []byte(`{"id":"aaaaaaaa-1111-1111-1111-111111111111"}`)}, nil)
		signed := []byte(`{"title":"x"}`)
		m.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil)
		m.EXPECT().ValidatePlaylistGroup(signed).Return(&playlistgroup.Group{Title: "x"}, nil)
		st.EXPECT().UpdatePlaylistGroup(gomock.Any(), rowID.String(), gomock.Any(), gomock.Any()).Return(nil, nil)
		e := executor.New(st, m, false, nil, testPublicBase)
		if _, err := e.UpdatePlaylistGroup(context.Background(), "moving", &models.PlaylistGroupUpdateRequest{Title: &title}); err != nil {
			t.Fatal(err)
		}
	})
}

// The immutability guard is atomic with the write: the executor passes the updated_at it read as an
// optimistic-concurrency token, so a signed PUT that commits between the API-key read and its write
// makes the store reject the stale write (ErrConcurrentModification → HTTP 409) rather than clobbering
// the now-foreign document. This pins the token threading and the conflict propagation; the store test
// TestStore_updateIsConditionalOnUpdatedAt proves the SQL condition itself.
func TestUpdate_passesReadUpdatedAtAndSurfacesConflict(t *testing.T) {
	t.Parallel()
	readAt := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	const feedKid = "did:key:feed"

	t.Run("playlist PATCH", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		st, m := mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl)
		st.EXPECT().GetPlaylist(gomock.Any(), "p").Return(&store.PlaylistRecord{
			ID: uuid.New(), Slug: "p", Body: playlist.Playlist{Created: "2020-01-01T00:00:00Z"}, UpdatedAt: readAt,
		}, nil)
		m.EXPECT().Kid().Return(feedKid).AnyTimes()
		signed := []byte(`{"title":"x"}`)
		m.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil)
		m.EXPECT().ValidatePlaylist(signed).Return(&playlist.Playlist{Title: "x"}, nil)
		// The store must receive exactly the updated_at that was read, and its conflict must propagate.
		st.EXPECT().UpdatePlaylist(gomock.Any(), gomock.Any(), gomock.Any(), readAt).Return(nil, store.ErrConcurrentModification)
		e := executor.New(st, m, false, nil, "")
		title := "x"
		_, err := e.UpdatePlaylist(context.Background(), "p", &models.PlaylistUpdateRequest{Title: &title})
		if !errors.Is(err, store.ErrConcurrentModification) {
			t.Fatalf("want ErrConcurrentModification, got %v", err)
		}
	})

	t.Run("channel replace", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		st, m := mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl)
		member := uuid.MustParse("bbbbbbbb-1111-1111-1111-111111111111")
		st.EXPECT().GetChannel(gomock.Any(), "c").Return(&store.ChannelRecord{
			ID: uuid.New(), Slug: "c", Body: channels.Channel{Created: "2020-01-01T00:00:00Z", Playlists: []string{localPlaylistRef("pl")}}, UpdatedAt: readAt,
		}, nil)
		st.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{ID: member, Slug: "pl", Raw: []byte(`{"id":"bbbbbbbb-1111-1111-1111-111111111111"}`)}, nil)
		m.EXPECT().Kid().Return(feedKid).AnyTimes()
		signed := []byte(`{"title":"x"}`)
		m.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil)
		m.EXPECT().ValidateChannel(signed).Return(&channels.Channel{Title: "x"}, nil)
		st.EXPECT().UpdateChannel(gomock.Any(), gomock.Any(), gomock.Any(), readAt).Return(nil, store.ErrConcurrentModification)
		e := executor.New(st, m, true, nil, testPublicBase)
		_, err := e.ReplaceChannel(context.Background(), "c", &models.ChannelReplaceRequest{Title: "x", Playlists: []string{localPlaylistRef("pl")}})
		if !errors.Is(err, store.ErrConcurrentModification) {
			t.Fatalf("want ErrConcurrentModification, got %v", err)
		}
	})
}
