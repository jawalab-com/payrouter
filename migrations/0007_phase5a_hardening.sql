-- Phase 5A review hardening: globally unambiguous API keys, recoverable
-- idempotency claims, and account-consistent financial relationships.
SET search_path = facade;

ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_account_id_key_prefix_key;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_key_hash_unique UNIQUE (key_hash);

ALTER TABLE idempotency_records
    ADD COLUMN processing_expires_at timestamptz,
    ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();
UPDATE idempotency_records
SET processing_expires_at = created_at + interval '30 seconds'
WHERE status = 'processing';

ALTER TABLE payment_intents
    ADD CONSTRAINT payment_intents_account_id_id_unique UNIQUE (account_id, id);

ALTER TABLE refunds DROP CONSTRAINT refunds_payment_intent_id_fkey;
ALTER TABLE refunds ADD CONSTRAINT refunds_account_payment_intent_fk
    FOREIGN KEY (account_id, payment_intent_id)
    REFERENCES payment_intents (account_id, id) ON DELETE RESTRICT;

ALTER TABLE provider_attempts DROP CONSTRAINT provider_attempts_payment_intent_id_fkey;
ALTER TABLE provider_attempts ADD CONSTRAINT provider_attempts_account_payment_intent_fk
    FOREIGN KEY (account_id, payment_intent_id)
    REFERENCES payment_intents (account_id, id) ON DELETE RESTRICT;

ALTER TABLE payment_transitions DROP CONSTRAINT payment_transitions_payment_intent_id_fkey;
ALTER TABLE payment_transitions ADD CONSTRAINT payment_transitions_account_payment_intent_fk
    FOREIGN KEY (account_id, payment_intent_id)
    REFERENCES payment_intents (account_id, id) ON DELETE RESTRICT;
