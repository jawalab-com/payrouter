-- Phase 5A: accounts and hashed, revocable Stripe-style API keys. The facade
-- resolves a Bearer key to an account (account isolation). Mirrors the Rust
-- platform's billing_api_keys (key_hash + key_prefix + revoked_at) and its
-- resolve_billing_api_key SECURITY DEFINER lookup.
SET search_path = facade;

CREATE TABLE IF NOT EXISTS accounts (
    id          text PRIMARY KEY,            -- acc_...
    livemode    boolean NOT NULL DEFAULT false,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS api_keys (
    id           text PRIMARY KEY,           -- key_... (facade internal id)
    account_id   text NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    key_prefix   text NOT NULL,              -- non-secret searchable prefix (sk_test_/sk_live_ + first chars)
    key_hash     bytea NOT NULL,             -- SHA-256 of the full secret
    last_used_at timestamptz,
    revoked_at   timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, key_prefix)
);

-- Resolve an active (non-revoked) api key by its hash and return the account id,
-- touching last_used_at as a side effect. SECURITY DEFINER so a future
-- least-privilege role can authenticate without UPDATE on api_keys.
CREATE OR REPLACE FUNCTION resolve_api_key(candidate_hash bytea)
RETURNS text
LANGUAGE sql SECURITY DEFINER SET search_path = facade AS $$
    UPDATE api_keys SET last_used_at = now()
    WHERE key_hash = candidate_hash AND revoked_at IS NULL
    RETURNING account_id;
$$;
REVOKE ALL ON FUNCTION resolve_api_key(bytea) FROM PUBLIC;

CREATE INDEX IF NOT EXISTS api_keys_hash_idx   ON api_keys (key_hash)   WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS api_keys_prefix_idx ON api_keys (key_prefix) WHERE revoked_at IS NULL;
