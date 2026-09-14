ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_current_organization_id_fkey;
ALTER TABLE sessions
    ADD CONSTRAINT sessions_current_organization_id_fkey
    FOREIGN KEY (current_organization_id) REFERENCES organizations (id);

ALTER TABLE email_accounts DROP CONSTRAINT IF EXISTS email_accounts_organization_id_fkey;
ALTER TABLE email_accounts
    ADD CONSTRAINT email_accounts_organization_id_fkey
    FOREIGN KEY (organization_id) REFERENCES organizations (id);

ALTER TABLE sequences DROP CONSTRAINT IF EXISTS sequences_organization_id_fkey;
ALTER TABLE sequences
    ADD CONSTRAINT sequences_organization_id_fkey
    FOREIGN KEY (organization_id) REFERENCES organizations (id);

ALTER TABLE contacts DROP CONSTRAINT IF EXISTS contacts_organization_id_fkey;
ALTER TABLE contacts
    ADD CONSTRAINT contacts_organization_id_fkey
    FOREIGN KEY (organization_id) REFERENCES organizations (id);

ALTER TABLE campaigns DROP CONSTRAINT IF EXISTS campaigns_organization_id_fkey;
ALTER TABLE campaigns
    ADD CONSTRAINT campaigns_organization_id_fkey
    FOREIGN KEY (organization_id) REFERENCES organizations (id);

-- Refuse to roll back while erasure is still owed. These rows are the only
-- remaining copy of a deleted mailbox's refresh token and the prefix its mail
-- sits under; the mailbox they came from is already gone, so dropping the table
-- makes revoking that grant and deleting that mail permanently impossible.
-- Let the queue drain first (the job clears a row as soon as both halves are
-- done), then run this again.
DO $$
DECLARE owed integer;
BEGIN
    SELECT count(*) INTO owed FROM public.mailbox_erasures;
    IF owed > 0 THEN
        RAISE EXCEPTION
            'refusing to roll back: % mailbox erasure(s) still owed. Dropping them loses the only copy of those mailboxes'' refresh tokens, so their provider grants could never be revoked and their stored mail could never be deleted. Wait for the mailbox_erasure job to drain the queue, then retry.', owed;
    END IF;
END $$;

DROP TABLE IF EXISTS public.mailbox_erasures;

ALTER TABLE decision_log DROP CONSTRAINT IF EXISTS decision_log_mailbox_id_fkey;
ALTER TABLE warmup_tampering_events DROP CONSTRAINT IF EXISTS warmup_tampering_events_email_account_id_fkey;
ALTER TABLE warmup_received DROP CONSTRAINT IF EXISTS warmup_received_sender_account_id_fkey;
ALTER TABLE warmup_received DROP CONSTRAINT IF EXISTS warmup_received_email_account_id_fkey;
ALTER TABLE warmup_pending_engagements DROP CONSTRAINT IF EXISTS warmup_pending_engagements_email_account_id_fkey;
ALTER TABLE email_message_map DROP CONSTRAINT IF EXISTS email_message_map_email_id_fkey;
ALTER TABLE email_history_ids DROP CONSTRAINT IF EXISTS email_history_ids_email_id_fkey;
ALTER TABLE email_delta_links DROP CONSTRAINT IF EXISTS email_delta_links_email_id_fkey;
ALTER TABLE ai_thread_drafts DROP CONSTRAINT IF EXISTS ai_thread_drafts_email_account_id_fkey;
