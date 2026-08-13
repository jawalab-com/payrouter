-- Phase 5A: provider attempts — an audited, append-only history of every gateway
-- interaction for a PaymentIntent (create/confirm/refund/status). Replaces the
-- single overwritten FailureMessage: the intent's last_payment_error is derived
-- from the latest attempt whose error is non-empty.
SET search_path = facade;

-- One generic append-only guard, reused by payment_transitions (0006).
CREATE OR REPLACE FUNCTION facade_append_only_reject() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '%.append_only_violation', TG_TABLE_NAME;
END;
$$;

CREATE TABLE provider_attempts (
    id                bigserial PRIMARY KEY,
    account_id        text NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    payment_intent_id text NOT NULL REFERENCES payment_intents(id) ON DELETE RESTRICT,
    reference         text NOT NULL DEFAULT '',  -- pi_... or re_... the attempt concerns
    gateway           text NOT NULL,
    gateway_reference text NOT NULL DEFAULT '',
    operation         text NOT NULL CHECK (operation IN ('create','confirm','refund','status')),
    status            text NOT NULL DEFAULT '',  -- resulting status, '' if not applicable
    request           jsonb NOT NULL DEFAULT '{}'::jsonb,
    response          jsonb NOT NULL DEFAULT '{}'::jsonb,
    error             text NOT NULL DEFAULT '',
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX provider_attempts_intent_idx
    ON provider_attempts (account_id, payment_intent_id, id);

CREATE TRIGGER provider_attempts_no_update
BEFORE UPDATE OR DELETE ON provider_attempts
FOR EACH ROW EXECUTE FUNCTION facade_append_only_reject();
