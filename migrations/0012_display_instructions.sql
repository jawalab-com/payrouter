-- Direct instrument issuance: when PayRouter renders checkout itself, the
-- gateway returns a payment instrument (QRIS payload, virtual account number)
-- rather than a redirect URL. It is stored as JSON so the checkout page can be
-- reloaded, and so a customer who closes the tab can return to the same
-- instrument instead of being issued a second one for the same order.
--
-- Kept as a single JSON column rather than typed columns because the shape is
-- per-instrument (a QRIS has a payload, a virtual account has a bank and an
-- account number, Mandiri additionally has a biller code) and the canonical
-- definition lives in the gateway package.
SET search_path = facade;

ALTER TABLE payment_intents
    ADD COLUMN display_json text NOT NULL DEFAULT '';
