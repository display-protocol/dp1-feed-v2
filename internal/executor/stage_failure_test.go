package executor_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	dp1 "github.com/display-protocol/dp1-go"
	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/playlistgroup"
	"github.com/google/uuid"
	"go.uber.org/mock/gomock"

	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/mocks"
	"github.com/display-protocol/dp1-feed-v2/internal/models"
	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// Each write path has three stages after the document is ready — feed sign, validate, persist — and
// each failure must surface with its stage named and the underlying error preserved, because the HTTP
// layer maps dp1-go validation errors to 400 and everything else to 500. These tests pin that for
// every document kind on the API-key path; the signed paths share the same signAndValidate* helpers.

type stage int

const (
	stageSign stage = iota
	stageValidate
	stageStore
)

var errStage = errors.New("stage failed")

func expectPlaylistStages(m *mocks.MockValidatorSigner, st *mocks.MockStore, failAt stage) {
	signed := []byte(`{"dpVersion":"1.1.0","title":"t","items":[]}`)
	if failAt == stageSign {
		m.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(nil, errStage)
		return
	}
	m.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil)
	if failAt == stageValidate {
		m.EXPECT().ValidatePlaylist(signed).Return(nil, dp1.ErrValidation)
		return
	}
	m.EXPECT().ValidatePlaylist(signed).Return(&playlist.Playlist{Title: "t"}, nil)
	st.EXPECT().CreatePlaylist(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Return(errStage)
}

func expectGroupStages(m *mocks.MockValidatorSigner, st *mocks.MockStore, failAt stage) {
	signed := []byte(`{"title":"g"}`)
	if failAt == stageSign {
		m.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(nil, errStage)
		return
	}
	m.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil)
	if failAt == stageValidate {
		m.EXPECT().ValidatePlaylistGroup(signed).Return(nil, dp1.ErrValidation)
		return
	}
	m.EXPECT().ValidatePlaylistGroup(signed).Return(&playlistgroup.Group{Title: "g"}, nil)
	st.EXPECT().CreatePlaylistGroup(gomock.Any(), gomock.Any()).Return(errStage)
}

func expectChannelStages(m *mocks.MockValidatorSigner, st *mocks.MockStore, failAt stage) {
	signed := []byte(`{"title":"c"}`)
	if failAt == stageSign {
		m.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(nil, errStage)
		return
	}
	m.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil)
	if failAt == stageValidate {
		m.EXPECT().ValidateChannel(signed).Return(nil, dp1.ErrValidation)
		return
	}
	m.EXPECT().ValidateChannel(signed).Return(&channels.Channel{Title: "c"}, nil)
	st.EXPECT().CreateChannel(gomock.Any(), gomock.Any()).Return(errStage)
}

