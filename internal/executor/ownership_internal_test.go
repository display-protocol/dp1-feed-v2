package executor

import (
	"errors"
	"strings"
	"testing"

	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/extension/identity"
	"github.com/display-protocol/dp1-go/playlist"
)

// The ownership rules are pure functions over key sets and signature entries, so they are exercised
// here directly, in-package, without mocks. Behavior through the executor API (which error each
// mutation returns, and when) lives in executor_test.go.

func sig(kid, role string) playlist.Signature {
	return playlist.Signature{Alg: "ed25519", Kid: kid, Role: role, Sig: "s"}
}

func keys(ks ...string) keySet {
	set := make(keySet, len(ks))
	for _, k := range ks {
		set[k] = struct{}{}
	}
	return set
}

func sameKeys(a, b keySet) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

func TestOwnerSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		declared keySet
		role     string
		sigs     []playlist.Signature
		want     keySet
	}{
		{
			name:     "declared owners win even when other keys sign in the owner role",
			declared: keys("K1"),
			role:     playlist.RoleCurator,
			sigs:     []playlist.Signature{sig("K1", playlist.RoleCurator), sig("K2", playlist.RoleCurator)},
			want:     keys("K1"),
		},
		{
			name:     "declared owners are the set even if none of them signed",
			declared: keys("K1"),
			role:     playlist.RoleCurator,
			sigs:     []playlist.Signature{sig("K9", playlist.RoleLicensor)},
			want:     keys("K1"),
		},
		{
			name: "no declared owners: owner-role signers, other roles and the feed ignored",
			role: playlist.RoleCurator,
			sigs: []playlist.Signature{
				sig("K1", playlist.RoleCurator),
				sig("K2", playlist.RoleLicensor),
				sig("K3", playlist.RoleAgent),
				sig("KF", playlist.RoleFeed),
				sig(" K4 ", playlist.RoleCurator),
			},
			want: keys("K1", "K4"),
		},
		{
			name:     "empty declared set falls through to the signature chain",
			declared: keySet{},
			role:     channels.RolePublisher,
			sigs:     []playlist.Signature{sig("P1", channels.RolePublisher), sig("C1", playlist.RoleCurator)},
			want:     keys("P1"),
		},
		{
			name: "nothing signed in the owner role: empty",
			role: playlist.RoleCurator,
			sigs: []playlist.Signature{sig("K1", playlist.RoleAgent), sig("", playlist.RoleCurator)},
			want: keys(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ownerSet(tc.declared, tc.role, tc.sigs); !sameKeys(got, tc.want) {
				t.Fatalf("ownerSet = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRequireOwnerSignature(t *testing.T) {
	t.Parallel()
	missing := errors.New("missing")
	tests := []struct {
		name        string
		owners      keySet
		sigs        []playlist.Signature
		wantErr     bool
		wantMention string // substring the error must carry, when it must explain itself
	}{
		{
			name:   "owner signing in the owner role authorizes",
			owners: keys("K1"),
			sigs:   []playlist.Signature{sig("K2", playlist.RoleLicensor), sig("K1", playlist.RoleCurator)},
		},
		{
			name:        "owner signing only as licensor does not, and the error says so",
			owners:      keys("K1"),
			sigs:        []playlist.Signature{sig("K1", playlist.RoleLicensor)},
			wantErr:     true,
			wantMention: `K1 signed as "licensor"`,
		},
		{
			name:        "owner signing only as agent does not",
			owners:      keys("K1"),
			sigs:        []playlist.Signature{sig("K1", playlist.RoleAgent)},
			wantErr:     true,
			wantMention: `"curator" role is required`,
		},
		{
			name:    "non-owner signing in the owner role does not",
			owners:  keys("K1"),
			sigs:    []playlist.Signature{sig("K2", playlist.RoleCurator)},
			wantErr: true,
		},
		{
			name:        "empty owner set: nobody can authorize",
			owners:      keys(),
			sigs:        []playlist.Signature{sig("K1", playlist.RoleCurator)},
			wantErr:     true,
			wantMention: `no "curator"-role signature`,
		},
		{
			name:   "kid whitespace is tolerated",
			owners: keys("K1"),
			sigs:   []playlist.Signature{sig(" K1 ", playlist.RoleCurator)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := requireOwnerSignature(tc.owners, playlist.RoleCurator, tc.sigs, missing)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, missing) {
				t.Fatalf("error %v does not wrap the caller's sentinel", err)
			}
			if tc.wantMention != "" && !strings.Contains(err.Error(), tc.wantMention) {
				t.Fatalf("error %q should mention %q", err, tc.wantMention)
			}
		})
	}
}

func TestRequireOwnersRetained(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		stored, incoming keySet
		wantRemoved      string // "" means allowed
	}{
		{name: "identical", stored: keys("K1"), incoming: keys("K1")},
		{name: "growth is allowed", stored: keys("K1"), incoming: keys("K1", "K2")},
		{name: "empty stored set is trivially retained", stored: keys(), incoming: keys("K1")},
		{name: "swap removes the stored owner", stored: keys("K1"), incoming: keys("K2"), wantRemoved: "K1"},
		{name: "shrink is refused, listing every removed key", stored: keys("K1", "K2", "K3"), incoming: keys("K2"), wantRemoved: "K1, K3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := requireOwnersRetained(tc.stored, tc.incoming)
			if tc.wantRemoved == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrOwnerRemoved) || !strings.HasSuffix(err.Error(), tc.wantRemoved) {
				t.Fatalf("got %v, want ErrOwnerRemoved naming %q", err, tc.wantRemoved)
			}
		})
	}
}

