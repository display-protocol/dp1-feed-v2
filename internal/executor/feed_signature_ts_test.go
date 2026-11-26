package executor_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/mock/gomock"

	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/mocks"
)

// The feed signature's "ts" is when the feed signed (DP-1 §7.1), not the document's client-supplied
// "created". Both create fixtures carry a "created" far in the past, so passing it through would fail.
func TestCreate_feedSignatureTimestampIsSigningTime(t *testing.T) {
	t.Parallel()

	stop := errors.New("stop after signing")
	tests := []struct {
		name   string
		create func(t *testing.T, mockStore *mocks.MockStore, mockDP1 *mocks.MockValidatorSigner, sign func([]byte, time.Time) ([]byte, error)) error
	}{
		{
			name: "playlist",
			create: func(_ *testing.T, mockStore *mocks.MockStore, mockDP1 *mocks.MockValidatorSigner, sign func([]byte, time.Time) ([]byte, error)) error {
				mockDP1.EXPECT().VerifyPlaylistSignatures(gomock.Any()).Return(true, nil, nil)
				mockDP1.EXPECT().SignPlaylist(gomock.Any(), gomock.Any()).DoAndReturn(sign)
				req := validCreateReq()
				req.Raw = mustJSONRaw(req)
				_, err := executor.New(mockStore, mockDP1, false, nil, "").CreatePlaylist(context.Background(), req)
				return err
			},
		},
		{
			name: "channel",
			create: func(_ *testing.T, mockStore *mocks.MockStore, mockDP1 *mocks.MockValidatorSigner, sign func([]byte, time.Time) ([]byte, error)) error {
				mockDP1.EXPECT().VerifyChannelSignatures(gomock.Any()).Return(true, nil, nil)
				mockDP1.EXPECT().SignChannel(gomock.Any(), gomock.Any()).DoAndReturn(sign)
				req := validChannelCreateReq("chan")
				req.Raw = mustJSONRaw(req)
				_, err := executor.New(mockStore, mockDP1, true, nil, "").CreateChannel(context.Background(), req)
				return err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctrl := gomock.NewController(t)

			var got time.Time
			sign := func(_ []byte, ts time.Time) ([]byte, error) {
				got = ts
				return nil, stop
			}
			before := time.Now()
			err := tt.create(t, mocks.NewMockStore(ctrl), mocks.NewMockValidatorSigner(ctrl), sign)
			after := time.Now()

			if !errors.Is(err, stop) {
				t.Fatalf("want the signer's error, got %v", err)
			}
			if got.Before(before) || got.After(after) {
				t.Fatalf("feed signature ts = %s, want the signing time in [%s, %s]", got, before, after)
			}
		})
	}
}
