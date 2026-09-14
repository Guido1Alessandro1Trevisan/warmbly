-- Erasing a mailbox has to clear the labels and snoozes on threads it emptied,
-- which asks "does any message in this thread still exist in this workspace".
-- Every existing index on unibox_emails leads with user_id or email_id, so that
-- question was a sequential scan. The unibox's own cross-mailbox thread lookups
-- read the same way.
--
-- Built concurrently, on its own, because unibox_emails holds every message
-- every mailbox has ever synced and a plain CREATE INDEX would block writes on
-- it for the duration, which is a mailbox sync stalling across the fleet.
-- CONCURRENTLY cannot run inside a transaction, and golang-migrate sends a
-- whole file as one, so this file has exactly one statement.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_unibox_emails_thread_id
    ON public.unibox_emails USING btree (thread_id);
