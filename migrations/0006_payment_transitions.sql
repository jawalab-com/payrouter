-- Phase 5A: append-only PaymentIntent status transitions. Every status change
-- inserts a row; the table rejects UPDATE/DELETE via the generic
-- facade_append_only_reject guard defined in 0003.
SET search_path = facade;

CREATE TABLE payment_transitions (
    id                bigserial PRIMARY KEY,
    account_id        text NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    payment_intent_id text NOT NULL REFERENCES payment_intents(id) ON DELETE RESTRICT,
    from_status       text NOT NULL DEFAULT '',
    to_status         text NOT NULL,
    reason            text NOT NULL DEFAULT '',
    metadata          jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX payment_transitions_intent_idx
    ON payment_transitions (account_id, payment_intent_id, id);

CREATE TRIGGER payment_transitions_no_update
BEFORE UPDATE OR DELETE ON payment_transitions
FOR EACH ROW EXECUTE FUNCTION facade_append_only_reject();