func TestCreate_stageFailures(t *testing.T) {
	t.Parallel()
	member := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	memberRaw := []byte(`{"id":"66666666-6666-6666-6666-666666666666"}`)

	stages := []struct {
		name   string
		at     stage
		prefix string
		is     error
	}{
		{"sign", stageSign, "feed sign:", errStage},
		{"validate", stageValidate, "post-sign validation:", dp1.ErrValidation},
		{"store", stageStore, "store:", errStage},
	}
	kinds := []struct {
		name string
		run  func(t *testing.T, at stage) error
	}{
		{"playlist", func(t *testing.T, at stage) error {
			ctrl := gomock.NewController(t)
			st, m := mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl)
			expectPlaylistStages(m, st, at)
			e := executor.New(st, m, false, nil, "")
			_, err := e.CreatePlaylist(context.Background(), &models.PlaylistCreateRequest{DPVersion: "1.1.0", Title: "t", Items: []playlist.PlaylistItem{{Source: "https://x"}}})
			return err
		}},
		{"playlist-group", func(t *testing.T, at stage) error {
			ctrl := gomock.NewController(t)
			st, m := mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl)
			st.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{ID: member, Slug: "pl", Raw: memberRaw}, nil)
			expectGroupStages(m, st, at)
			e := executor.New(st, m, false, nil, testPublicBase)
			_, err := e.CreatePlaylistGroup(context.Background(), &models.PlaylistGroupCreateRequest{Title: "g", Playlists: []string{localPlaylistRef("pl")}})
			return err
		}},
		{"channel", func(t *testing.T, at stage) error {
			ctrl := gomock.NewController(t)
			st, m := mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl)
			st.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{ID: member, Slug: "pl", Raw: memberRaw}, nil)
			expectChannelStages(m, st, at)
			e := executor.New(st, m, true, nil, testPublicBase)
			_, err := e.CreateChannel(context.Background(), &models.ChannelCreateRequest{Title: "c", Playlists: []string{localPlaylistRef("pl")}})
			return err
		}},
	}
	for _, k := range kinds {
		for _, s := range stages {
			t.Run(k.name+"/"+s.name, func(t *testing.T) {
				t.Parallel()
				err := k.run(t, s.at)
				if err == nil || !strings.HasPrefix(err.Error(), s.prefix) || !errors.Is(err, s.is) {
					t.Fatalf("want %q wrapping %v, got %v", s.prefix, s.is, err)
				}
				if s.at == stageValidate && !executor.IsDP1ValidationError(err) {
					t.Fatal("validation failure must map to a DP-1 validation error (HTTP 400)")
				}
			})
		}
	}
}

