-- Validate the constraints migration 000158 added NOT VALID.
--
-- Its own migration, and therefore its own transaction, because that is the
-- entire point: VALIDATE CONSTRAINT takes SHARE UPDATE EXCLUSIVE and scans
-- without blocking reads or writes, but run in 000158 it would sit behind the
-- ACCESS EXCLUSIVE locks that migration holds on contacts, campaigns and
-- email_accounts until it commits.
--
-- The constraints are already enforcing new writes and already performing their
-- delete actions. This only marks the existing rows as checked, which is what
-- lets the planner rely on them. 000158 deleted every violating row first, so
-- nothing here can fail.

ALTER TABLE ai_thread_drafts VALIDATE CONSTRAINT ai_thread_drafts_email_account_id_fkey;
ALTER TABLE email_delta_links VALIDATE CONSTRAINT email_delta_links_email_id_fkey;
ALTER TABLE email_history_ids VALIDATE CONSTRAINT email_history_ids_email_id_fkey;
ALTER TABLE email_message_map VALIDATE CONSTRAINT email_message_map_email_id_fkey;
ALTER TABLE warmup_pending_engagements VALIDATE CONSTRAINT warmup_pending_engagements_email_account_id_fkey;
ALTER TABLE warmup_received VALIDATE CONSTRAINT warmup_received_email_account_id_fkey;
ALTER TABLE warmup_received VALIDATE CONSTRAINT warmup_received_sender_account_id_fkey;
ALTER TABLE warmup_tampering_events VALIDATE CONSTRAINT warmup_tampering_events_email_account_id_fkey;
ALTER TABLE decision_log VALIDATE CONSTRAINT decision_log_mailbox_id_fkey;

ALTER TABLE campaigns VALIDATE CONSTRAINT campaigns_organization_id_fkey;
ALTER TABLE contacts VALIDATE CONSTRAINT contacts_organization_id_fkey;
ALTER TABLE sequences VALIDATE CONSTRAINT sequences_organization_id_fkey;
ALTER TABLE email_accounts VALIDATE CONSTRAINT email_accounts_organization_id_fkey;
ALTER TABLE sessions VALIDATE CONSTRAINT sessions_current_organization_id_fkey;
