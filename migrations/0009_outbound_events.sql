-- Phase 5C: durable outbound Stripe-shaped events. The transactional outbox: a
-- state change and its event enqueue in one transaction, then a deliverer drains
-- due rows with FOR UPDATE SKIP LOCKED, re-signing the payload per attempt. The
-- payload/type/reference/account identity is immutable after insert (forensic
-- integrity); only delivery lifecycle columns mutate.
SET search_path = facade;

CREATE TABLE outbound_events (
    id              text PRIMARY KEY,             -- evt_... (stable Stripe Event id)
    account_id      text REFERENCES accounts(id) ON DELETE RESTRICT,
    type            text NOT NULL,                -- Stripe event type
    reference       text NOT NULL DEFAULT '',     -- pi_/sub_/in_...
    payload         bytea NOT NULL,               -- immutable full Stripe Event JSON
    status          text NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','in_flight','delivered','failed','dead')),
    attempts        integer NOT NULL DEFAULT 0,
    max_attempts    integer NOT NULL DEFAULT 5,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error      text NOT NULL DEFAULT '',
    claim_token     text,
    claimed_until   timestamptz,
    created         bigint NOT NULL,              -- unix seconds (Stripe-shaped)
    created_at      timestamptz NOT NULL DEFAULT now(),
    delivered_at    timestamptz
);

CREATE INDEX outbound_events_due_idx
    ON outbound_events (status, next_attempt_at)
    WHERE status IN ('pending', 'failed');

-- Forensic immutability: the event identity and payload may never change after
-- insert. Only delivery-lifecycle columns (status, attempts, next_attempt_at,
-- last_error, claim_token, claimed_until, delivered_at) are mutable.
CREATE OR REPLACE FUNCTION facade_outbound_immutable_fields() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.type IS DISTINCT FROM OLD.type
       OR NEW.reference IS DISTINCT FROM OLD.reference
       OR NEW.account_id IS DISTINCT FROM OLD.account_id
       OR NEW.payload IS DISTINCT FROM OLD.payload
       OR NEW.created IS DISTINCT FROM OLD.created THEN
        RAISE EXCEPTION 'outbound_events identity/payload is immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER outbound_events_immutable
BEFORE UPDATE ON outbound_events
FOR EACH ROW EXECUTE FUNCTION facade_outbound_immutable_fields();