// Replace and Update on the API-key path go through the same stages; pin the persist failure and the
// stored-document parse failure (a corrupt "created" in storage) for each kind.
func TestReplaceUpdate_storeAndParseFailures(t *testing.T) {
	t.Parallel()
	const feedKid = "did:key:feed"
	created := time.Date(2020, 5, 15, 10, 30, 0, 0, time.UTC).Format(time.RFC3339Nano)
	rowID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	member := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	memberRaw := []byte(`{"id":"88888888-8888-8888-8888-888888888888"}`)
	title := "x"

	t.Run("playlist replace store failure", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		st, m := mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl)
		st.EXPECT().GetPlaylist(gomock.Any(), "p").Return(&store.PlaylistRecord{ID: rowID, Slug: "p", Body: playlist.Playlist{Created: created}}, nil)
		signed := []byte(`{"title":"x"}`)
		m.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).Return(signed, nil)
		m.EXPECT().ValidatePlaylist(signed).Return(&playlist.Playlist{Title: "x"}, nil)
		st.EXPECT().UpdatePlaylist(gomock.Any(), "p", gomock.Any()).Return(errStage)
		e := executor.New(st, m, false, nil, "")
		_, err := e.ReplacePlaylist(context.Background(), "p", &models.PlaylistReplaceRequest{DPVersion: "1.1.0", Title: "x", Items: []playlist.PlaylistItem{{Source: "https://x"}}})
		if !errors.Is(err, errStage) {
			t.Fatalf("want store error, got %v", err)
		}
	})

	t.Run("playlist update corrupt stored created", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		st, m := mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl)
		st.EXPECT().GetPlaylist(gomock.Any(), "p").Return(&store.PlaylistRecord{ID: rowID, Slug: "p", Body: playlist.Playlist{Created: "yesterday"}}, nil)
		e := executor.New(st, m, false, nil, "")
		_, err := e.UpdatePlaylist(context.Background(), "p", &models.PlaylistUpdateRequest{Title: &title})
		if err == nil || !strings.Contains(err.Error(), "parse document created") {
			t.Fatalf("want parse error, got %v", err)
		}
	})

	t.Run("group update store failure with feed-only signatures", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		st, m := mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl)
		st.EXPECT().GetPlaylistGroup(gomock.Any(), "g").Return(&store.PlaylistGroupRecord{ID: rowID, Slug: "g", Body: playlistgroup.Group{
			Created: created, Playlists: []string{localPlaylistRef("pl")},
			Signatures: []playlist.Signature{{Kid: feedKid, Role: "feed"}},
		}}, nil)
		m.EXPECT().Kid().Return(feedKid).AnyTimes()
		st.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{ID: member, Slug: "pl", Raw: memberRaw}, nil)
		signed := []byte(`{"title":"x"}`)
		m.EXPECT().SignPlaylistGroup(gomock.Any(), gomock.Any()).Return(signed, nil)
		m.EXPECT().ValidatePlaylistGroup(signed).Return(&playlistgroup.Group{Title: "x"}, nil)
		st.EXPECT().UpdatePlaylistGroup(gomock.Any(), "g", gomock.Any()).Return(errStage)
		e := executor.New(st, m, false, nil, testPublicBase)
		_, err := e.UpdatePlaylistGroup(context.Background(), "g", &models.PlaylistGroupUpdateRequest{Title: &title})
		if !errors.Is(err, errStage) {
			t.Fatalf("want store error, got %v", err)
		}
	})

	t.Run("group replace refused for foreign signatures", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		st, m := mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl)
		st.EXPECT().GetPlaylistGroup(gomock.Any(), "g").Return(&store.PlaylistGroupRecord{ID: rowID, Slug: "g", Body: playlistgroup.Group{
			Created: created, Signatures: []playlist.Signature{{Kid: "did:key:curator", Role: "curator"}},
		}}, nil)
		m.EXPECT().Kid().Return(feedKid).AnyTimes()
		st.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{ID: member, Slug: "pl", Raw: memberRaw}, nil)
		e := executor.New(st, m, false, nil, testPublicBase)
		_, err := e.ReplacePlaylistGroup(context.Background(), "g", &models.PlaylistGroupReplaceRequest{Title: "x", Playlists: []string{localPlaylistRef("pl")}})
		if !errors.Is(err, executor.ErrDocumentImmutable) {
			t.Fatalf("want ErrDocumentImmutable, got %v", err)
		}
	})

	t.Run("channel update store failure", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		st, m := mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl)
		st.EXPECT().GetChannel(gomock.Any(), "c").Return(&store.ChannelRecord{ID: rowID, Slug: "c", Body: channels.Channel{
			Created: created, Playlists: []string{localPlaylistRef("pl")},
		}}, nil)
		st.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{ID: member, Slug: "pl", Raw: memberRaw}, nil)
		signed := []byte(`{"title":"x"}`)
		m.EXPECT().SignChannel(gomock.Any(), gomock.Any()).Return(signed, nil)
		m.EXPECT().ValidateChannel(signed).Return(&channels.Channel{Title: "x"}, nil)
		st.EXPECT().UpdateChannel(gomock.Any(), rowID.String(), gomock.Any()).Return(errStage)
		e := executor.New(st, m, true, nil, testPublicBase)
		_, err := e.UpdateChannel(context.Background(), "c", &models.ChannelUpdateRequest{Title: &title})
		if !errors.Is(err, errStage) {
			t.Fatalf("want store error, got %v", err)
		}
	})

	t.Run("channel replace refused for foreign signatures", func(t *testing.T) {
		t.Parallel()
		ctrl := gomock.NewController(t)
		st, m := mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl)
		st.EXPECT().GetChannel(gomock.Any(), "c").Return(&store.ChannelRecord{ID: rowID, Slug: "c", Body: channels.Channel{
			Created: created, Signatures: []playlist.Signature{{Kid: "did:key:publisher", Role: "publisher"}},
		}}, nil)
		m.EXPECT().Kid().Return(feedKid).AnyTimes()
		st.EXPECT().GetPlaylist(gomock.Any(), "pl").Return(&store.PlaylistRecord{ID: member, Slug: "pl", Raw: memberRaw}, nil)
		e := executor.New(st, m, true, nil, testPublicBase)
		_, err := e.ReplaceChannel(context.Background(), "c", &models.ChannelReplaceRequest{Title: "x", Playlists: []string{localPlaylistRef("pl")}})
		if !errors.Is(err, executor.ErrDocumentImmutable) {
			t.Fatalf("want ErrDocumentImmutable, got %v", err)
		}
	})
}
