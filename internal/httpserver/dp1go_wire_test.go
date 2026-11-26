package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/display-protocol/dp1-go/playlist"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap/zaptest"

	"github.com/display-protocol/dp1-feed-v2/internal/mocks"
	"github.com/display-protocol/dp1-feed-v2/internal/models"
)

// The item endpoints are the one place the feed re-marshals a dp1-go type onto the wire (documents are
// served from stored bytes), so the spelling of provenance.contract.seriesId there is whatever the
// pinned dp1-go writes. Core §6: an integer when it fits exactly in a JSON number, else a decimal string.
// A dp1-go bump that changes this changes the observable API and the item's ETag.
func TestGetPlaylistItem_seriesIDWireForm(t *testing.T) {
	for _, tc := range []struct {
		name, seriesID, want string
	}{
		{"small id is an integer", "7", `"seriesId":7`},
		{"2^53-1 is an integer", "9007199254740991", `"seriesId":9007199254740991`},
		{"2^53 stays a string", "9007199254740992", `"seriesId":"9007199254740992"`},
		{"leading zero stays a string", "007", `"seriesId":"007"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			itemID := uuid.New()
			series := playlist.SeriesID(tc.seriesID)
			exec := mocks.NewMockExecutor(ctrl)
			exec.EXPECT().GetPlaylistItem(gomock.Any(), itemID).Return(&playlist.PlaylistItem{
				ID: itemID.String(),
				Provenance: &playlist.ProvenanceBlock{
					Type:     playlist.ProvenanceSeriesRegistry,
					Contract: &playlist.ProvenanceContract{SeriesID: &series},
				},
			}, nil)

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/playlist-items/"+itemID.String(), nil)
			c.Params = gin.Params{{Key: "id", Value: itemID.String()}}
			(&Handler{Exec: exec, Log: zaptest.NewLogger(t)}).GetPlaylistItem(c)

			require.Equal(t, http.StatusOK, w.Code)
			assert.Contains(t, w.Body.String(), tc.want)
		})
	}
}

// identity.Thumbnail has its own UnmarshalJSON (it accepts every spelling of an integer), and a custom
// unmarshaler escapes the decoder's DisallowUnknownFields. The strict-member guarantee inside a
// thumbnail therefore rests on checkExactMembers alone; this pins both halves.
func TestDecodeDocument_thumbnailIntegerSpellingsAndStrictness(t *testing.T) {
	body := func(thumbnail string) []byte {
		return []byte(`{"dpVersion":"1.1.0","title":"t","items":[],"curators":[{"name":"n","avatar":` + thumbnail + `}]}`)
	}

	for _, spelling := range []string{`100`, `100.0`, `1e2`} {
		var req models.PlaylistCreateRequest
		require.NoError(t, decodeDocument(body(`{"uri":"https://example.com/a.png","w":`+spelling+`}`), &req), spelling)
		require.Len(t, req.Curators, 1)
		require.NotNil(t, req.Curators[0].Avatar)
		require.NotNil(t, req.Curators[0].Avatar.W)
		assert.Equal(t, 100, *req.Curators[0].Avatar.W, spelling)
	}

	for name, thumbnail := range map[string]string{
		"unknown member":  `{"uri":"https://example.com/a.png","alt":"x"}`,
		"misspelled case": `{"uri":"https://example.com/a.png","W":100}`,
		"fraction":        `{"uri":"https://example.com/a.png","w":100.5}`,
	} {
		var req models.PlaylistCreateRequest
		assert.Error(t, decodeDocument(body(thumbnail), &req), name)
	}
}
