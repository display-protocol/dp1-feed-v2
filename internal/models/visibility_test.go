package models

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestVisibilityDecodesOnlyStrings(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    Visibility
		wantErr bool
	}{
		{name: "absent", body: `{}`, want: ""},
		{name: "unlisted", body: `{"visibility":"unlisted"}`, want: "unlisted"},
		{name: "public", body: `{"visibility":"public"}`, want: "public"},
		// The vocabulary is open: a value a later spec version defines is accepted, and the store keeps it
		// out of directories until it understands it.
		{name: "unknown value", body: `{"visibility":"followers"}`, want: "followers"},
		{name: "empty string", body: `{"visibility":""}`, want: ""},
		{name: "null", body: `{"visibility":null}`, wantErr: true},
		{name: "number", body: `{"visibility":1}`, wantErr: true},
		{name: "boolean", body: `{"visibility":false}`, wantErr: true},
		{name: "object", body: `{"visibility":{}}`, wantErr: true},
		{name: "array", body: `{"visibility":["unlisted"]}`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				Visibility Visibility `json:"visibility,omitempty"`
			}
			err := json.Unmarshal([]byte(tc.body), &got)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("decoded %q, want an error", got.Visibility)
				}
				if tc.name == "null" && !errors.Is(err, errVisibilityNotString) {
					t.Fatalf("null: want errVisibilityNotString, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Visibility != tc.want {
				t.Fatalf("got %q, want %q", got.Visibility, tc.want)
			}
		})
	}
}
