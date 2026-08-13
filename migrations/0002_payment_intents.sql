-- Phase 5A: durable PaymentIntents. account-scoped; money is integer minor
-- units + ISO currency (Stripe-shaped). Status holds a stripe.PaymentIntentStatus
-- string value. `created` is unix seconds (Stripe shape); created_at/updated_at
-- are DB-native. Secondary lookups: gateway_reference (callbacks that carry the
-- gateway's own id) and created-desc pagination.
SET search_path = facade;

CREATE TABLE payment_intents (
    id                  text PRIMARY KEY,            -- pi_...
    account_id          text NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    amount_minor        bigint NOT NULL CHECK (amount_minor >= 0),
    currency            char(3) NOT NULL,
    status              text NOT NULL,               -- stripe.PaymentIntentStatus value
    client_secret       text NOT NULL,
    payment_method_type text NOT NULL DEFAULT '',
    gateway             text NOT NULL DEFAULT '',
    gateway_reference   text NOT NULL DEFAULT '',
    next_action_type    text NOT NULL DEFAULT '',
    next_action_url     text NOT NULL DEFAULT '',
    next_action_return  text NOT NULL DEFAULT '',
    description         text NOT NULL DEFAULT '',
    failure_message     text NOT NULL DEFAULT '',
    metadata            jsonb NOT NULL DEFAULT '{}'::jsonb,
    livemode            boolean NOT NULL DEFAULT false,
    created             bigint NOT NULL,             -- unix seconds (Stripe-shaped)
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX payment_intents_account_gw_ref_idx
    ON payment_intents (account_id, gateway_reference)
    WHERE gateway_reference <> '';
CREATE INDEX payment_intents_account_created_idx
    ON payment_intents (account_id, created DESC, id DESC);
