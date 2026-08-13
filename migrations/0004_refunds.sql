-- Phase 5A: durable Refunds. account-scoped; scoped by the originating
-- PaymentIntent for "refunds on an intent" lookups.
SET search_path = facade;

CREATE TABLE refunds (
    id                text PRIMARY KEY,            -- re_...
    account_id        text NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    payment_intent_id text NOT NULL REFERENCES payment_intents(id) ON DELETE RESTRICT,
    amount_minor      bigint NOT NULL CHECK (amount_minor >= 0),
    currency          char(3) NOT NULL,
    status            text NOT NULL,               -- succeeded | failed | canceled | pending
    reason            text NOT NULL DEFAULT '',
    metadata          jsonb NOT NULL DEFAULT '{}'::jsonb,
    livemode          boolean NOT NULL DEFAULT false,
    created           bigint NOT NULL,             -- unix seconds (Stripe-shaped)
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX refunds_account_intent_idx
    ON refunds (account_id, payment_intent_id, id);
