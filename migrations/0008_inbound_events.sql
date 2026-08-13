-- Phase 5B: durable, lease-owned inbound gateway events. Provider identity is
-- the primary logical deduplication key; the raw payload hash remains a safe
-- fallback for providers that expose no stable event identity.
SET search_path = facade;

CREATE TABLE inbound_events (
    id                bigserial PRIMARY KEY,
    account_id        text REFERENCES accounts(id) ON DELETE RESTRICT,
    gateway           text NOT NULL,
    provider_event_id text NOT NULL DEFAULT '',
    event_hash        bytea NOT NULL,
    event_type        text NOT NULL DEFAULT '',
    reference         text NOT NULL DEFAULT '',
    raw               bytea NOT NULL,
    parsed            jsonb NOT NULL DEFAULT '{}'::jsonb,
    status            text NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending','processing','applied','failed','dead')),
    attempts          integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error        text NOT NULL DEFAULT '',
    claim_token       text,
    claimed_until     timestamptz,
    available_at      timestamptz NOT NULL DEFAULT now(),
    applied_at        timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX inbound_events_provider_identity_idx
    ON inbound_events (gateway, provider_event_id, event_type)
    WHERE provider_event_id <> '';

CREATE UNIQUE INDEX inbound_events_payload_fallback_idx
    ON inbound_events (gateway, event_hash, event_type, reference)
    WHERE provider_event_id = '';

CREATE INDEX inbound_events_claim_idx
    ON inbound_events (gateway, available_at, created_at, id)
    WHERE status IN ('pending', 'processing', 'failed');

-- Processing state may advance, but forensic and normalized event identity is
-- immutable after ingestion. Corrections arrive as new provider events.
CREATE OR REPLACE FUNCTION inbound_event_identity_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.gateway IS DISTINCT FROM OLD.gateway
       OR NEW.provider_event_id IS DISTINCT FROM OLD.provider_event_id
       OR NEW.event_hash IS DISTINCT FROM OLD.event_hash
       OR NEW.event_type IS DISTINCT FROM OLD.event_type
       OR NEW.reference IS DISTINCT FROM OLD.reference
       OR NEW.raw IS DISTINCT FROM OLD.raw
       OR NEW.parsed IS DISTINCT FROM OLD.parsed
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'inbound_events.identity_immutable';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER inbound_events_identity_immutable
BEFORE UPDATE ON inbound_events
FOR EACH ROW EXECUTE FUNCTION inbound_event_identity_immutable();
