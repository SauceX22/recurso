package domain

import "github.com/google/uuid"

// TenantScopedID pairs a row ID with its owning tenant. Background sweeps run
// on a tenantless context, so a candidate list must carry each row's tenant
// for the sweep to inject it (TenantIDKey) before calling tenant-scoped
// repository methods.
type TenantScopedID struct {
	ID       uuid.UUID
	TenantID uuid.UUID
}
