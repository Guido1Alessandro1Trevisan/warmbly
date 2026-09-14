-- Disconnecting a mailbox left three kinds of residue behind.
--
-- 1. Nine tables name a mailbox with no foreign key, so their rows outlived the
--    mailbox forever. Two of them (warmup_received, warmup_tampering_events)
--    hold the message ids of mail that arrived in someone's inbox, and
--    email_message_map maps provider message ids to internal ones.
-- 2. Every message body the mailbox ever synced stays in object storage. The
--    row in unibox_emails cascades; the bytes it points at did not.
-- 3. The OAuth grant stays live at Google or Microsoft. Deleting our copy of a
--    refresh token does not revoke it, and Google's API Services User Data
--    Policy requires that a user's data be deleted on request, promptly.
--
-- (1) is closed here with foreign keys. (2) and (3) cannot be: they are work
-- against an HTTP endpoint and an object store, which no transaction can do,
-- and which must survive the process that started them. mailbox_erasures is
-- the record of that outstanding work.
--
-- Every constraint below is added NOT VALID. That binds new writes and makes
-- the delete actions live immediately, without scanning the table under the
-- lock; migration 000159 does the scan, under a weaker lock that does not block
-- writes. golang-migrate sends a whole file as one implicit transaction, so a
-- VALIDATE here would sit behind the ACCESS EXCLUSIVE locks this migration
-- already holds, and contacts and campaigns are live tables.

-- ---------- 1. the rows that had no foreign key ----------

-- Orphans first: every constraint below is rejected while a row still points
-- at a mailbox that is already gone. NOT EXISTS rather than NOT IN, which
-- plans as an anti-join and does not silently match nothing if the subquery
-- ever yields a NULL.
DELETE FROM ai_thread_drafts t
 WHERE NOT EXISTS (SELECT 1 FROM email_accounts a WHERE a.id = t.email_account_id);
DELETE FROM email_delta_links t
 WHERE NOT EXISTS (SELECT 1 FROM email_accounts a WHERE a.id = t.email_id);
DELETE FROM email_history_ids t
 WHERE NOT EXISTS (SELECT 1 FROM email_accounts a WHERE a.id = t.email_id);
DELETE FROM email_message_map t
 WHERE NOT EXISTS (SELECT 1 FROM email_accounts a WHERE a.id = t.email_id);
DELETE FROM warmup_pending_engagements t
 WHERE NOT EXISTS (SELECT 1 FROM email_accounts a WHERE a.id = t.email_account_id);
DELETE FROM warmup_received t
 WHERE NOT EXISTS (SELECT 1 FROM email_accounts a WHERE a.id = t.email_account_id)
    OR NOT EXISTS (SELECT 1 FROM email_accounts a WHERE a.id = t.sender_account_id);
DELETE FROM warmup_tampering_events t
 WHERE NOT EXISTS (SELECT 1 FROM email_accounts a WHERE a.id = t.email_account_id);

ALTER TABLE ai_thread_drafts
    ADD CONSTRAINT ai_thread_drafts_email_account_id_fkey
    FOREIGN KEY (email_account_id) REFERENCES email_accounts (id) ON DELETE CASCADE NOT VALID;

ALTER TABLE email_delta_links
    ADD CONSTRAINT email_delta_links_email_id_fkey
    FOREIGN KEY (email_id) REFERENCES email_accounts (id) ON DELETE CASCADE NOT VALID;

ALTER TABLE email_history_ids
    ADD CONSTRAINT email_history_ids_email_id_fkey
    FOREIGN KEY (email_id) REFERENCES email_accounts (id) ON DELETE CASCADE NOT VALID;

ALTER TABLE email_message_map
    ADD CONSTRAINT email_message_map_email_id_fkey
    FOREIGN KEY (email_id) REFERENCES email_accounts (id) ON DELETE CASCADE NOT VALID;

ALTER TABLE warmup_pending_engagements
    ADD CONSTRAINT warmup_pending_engagements_email_account_id_fkey
    FOREIGN KEY (email_account_id) REFERENCES email_accounts (id) ON DELETE CASCADE NOT VALID;

-- Both sides: a warmup receipt names the mailbox that received the mail and the
-- one that sent it, and either being deleted makes the row meaningless.
ALTER TABLE warmup_received
    ADD CONSTRAINT warmup_received_email_account_id_fkey
    FOREIGN KEY (email_account_id) REFERENCES email_accounts (id) ON DELETE CASCADE NOT VALID;

ALTER TABLE warmup_received
    ADD CONSTRAINT warmup_received_sender_account_id_fkey
    FOREIGN KEY (sender_account_id) REFERENCES email_accounts (id) ON DELETE CASCADE NOT VALID;

ALTER TABLE warmup_tampering_events
    ADD CONSTRAINT warmup_tampering_events_email_account_id_fkey
    FOREIGN KEY (email_account_id) REFERENCES email_accounts (id) ON DELETE CASCADE NOT VALID;