func TestRequireNewOwnerConsent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name             string
		stored, incoming keySet
		sigs             []playlist.Signature
		wantUnsigned     string // "" means allowed
	}{
		{
			name: "no change needs no consent", stored: keys("K1"), incoming: keys("K1"),
			sigs: []playlist.Signature{sig("K1", playlist.RoleCurator)},
		},
		{
			name: "new owner signed in the owner role", stored: keys("K1"), incoming: keys("K1", "K2"),
			sigs: []playlist.Signature{sig("K1", playlist.RoleCurator), sig("K2", playlist.RoleCurator)},
		},
		{
			name: "new owner did not sign at all", stored: keys("K1"), incoming: keys("K1", "K2"),
			sigs:         []playlist.Signature{sig("K1", playlist.RoleCurator)},
			wantUnsigned: "K2",
		},
		{
			name: "new owner signed, but as licensor", stored: keys("K1"), incoming: keys("K1", "K2"),
			sigs:         []playlist.Signature{sig("K1", playlist.RoleCurator), sig("K2", playlist.RoleLicensor)},
			wantUnsigned: "K2",
		},
		{
			name: "existing owners need not re-sign as owner for others to be added", stored: keys("K1", "K3"), incoming: keys("K1", "K2", "K3"),
			sigs: []playlist.Signature{sig("K1", playlist.RoleCurator), sig("K2", playlist.RoleCurator)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := requireNewOwnerConsent(tc.stored, tc.incoming, playlist.RoleCurator, tc.sigs)
			if tc.wantUnsigned == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrOwnerConsentRequired) || !strings.HasSuffix(err.Error(), tc.wantUnsigned) {
				t.Fatalf("got %v, want ErrOwnerConsentRequired naming %q", err, tc.wantUnsigned)
			}
		})
	}
}

func TestDeclaredKeySets(t *testing.T) {
	t.Parallel()
	got := entityKeySet([]identity.Entity{{Key: " K1 "}, {Key: ""}, {Name: "no key"}, {Key: "K2"}})
	if !sameKeys(got, keys("K1", "K2")) {
		t.Fatalf("entityKeySet = %v", got)
	}
	if got := publisherKeySet(nil); len(got) != 0 {
		t.Fatalf("publisherKeySet(nil) = %v", got)
	}
	if got := publisherKeySet(&identity.Entity{Key: "  "}); len(got) != 0 {
		t.Fatalf("publisherKeySet(blank) = %v", got)
	}
	if got := publisherKeySet(&identity.Entity{Key: "P1"}); !sameKeys(got, keys("P1")) {
		t.Fatalf("publisherKeySet = %v", got)
	}
}

func TestIsForbiddenError(t *testing.T) {
	t.Parallel()
	for _, err := range []error{ErrNotResourceOwner, ErrOwnerRemoved, ErrOwnerConsentRequired} {
		if !IsForbiddenError(err) {
			t.Errorf("%v should be forbidden", err)
		}
	}
	if IsForbiddenError(nil) || IsForbiddenError(ErrSignaturesRequired) {
		t.Error("non-ownership errors must not be forbidden")
	}
}
