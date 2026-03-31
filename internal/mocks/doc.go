// Package mocks holds gomock-generated test doubles for interfaces declared in this module.
//
// Regenerate all mocks from the repository root (Go 1.24+, mockgen via go.mod tool):
//
//	go generate ./...
//
// Individual directives live below; they use go tool mockgen (not a global mockgen install).
package mocks

//go:generate go tool mockgen -destination=store_mock.go -package=mocks github.com/display-protocol/dp1-feed-v2/internal/store Store
//go:generate go tool mockgen -destination=executor_mock.go -package=mocks github.com/display-protocol/dp1-feed-v2/internal/executor Executor
//go:generate go tool mockgen -destination=dp1svc_mock.go -package=mocks github.com/display-protocol/dp1-feed-v2/internal/dp1svc ValidatorSigner
