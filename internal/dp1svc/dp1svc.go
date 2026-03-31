// Package dp1svc wraps dp1-go validation and Ed25519 multi-signature signing for feed operator documents (DP-1 v1.1.0+).
package dp1svc

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	dp1 "github.com/display-protocol/dp1-go"
	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/playlistgroup"
	"github.com/display-protocol/dp1-go/sign"
)

// ValidatorSigner validates playlist JSON and signs with v1.1+ multi-signature (feed role).
// Implemented by *Service so executors can depend on an interface for tests and DI.
//
// Gomock: generated type mocks.MockValidatorSigner in internal/mocks/dp1svc_mock.go.
// Regenerate all mocks from repository root: go generate ./...
// (directives in internal/mocks/doc.go; uses go tool mockgen from go.mod tools.)
type ValidatorSigner interface {
	// ValidatePlaylist validates against the core playlist JSON Schema and returns the parsed document (dp1-go ParseAndValidatePlaylist).
	ValidatePlaylist(raw []byte) (*playlist.Playlist, error)
	// ValidatePlaylistWithExtension validates core plus the playlists registry extension overlay and returns the parsed document.
	ValidatePlaylistWithExtension(raw []byte) (*playlist.Playlist, error)
	// SignPlaylist attaches a signatures[] entry (Ed25519, feed role) and strips legacy single-signature fields.
	SignPlaylist(raw []byte, ts time.Time) ([]byte, error)

	// ValidatePlaylistGroup validates a signed playlist-group document and returns the parsed document (dp1-go ParseAndValidatePlaylistGroup).
	ValidatePlaylistGroup(raw []byte) (*playlistgroup.Group, error)
	// SignPlaylistGroup signs with feed role (DP-1 playlist-group schema).
	SignPlaylistGroup(raw []byte, ts time.Time) ([]byte, error)

	// ValidateChannel validates a signed channels extension document and returns the parsed document (dp1-go ParseAndValidateChannel).
	ValidateChannel(raw []byte) (*channels.Channel, error)
	// SignChannel signs with feed role (channels extension schema).
	SignChannel(raw []byte, ts time.Time) ([]byte, error)
}

// Service holds the operator Ed25519 key and did:key kid used in v1.1+ multi-signature entries.
type Service struct {
	priv ed25519.PrivateKey
	kid  string
}

// Ed25519PrivateKeyFromHex parses a 64-byte Ed25519 seed (128 hex chars) or full private key (64 bytes = 128 hex).
func Ed25519PrivateKeyFromHex(signingKeyHex string) (ed25519.PrivateKey, error) {
	if signingKeyHex == "" {
		return nil, fmt.Errorf("signing key is required")
	}
	raw, err := hex.DecodeString(signingKeyHex)
	if err != nil {
		return nil, fmt.Errorf("signing key hex: %w", err)
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	default:
		return nil, fmt.Errorf("signing key must be %d or %d bytes hex", ed25519.SeedSize, ed25519.PrivateKeySize)
	}
}

// New parses signingKeyHex and pairs it with kid (typically a did:key derived from the same key).
func New(signingKeyHex, kid string) (*Service, error) {
	priv, err := Ed25519PrivateKeyFromHex(signingKeyHex)
	if err != nil {
		return nil, err
	}
	if kid == "" {
		return nil, fmt.Errorf("signing kid is required")
	}
	return &Service{priv: priv, kid: kid}, nil
}

// ValidatePlaylist validates core playlist JSON via dp1-go and returns the typed document.
func (s *Service) ValidatePlaylist(raw []byte) (*playlist.Playlist, error) {
	p, err := dp1.ParseAndValidatePlaylist(raw)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("dp1: ParseAndValidatePlaylist returned nil document")
	}
	return p, nil
}

// ValidatePlaylistWithExtension validates playlist+playlists registry extension via dp1-go.
func (s *Service) ValidatePlaylistWithExtension(raw []byte) (*playlist.Playlist, error) {
	p, err := dp1.ParseAndValidatePlaylistWithPlaylistsExtension(raw)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("dp1: ParseAndValidatePlaylistWithPlaylistsExtension returned nil document")
	}
	return p, nil
}

// SignPlaylist signs the document with a v1.1+ multi-signature entry (feed role). Raw JSON must omit top-level signature fields per §7.1.
func (s *Service) SignPlaylist(raw []byte, ts time.Time) ([]byte, error) {
	sig, err := sign.SignMultiEd25519(raw, s.priv, playlist.RoleFeed, ts.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	// Re-hydrate as map so we attach signatures[] and strip legacy top-level signature in one marshal round-trip.
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	m["signatures"] = []playlist.Signature{sig}
	delete(m, "signature")
	return json.Marshal(m)
}

// ValidatePlaylistGroup implements ValidatorSigner.
func (s *Service) ValidatePlaylistGroup(raw []byte) (*playlistgroup.Group, error) {
	g, err := dp1.ParseAndValidatePlaylistGroup(raw)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, fmt.Errorf("dp1: ParseAndValidatePlaylistGroup returned nil document")
	}
	return g, nil
}

// SignPlaylistGroup implements ValidatorSigner (feed role per playlist-group examples).
func (s *Service) SignPlaylistGroup(raw []byte, ts time.Time) ([]byte, error) {
	sig, err := sign.SignMultiEd25519(raw, s.priv, playlist.RoleFeed, ts.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	// Same map round-trip as SignPlaylist: multi-sig array + drop legacy signature.
	m["signatures"] = []playlist.Signature{sig}
	delete(m, "signature")
	return json.Marshal(m)
}

// ValidateChannel implements ValidatorSigner.
func (s *Service) ValidateChannel(raw []byte) (*channels.Channel, error) {
	ch, err := dp1.ParseAndValidateChannel(raw)
	if err != nil {
		return nil, err
	}
	if ch == nil {
		return nil, fmt.Errorf("dp1: ParseAndValidateChannel returned nil document")
	}
	return ch, nil
}

// SignChannel implements ValidatorSigner (feed role per channels extension examples).
func (s *Service) SignChannel(raw []byte, ts time.Time) ([]byte, error) {
	sig, err := sign.SignMultiEd25519(raw, s.priv, playlist.RoleFeed, ts.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	// Same map round-trip as SignPlaylist: multi-sig array + drop legacy signature.
	m["signatures"] = []playlist.Signature{sig}
	delete(m, "signature")
	return json.Marshal(m)
}
