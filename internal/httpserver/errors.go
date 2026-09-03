package httpserver

// JSON error responses and mapping from store/executor errors to HTTP status + stable error codes.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// ErrorResponse matches the feed OpenAPI error shape.
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// writeJSON sets Content-Type and status, then encodes v (handlers use this indirectly via writeError).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes { error, message } matching the feed OpenAPI error shape.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{Error: code, Message: message})
}

// mapStoreError maps store.ErrNotFound, store.ErrConcurrentModification, store.ErrListLimitExceeded,
// or falls through to 500.
func mapStoreError(err error) (status int, code, msg string) {
	if errors.Is(err, store.ErrNotFound) {
		return http.StatusNotFound, "not_found", err.Error()
	}
	if errors.Is(err, store.ErrConcurrentModification) {
		return http.StatusConflict, "conflict", "the resource changed since it was authorized; re-read and retry"
	}
	if errors.Is(err, store.ErrDocumentDeleted) {
		return http.StatusConflict, "conflict", "this id was deleted on this feed and cannot be reused; publish under a new id"
	}
	// Distinct from the tombstone case above: there the id is gone for good, here it is already taken.
	// Usually a retry of a POST whose response was lost, so the message says the resource is already there.
	// Detail is read off the typed error rather than err.Error(), which would leak the wrapping chain
	// ("store: document already exists: …") into the response body.
	var exists *store.AlreadyExistsError
	if errors.As(err, &exists) {
		return http.StatusConflict, "conflict", exists.Detail
	}
	if errors.Is(err, store.ErrListLimitExceeded) {
		return http.StatusBadRequest, "bad_request", err.Error()
	}
	return http.StatusInternalServerError, "internal_error", err.Error()
}

// mapExecutorError checks executor-specific errors, then delegates to mapStoreError for common cases.
// Executor-specific: extensions disabled, DP-1 sign errors, DP-1 validation errors, trusted model errors.
// Common store errors (not_found, list limits, fallback 500) are handled by mapStoreError.
func mapExecutorError(err error) (status int, code, msg string) {
	if executor.IsExtensionsDisabled(err) {
		return http.StatusNotFound, "extensions_disabled", "DP-1 extensions are disabled on this deployment"
	}
	if executor.IsSignaturesRequiredError(err) {
		return http.StatusUnauthorized, "unauthorized", err.Error()
	}
	if executor.IsForbiddenError(err) {
		return http.StatusForbidden, "forbidden", err.Error()
	}
	if executor.IsIntentError(err) {
		return http.StatusBadRequest, "bad_request", err.Error()
	}
	if executor.IsInvalidSubmissionError(err) {
		return http.StatusBadRequest, "bad_request", err.Error()
	}
	if executor.IsBlockedFetchDestinationError(err) {
		return http.StatusBadRequest, "bad_request", err.Error()
	}
	if executor.IsDP1SignError(err) {
		return http.StatusBadRequest, "signature_invalid", err.Error()
	}
	if executor.IsDP1ValidationError(err) {
		return http.StatusBadRequest, "validation_error", err.Error()
	}
	// Trusted model errors
	if executor.IsSignatureVerificationError(err) {
		return http.StatusBadRequest, "signature_verification_failed", err.Error()
	}
	if executor.IsInvalidTimestampError(err) {
		return http.StatusBadRequest, "invalid_timestamp", err.Error()
	}
	if executor.IsInvalidIDError(err) {
		return http.StatusBadRequest, "invalid_id", err.Error()
	}
	return mapStoreError(err)
}
