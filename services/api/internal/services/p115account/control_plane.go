package p115account

import (
	"context"

	p115 "github.com/konghang/ember/backend/internal/integrations/p115"

	"gorm.io/gorm"
)

// ControlPlaneRevoker exposes only credential erasure needed by user deletion.
// It cannot decrypt credentials or touch Redis leases.
type ControlPlaneRevoker struct {
	store         personalRevocationStore
	forgetAccount func(string)
}

type personalRevocationStore interface {
	RevokePersonal(context.Context, string) (string, error)
}

// NewControlPlaneRevoker builds the narrow user-deletion dependency.
func NewControlPlaneRevoker(database *gorm.DB) (*ControlPlaneRevoker, error) {
	if database == nil {
		return nil, ErrStoreUnavailable
	}
	return &ControlPlaneRevoker{store: &gormAccountStore{db: database}, forgetAccount: p115.ForgetDataPlaneAccount}, nil
}

// RevokePersonalAccount writes the irreversible tombstone for one user, then
// notifies local pacing cleanup only after the transaction commits.
func (r *ControlPlaneRevoker) RevokePersonalAccount(ctx context.Context, userID string) error {
	return revokePersonalAndForget(ctx, r.store, userID, r.forgetAccount)
}

// revokePersonalAndForget cleans up only the UID returned by the committed
// tombstone operation. A failed transaction or idempotent miss has no side effect.
func revokePersonalAndForget(ctx context.Context, store personalRevocationStore, userID string, forget func(string)) error {
	providerUserID, err := store.RevokePersonal(ctx, userID)
	if err != nil {
		return err
	}
	if providerUserID != "" && forget != nil {
		forget(providerUserID)
	}
	return nil
}
