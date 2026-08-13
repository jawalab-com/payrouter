-- Phase 5A: durable request idempotency. One row per (account, operation,
-- Idempotency-Key). The middleware claims a row, runs the handler, then stores
-- the Stripe response; a replay returns the stored response, a payload-hash
-- mismatch is rejected. Mirrors the Rust platform's idempotency_records.
SET search_path = facade;

CREATE TABLE idempotency_records (
    id               bigserial PRIMARY KEY,
    account_id       text NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    operation        text NOT NULL,               -- e.g. payment_intents.create, refunds.create
    idempotency_key  text NOT NULL,
    request_hash     bytea NOT NULL,              -- SHA-256 of the canonical request body
    status           text NOT NULL DEFAULT 'processing'
                     CHECK (status IN ('processing','completed','failed')),
    response_status  integer,                     -- HTTP status of the stored response
    response_body    bytea,                       -- stored Stripe response body
    created_at       timestamptz NOT NULL DEFAULT now(),
    completed_at     timestamptz,
    UNIQUE (account_id, operation, idempotency_key)
);
