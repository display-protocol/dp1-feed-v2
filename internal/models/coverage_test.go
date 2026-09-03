package models

import (
	"reflect"
	"strings"
	"testing"

	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/playlistgroup"
)

// Request bodies are decoded with unknown fields rejected (internal/httpserver/bind.go). That makes the
// request models the effective schema for signed submissions, so they must describe every member the
// dp1-go document structs do — otherwise a dp1-go upgrade that adds a member turns into a 400 for every
// client that sends it. This test fails the build the moment a document struct gains a JSON member the
// request model lacks.
func TestCreateRequestsCoverDocumentStructs(t *testing.T) {
	cases := []struct {
		name string
		req  any
		doc  any
	}{
		{"playlist", PlaylistCreateRequest{}, playlist.Playlist{}},
		{"playlist-group", PlaylistGroupCreateRequest{}, playlistgroup.Group{}},
		{"channel", ChannelCreateRequest{}, channels.Channel{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			have := jsonMembers(reflect.TypeOf(tc.req))
			for member := range jsonMembers(reflect.TypeOf(tc.doc)) {
				if !have[member] {
					t.Errorf("%T lacks JSON member %q declared by %T; signed documents carrying it would be rejected as unknown", tc.req, member, tc.doc)
				}
			}
		})
	}
}

// jsonMembers returns the wire names a struct decodes, ignoring fields tagged "-".
func jsonMembers(typ reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = true
	}
	return out
}
