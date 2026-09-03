package httpserver

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	dp1 "github.com/display-protocol/dp1-go"
	"github.com/display-protocol/dp1-go/sign"

	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

func TestMapExecutorError_dp1SignErrorsAreBadRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"sig_invalid", sign.ErrSigInvalid},
		{"unsupported_alg", sign.ErrUnsupportedAlg},
		{"no_signatures", sign.ErrNoSignatures},
		{"wrapped", fmt.Errorf("verify: %w", sign.ErrSigInvalid)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, code, msg := mapExecutorError(tc.err)
			if st != http.StatusBadRequest || code != "signature_invalid" || msg == "" {
				t.Fatalf("got status=%d code=%q msg=%q", st, code, msg)
			}
		})
	}
}

func TestMapExecutorError_plainMessageErrorsAreInternal(t *testing.T) {
	t.Parallel()
	err := errors.New("post-sign validation: schema says no")
	st, code, _ := mapExecutorError(err)
	if st != http.StatusInternalServerError || code != "internal_error" {
		t.Fatalf("got status=%d code=%q", st, code)
	}
}

func TestMapExecutorError_dp1ValidationErrorsAreBadRequest(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"err_validation", dp1.ErrValidation},
		{"wrapped_validation", fmt.Errorf("post-sign validation: %w", dp1.ErrValidation)},
		{"coded_schema", dp1.WithCode(dp1.CodePlaylistInvalid, fmt.Errorf("inner: %w", dp1.ErrValidation))},
		{"coded_wrapped", fmt.Errorf("x: %w", dp1.WithCode(dp1.CodeChannelInvalid, fmt.Errorf("inner: %w", dp1.ErrValidation)))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, code, msg := mapExecutorError(tc.err)
			if st != http.StatusBadRequest || code != "validation_error" || msg == "" {
				t.Fatalf("got status=%d code=%q msg=%q", st, code, msg)
			}
		})
	}
}

func TestMapExecutorError_notFound(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrap: %w", store.ErrNotFound)
	st, code, _ := mapExecutorError(err)
	if st != http.StatusNotFound || code != "not_found" {
		t.Fatalf("got status=%d code=%q", st, code)
	}
}

// TestMapExecutorError_signatureAuthzErrors covers the signatures-only error mappings: missing
// signatures (401), ownership failures (403), and a malformed delete-intent (400).
func TestMapExecutorError_signatureAuthzErrors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		err      error
		wantSt   int
		wantCode string
	}{
		{"signatures_required", executor.ErrSignaturesRequired, http.StatusUnauthorized, "unauthorized"},
		{"signatures_required_wrapped", fmt.Errorf("x: %w", executor.ErrSignaturesRequired), http.StatusUnauthorized, "unauthorized"},
		{"not_owner", executor.ErrNotResourceOwner, http.StatusForbidden, "forbidden"},
		{"owner_immutable", executor.ErrOwnerImmutable, http.StatusForbidden, "forbidden"},
		{"owner_immutable_wrapped", fmt.Errorf("x: %w", executor.ErrOwnerImmutable), http.StatusForbidden, "forbidden"},
		{"delete_request_invalid", executor.ErrIntentInvalid, http.StatusBadRequest, "bad_request"},
		{"delete_request_wrapped", fmt.Errorf("x: %w", executor.ErrIntentInvalid), http.StatusBadRequest, "bad_request"},
		{"slug_required", executor.ErrSlugRequired, http.StatusBadRequest, "bad_request"},
		{"item_id_required", executor.ErrItemIDRequired, http.StatusBadRequest, "bad_request"},
		{"item_id_required_wrapped", fmt.Errorf("x: %w", executor.ErrItemIDRequired), http.StatusBadRequest, "bad_request"},
		{"signature_verification", executor.ErrSignatureVerificationFailed, http.StatusBadRequest, "signature_verification_failed"},
		{"no_curator_signature", executor.ErrNoValidCuratorSignature, http.StatusBadRequest, "signature_verification_failed"},
		{"no_publisher_signature", executor.ErrNoValidPublisherSignature, http.StatusBadRequest, "signature_verification_failed"},
		{"invalid_timestamp", executor.ErrInvalidTimestamp, http.StatusBadRequest, "invalid_timestamp"},
		{"invalid_id", executor.ErrInvalidID, http.StatusBadRequest, "invalid_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, code, msg := mapExecutorError(tc.err)
			if st != tc.wantSt || code != tc.wantCode || msg == "" {
				t.Fatalf("got status=%d code=%q msg=%q; want %d %q", st, code, msg, tc.wantSt, tc.wantCode)
			}
		})
	}
}

// TestMapStoreError_listLimitExceeded covers the list-limit branch of mapStoreError.
func TestMapStoreError_listLimitExceeded(t *testing.T) {
	t.Parallel()
	st, code, _ := mapExecutorError(fmt.Errorf("wrap: %w", store.ErrListLimitExceeded))
	if st != http.StatusBadRequest || code != "bad_request" {
		t.Fatalf("got status=%d code=%q", st, code)
	}
}

// A write refused because the row changed since it was authorized is a client-retryable conflict, not a
// 500: the caller should re-read and retry against the new generation.
func TestMapStoreError_concurrentModification(t *testing.T) {
	t.Parallel()
	st, code, msg := mapExecutorError(fmt.Errorf("store: %w", store.ErrConcurrentModification))
	if st != http.StatusConflict || code != "conflict" {
		t.Fatalf("got status=%d code=%q", st, code)
	}
	if !strings.Contains(msg, "re-read and retry") {
		t.Fatalf("message should tell the client what to do, got %q", msg)
	}
}

// A create naming a deleted id is a conflict, not a generic 500: the id is retired on this feed so a
// replay of the old (still validly signed) bytes cannot resurrect it, and the client needs to know that
// re-publishing requires a new id.
func TestMapStoreError_documentDeleted(t *testing.T) {
	t.Parallel()
	st, code, msg := mapExecutorError(fmt.Errorf("store: %w", store.ErrDocumentDeleted))
	if st != http.StatusConflict || code != "conflict" {
		t.Fatalf("got status=%d code=%q", st, code)
	}
	if !strings.Contains(msg, "new id") {
		t.Fatalf("message should tell the client to publish under a new id, got %q", msg)
	}
}
