package storepg

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SeedDefaultAccount ensures a single default account exists and registers the
// given Stripe-style secret as its API key (idempotent). This keeps existing
// clients using PAYMENT_API_KEY working when the facade moves to durable,
// account-scoped auth. Multi-account administration arrives in a later phase.
func SeedDefaultAccount(ctx context.Context, pool *pgxpool.Pool, secret string) error {
	livemode := strings.HasPrefix(secret, "sk_live_")
	s := New(pool, "") // gateway name is irrelevant for seeding
	if err := s.EnsureAccount(ctx, "acc_default", livemode); err != nil {
		return err
	}
	hash, prefix := HashAPIKey(secret)
	return s.EnsureAPIKey(ctx, "acc_default", prefix, hash)
}
