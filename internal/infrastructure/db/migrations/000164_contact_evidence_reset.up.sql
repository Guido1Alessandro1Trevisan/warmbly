-- When a contact's address last changed, so the delivery-credit job can tell
-- mail sent to the old mailbox from mail sent to this one.
--
-- A contact's verdict and its evidence ledger describe an ADDRESS, so editing
-- the address wipes both. That wipe did not hold on its own: the credit job
-- re-derives a 'delivered' row from every step ever sent, with no lower bound
-- of its own, and its dedupe is exactly the set of rows the wipe deleted. The
-- next pass handed them straight back, and the new address inherited a verdict
-- earned by the old one, decisive enough to excuse it from ever being probed
-- (issue #511).
--
-- NULL means the address has never changed, which is every contact today.
ALTER TABLE contacts
    ADD COLUMN verification_evidence_reset_at timestamptz;
