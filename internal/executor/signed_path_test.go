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
