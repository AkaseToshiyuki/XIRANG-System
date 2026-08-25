package build

import (
	"context"
	"time"

	"github.com/xirang-grid/xirang/internal/domain"
)

// UnavailableManager keeps the management API discoverable when a native
// worker has not been configured. Clients receive an explicit unready worker
// and 503 responses instead of mistaking missing routes for a broken console.
type UnavailableManager struct {
	architecture string
}

func NewUnavailableManager(architecture string) *UnavailableManager {
	return &UnavailableManager{architecture: architecture}
}

func (manager *UnavailableManager) Submit(context.Context, domain.BuildRecipe, domain.Principal) (domain.Build, error) {
	return domain.Build{}, ErrUnavailable
}
func (manager *UnavailableManager) Get(context.Context, string) (domain.Build, error) {
	return domain.Build{}, ErrUnavailable
}
func (manager *UnavailableManager) List(context.Context, int) ([]domain.Build, error) {
	return []domain.Build{}, nil
}
func (manager *UnavailableManager) Cancel(context.Context, string, domain.Principal) (domain.Build, error) {
	return domain.Build{}, ErrUnavailable
}
func (manager *UnavailableManager) Delete(context.Context, string, domain.Principal) (domain.BuildCleanupResult, error) {
	return domain.BuildCleanupResult{}, ErrUnavailable
}
func (manager *UnavailableManager) Cleanup(context.Context, domain.Principal) (domain.BuildCleanupResult, error) {
	return domain.BuildCleanupResult{}, nil
}
func (manager *UnavailableManager) RetentionPolicy() domain.BuildRetentionPolicy {
	return domain.BuildRetentionPolicy{MaximumRecords: 500, MaximumAge: 30 * 24 * time.Hour}
}
func (manager *UnavailableManager) UpdateRetentionPolicy(context.Context, domain.BuildRetentionPolicy, domain.Principal) (domain.BuildRetentionPolicy, error) {
	return domain.BuildRetentionPolicy{}, ErrUnavailable
}
func (manager *UnavailableManager) Workers() []domain.BuildWorker {
	return []domain.BuildWorker{{
		ID: "local-" + manager.architecture, Architecture: manager.architecture,
		Isolation: "native worker is not enabled", Ready: false, Toolchains: []string{},
	}}
}
func (manager *UnavailableManager) SigningKey() domain.BuildSigningKey {
	return domain.BuildSigningKey{}
}
func (manager *UnavailableManager) RotateSigningKey(context.Context, domain.Principal) (domain.BuildSigningKey, error) {
	return domain.BuildSigningKey{}, ErrUnavailable
}
