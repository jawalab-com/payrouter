-- Phase 5D part 2: durable, account-scoped checkout sessions, subscriptions, and
-- invoices — the recurring-billing resources migrated off the in-memory store.
-- Account-owned (account_id FKs); HTTP isolation is enforced by the handlers
-- comparing the caller's account to the row's owner. Money is integer minor
-- units + ISO currency.
SET search_path = facade;

CREATE TABLE checkout_sessions (
    id                 text PRIMARY KEY,            -- cs_...
    account_id         text NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    mode               text NOT NULL DEFAULT 'payment',
    status             text NOT NULL DEFAULT 'open',   -- open | complete | expired
    payment_status     text NOT NULL DEFAULT 'unpaid', -- paid | unpaid | no_payment_required
    amount_subtotal    bigint NOT NULL DEFAULT 0,
    amount_total       bigint NOT NULL DEFAULT 0,
    currency           char(3) NOT NULL DEFAULT '',
    customer_id        text NOT NULL DEFAULT '',
    customer_email     text NOT NULL DEFAULT '',
    success_url        text NOT NULL DEFAULT '',
    cancel_url         text NOT NULL DEFAULT '',
    url                text NOT NULL DEFAULT '',     -- gateway hosted page while open
    payment_intent_id  text NOT NULL DEFAULT '',     -- pi_... (payment mode)
    subscription_id    text NOT NULL DEFAULT '',     -- sub_... (subscription mode)
    client_reference_id text NOT NULL DEFAULT '',
    description        text NOT NULL DEFAULT '',
    metadata           jsonb NOT NULL DEFAULT '{}'::jsonb,
    created            bigint NOT NULL,
    expires_at         bigint NOT NULL DEFAULT 0,
    livemode           boolean NOT NULL DEFAULT false,
    created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX checkout_sessions_pi_idx
    ON checkout_sessions (account_id, payment_intent_id)
    WHERE payment_intent_id <> '';

CREATE TABLE subscriptions (
    id                    text PRIMARY KEY,          -- sub_...
    account_id            text NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    customer_id           text NOT NULL DEFAULT '',  -- cus_...
    price_id              text NOT NULL DEFAULT '',  -- price_... (recurring)
    status                text NOT NULL,             -- incomplete | active | canceled | past_due
    gateway_id            text NOT NULL DEFAULT '',  -- gateway subscription id (set on activation)
    auth_payment_intent_id text NOT NULL DEFAULT '', -- pi_... of the authorization save-card charge
    interval              text NOT NULL DEFAULT '',
    interval_count        bigint NOT NULL DEFAULT 1,
    amount_minor          bigint NOT NULL,
    currency              char(3) NOT NULL,
    current_period_end    bigint NOT NULL DEFAULT 0,
    latest_invoice_id     text NOT NULL DEFAULT '',  -- in_...
    canceled_at           bigint NOT NULL DEFAULT 0,
    metadata              jsonb NOT NULL DEFAULT '{}'::jsonb,
    created               bigint NOT NULL,
    livemode              boolean NOT NULL DEFAULT false,
    created_at            timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX subscriptions_gw_id_idx
    ON subscriptions (account_id, gateway_id)
    WHERE gateway_id <> '';
CREATE INDEX subscriptions_auth_pi_idx
    ON subscriptions (account_id, auth_payment_intent_id)
    WHERE auth_payment_intent_id <> '';

CREATE TABLE invoices (
    id               text PRIMARY KEY,               -- in_...
    account_id       text NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    subscription_id  text NOT NULL DEFAULT '',       -- sub_...
    payment_intent_id text NOT NULL DEFAULT '',      -- pi_... of the charge backing this invoice
    customer_id      text NOT NULL DEFAULT '',
    amount_minor     bigint NOT NULL,
    currency         char(3) NOT NULL,
    status           text NOT NULL,                  -- draft | open | paid | void
    billing_reason   text NOT NULL DEFAULT '',       -- subscription_create | subscription_cycle
    metadata         jsonb NOT NULL DEFAULT '{}'::jsonb,
    created          bigint NOT NULL,
    livemode         boolean NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX invoices_pi_idx
    ON invoices (account_id, payment_intent_id)
    WHERE payment_intent_id <> '';