-- decision_log is the fleet's operational history (what was placed where, and
-- why), not workspace data, and it is the one reference here worth keeping
-- after the mailbox goes. SET NULL keeps the record of the decision while
-- dropping the identity it was about.
UPDATE decision_log t SET mailbox_id = NULL
 WHERE t.mailbox_id IS NOT NULL
   AND NOT EXISTS (SELECT 1 FROM email_accounts a WHERE a.id = t.mailbox_id);
ALTER TABLE decision_log
    ADD CONSTRAINT decision_log_mailbox_id_fkey
    FOREIGN KEY (mailbox_id) REFERENCES email_accounts (id) ON DELETE SET NULL NOT VALID;

-- ---------- 2 and 3. the work that outlives the row ----------

CREATE TABLE public.mailbox_erasures (
    email_account_id uuid PRIMARY KEY,
    -- Deliberately NOT foreign keys. The whole purpose of this row is to
    -- outlive the mailbox, and a mailbox is also erased when its organization
    -- or its owner is hard-deleted, so a reference to either would cascade the
    -- work away in the one case that needs it most. They are labels for logs.
    organization_id uuid,
    user_id uuid NOT NULL,
    email text NOT NULL,
    provider text NOT NULL,

    -- The refresh token as it was stored, still sealed under
    -- CREDENTIALS_ENCRYPTION_KEY, copied here before email_accounts_oauth
    -- cascades away. Empty for SMTP/IMAP, which has no grant to revoke.
    refresh_token text DEFAULT ''::text NOT NULL,
    -- Every object the mailbox wrote lives under this one prefix, so the bytes
    -- go in one call rather than one per message.
    blob_prefix text DEFAULT ''::text NOT NULL,

    -- Half-done is recorded so a retry never repeats the half that worked.
    -- There is no "done" stamp: finishing deletes the row, because the row
    -- names the address the customer asked to have forgotten, and keeping it
    -- as a receipt would be keeping the thing being erased. The audit log is
    -- where the record of the deletion lives.
    token_revoked_at timestamp with time zone,
    blobs_erased_at timestamp with time zone,

    attempts integer DEFAULT 0 NOT NULL,
    last_error text DEFAULT ''::text NOT NULL,
    next_attempt_at timestamp with time zone DEFAULT now() NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL
);

COMMENT ON TABLE public.mailbox_erasures IS
    'Erasure still owed for a deleted mailbox: revoke its OAuth grant at the provider, and remove its message bodies from object storage. The row is deleted once both are done, so the table holds only outstanding work.';

-- The claim reads due work, oldest first, and nothing else.
CREATE INDEX idx_mailbox_erasures_due
    ON public.mailbox_erasures USING btree (next_attempt_at);

-- ---------- 4. make deleting a workspace possible at all ----------
--
-- HardDeleteOrganization is one DELETE against organizations, documented as
-- "relying on existing ON DELETE CASCADE FKs to clean up dependents". Four of
-- them have no delete action at all, so that DELETE raises a foreign key
-- violation for any workspace that has ever held a mailbox, a campaign, a
-- contact or a sequence, which is every workspace anyone would ask to delete.
-- A scheduled workspace deletion therefore ran its grace period, sent its
-- warning emails, and then failed every tick forever.
--
-- Each of these columns is nullable, so SET NULL would also satisfy the
-- constraint; it is the wrong answer. It would leave the workspace's campaigns
-- and contacts behind with no organization, which nothing lists and nothing
-- deletes. The rows belong to the workspace and go with it.
ALTER TABLE campaigns DROP CONSTRAINT IF EXISTS campaigns_organization_id_fkey;
ALTER TABLE campaigns
    ADD CONSTRAINT campaigns_organization_id_fkey
    FOREIGN KEY (organization_id) REFERENCES organizations (id) ON DELETE CASCADE NOT VALID;

ALTER TABLE contacts DROP CONSTRAINT IF EXISTS contacts_organization_id_fkey;
ALTER TABLE contacts
    ADD CONSTRAINT contacts_organization_id_fkey
    FOREIGN KEY (organization_id) REFERENCES organizations (id) ON DELETE CASCADE NOT VALID;

ALTER TABLE sequences DROP CONSTRAINT IF EXISTS sequences_organization_id_fkey;
ALTER TABLE sequences
    ADD CONSTRAINT sequences_organization_id_fkey
    FOREIGN KEY (organization_id) REFERENCES organizations (id) ON DELETE CASCADE NOT VALID;

ALTER TABLE email_accounts DROP CONSTRAINT IF EXISTS email_accounts_organization_id_fkey;
ALTER TABLE email_accounts
    ADD CONSTRAINT email_accounts_organization_id_fkey
    FOREIGN KEY (organization_id) REFERENCES organizations (id) ON DELETE CASCADE NOT VALID;

-- A session belongs to the person, not to the workspace it happens to be
-- looking at. Cascading here would sign someone out of an account they still
-- have, over a workspace they just left.
ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_current_organization_id_fkey;
ALTER TABLE sessions
    ADD CONSTRAINT sessions_current_organization_id_fkey
    FOREIGN KEY (current_organization_id) REFERENCES organizations (id) ON DELETE SET NULL NOT VALID;
