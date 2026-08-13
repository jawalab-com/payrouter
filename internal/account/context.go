// Package account carries the resolved billing account id through a request
// context. The auth middleware resolves a Stripe-style API key to an account and
// stashes it here; handlers read it to scope reads/writes (account isolation).
package account

import "context"

type ctxKey struct{}
type idempotencyKey struct{}

// With returns a copy of ctx carrying the account id.
func With(ctx context.Context, accountID string) context.Context {
	return context.WithValue(ctx, ctxKey{}, accountID)
}

// From returns the account id carried in ctx, or "" if none is set.
func From(ctx context.Context) string {
	v, _ := ctx.Value(ctxKey{}).(string)
	return v
}

func WithIdempotency(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, idempotencyKey{}, key)
}

func Idempotency(ctx context.Context) string {
	v, _ := ctx.Value(idempotencyKey{}).(string)
	return v
}
