package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStrongETagFromJSONBytes(t *testing.T) {
	b := []byte(`{"a":1}`)
	etag := strongETagFromJSONBytes(b)
	etag2 := strongETagFromJSONBytes(b)
	assert.Equal(t, etag, etag2)
	require.Len(t, etag, 2+64) // quotes + 64 hex chars
	assert.Equal(t, byte('"'), etag[0])
	assert.Equal(t, byte('"'), etag[len(etag)-1])
}

func TestStrongETagFromJSONBytes_KnownVector(t *testing.T) {
	b, err := json.Marshal(map[string]any{})
	require.NoError(t, err)
	assert.Equal(t, `{}`, string(b))
	got := strongETagFromJSONBytes(b)
	// echo -n '{}' | shasum -a 256
	const want = `"44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a"`
	assert.Equal(t, want, got)
}

func TestIfNoneMatchNotModified(t *testing.T) {
	etag := `"abc"`

	t.Run("empty header", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		assert.False(t, ifNoneMatchNotModified(r, etag))
	})

	t.Run("star", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("If-None-Match", "*")
		assert.False(t, ifNoneMatchNotModified(r, etag))
	})

	t.Run("exact match", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("If-None-Match", `"abc"`)
		assert.True(t, ifNoneMatchNotModified(r, etag))
	})

	t.Run("weak form matches strong tag value", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("If-None-Match", `W/"abc"`)
		assert.True(t, ifNoneMatchNotModified(r, etag))
	})

	t.Run("multiple candidates", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("If-None-Match", `"x", "abc"`)
		assert.True(t, ifNoneMatchNotModified(r, etag))
	})

	t.Run("no match", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("If-None-Match", `"other"`)
		assert.False(t, ifNoneMatchNotModified(r, etag))
	})
}

func TestWriteJSONIndividualGET_NotModified(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type doc struct {
		X int `json:"x"`
	}
	payload := doc{X: 42}
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	wantETag := strongETagFromJSONBytes(b)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodGet, "/r", nil)
	req.Header.Set("If-None-Match", wantETag)
	c.Request = req

	err = writeJSONIndividualGET(c, payload)
	require.NoError(t, err)
	assert.Equal(t, http.StatusNotModified, w.Code)
	assert.Empty(t, w.Body.Bytes())
	assert.Equal(t, wantETag, w.Header().Get("ETag"))
}
