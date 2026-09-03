package httpserver

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/display-protocol/dp1-feed-v2/internal/models"
)

func bindTestContext(body string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return c
}

func TestBindDocument(t *testing.T) {
	const valid = `{"dpVersion":"1.1.0","title":"t","items":[{"source":"https://x"}]}`

	t.Run("returns the exact bytes received", func(t *testing.T) {
		var req models.PlaylistCreateRequest
		raw, err := bindDocument(bindTestContext(valid), &req)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != valid || req.Title != "t" {
			t.Fatalf("raw=%s title=%q", raw, req.Title)
		}
	})

	t.Run("rejects an unknown member by name", func(t *testing.T) {
		var req models.PlaylistCreateRequest
		_, err := bindDocument(bindTestContext(`{"dpVersion":"1.1.0","title":"t","items":[{"source":"https://x","created":"2026-01-01T00:00:00Z"}]}`), &req)
		if err == nil || err.Error() != `json: unknown field "created"` {
			t.Fatalf("want unknown-field error naming the member, got %v", err)
		}
	})

	t.Run("rejects case variants of known members", func(t *testing.T) {
		// encoding/json would bind "Summary" to summary silently; the contract says exact names only.
		cases := map[string]string{
			"top level": `{"dpVersion":"1.1.0","title":"t","Summary":"s","items":[{"source":"https://x"}]}`,
			"nested":    `{"dpVersion":"1.1.0","title":"t","items":[{"Source":"https://x"}]}`,
			"deep":      `{"dpVersion":"1.1.0","title":"t","items":[{"source":"https://x","display":{"Scaling":"fit"}}]}`,
		}
		want := map[string]string{"top level": "Summary", "nested": "Source", "deep": "Scaling"}
		for name, body := range cases {
			var req models.PlaylistCreateRequest
			_, err := bindDocument(bindTestContext(body), &req)
			if err == nil || err.Error() != `json: unknown field "`+want[name]+`"` {
				t.Fatalf("%s: want unknown field %q, got %v", name, want[name], err)
			}
		}
	})

	t.Run("leaves opaque members alone", func(t *testing.T) {
		// override and inlineManifest are json.RawMessage: their contents are the client's, not modeled.
		var req models.PlaylistCreateRequest
		_, err := bindDocument(bindTestContext(`{"dpVersion":"1.1.0","title":"t","items":[{"source":"https://x","override":{"AnyThing":1},"display":{"userOverrides":{"Scaling":true}}}]}`), &req)
		if err != nil {
			t.Fatalf("opaque members must not be checked: %v", err)
		}
	})

	t.Run("rejects trailing bytes after the document", func(t *testing.T) {
		// gin's decoder stops after the first value; without this guard the remainder would reach
		// the signer and surface as a 500 instead of a 400.
		var req models.PlaylistCreateRequest
		_, err := bindDocument(bindTestContext(valid+`{"garbage":true}`), &req)
		if !errors.Is(err, errTrailingBody) {
			t.Fatalf("want errTrailingBody, got %v", err)
		}
	})

	t.Run("still enforces required fields", func(t *testing.T) {
		var req models.PlaylistCreateRequest
		_, err := bindDocument(bindTestContext(`{"dpVersion":"1.1.0"}`), &req)
		if err == nil {
			t.Fatal("want required-field error")
		}
	})
}
