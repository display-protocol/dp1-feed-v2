package httpserver

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	dp1 "github.com/display-protocol/dp1-go"
	"github.com/display-protocol/dp1-go/sign"

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
