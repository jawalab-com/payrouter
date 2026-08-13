-- Phase 5D (part 1): durable, account-scoped catalog — customers, products,
-- prices — migrated off the in-memory store. Money is integer minor units +
-- ISO currency (Stripe-shaped). Rows are owned by an account; HTTP isolation is
-- enforced by the handlers comparing the caller's account to the row's owner.
SET search_path = facade;

CREATE TABLE customers (
    id         text PRIMARY KEY,             -- cus_...
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    email      text NOT NULL DEFAULT '',
    name       text NOT NULL DEFAULT '',
    phone      text NOT NULL DEFAULT '',
    metadata   jsonb NOT NULL DEFAULT '{}'::jsonb,
    created    bigint NOT NULL,
    livemode   boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE products (
    id          text PRIMARY KEY,            -- prod_...
    account_id  text NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    name        text NOT NULL,
    description text NOT NULL DEFAULT '',
    active      boolean NOT NULL DEFAULT true,
    metadata    jsonb NOT NULL DEFAULT '{}'::jsonb,
    created     bigint NOT NULL,
    updated     bigint NOT NULL,
    livemode    boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE prices (
    id             text PRIMARY KEY,         -- price_...
    account_id     text NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    product_id     text NOT NULL DEFAULT '', -- prod_...
    unit_amount    bigint NOT NULL,
    currency       char(3) NOT NULL,
    type           text NOT NULL CHECK (type IN ('one_time','recurring')),
    interval       text NOT NULL DEFAULT '', -- recurring: day|week|month|year
    interval_count bigint NOT NULL DEFAULT 1,
    usage_type     text NOT NULL DEFAULT 'licensed',
    active         boolean NOT NULL DEFAULT true,
    metadata       jsonb NOT NULL DEFAULT '{}'::jsonb,
    created        bigint NOT NULL,
    livemode       boolean NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX prices_account_product_idx ON prices (account_id, product_id);
CREATE INDEX products_account_idx ON products (account_id);
CREATE INDEX customers_account_idx ON customers (account_id);
