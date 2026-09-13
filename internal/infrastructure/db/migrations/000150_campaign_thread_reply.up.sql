-- Campaign follow-ups are replies, not new cold emails. Every step after the
-- contact's first went out with no In-Reply-To, no References and, on Gmail, no
-- threadId, so each one opened its own conversation and a recipient read the
-- nudge as a second stranger (issue #472).
--
-- Two columns make threading possible:

-- The per-step switch. Default true because replying in the thread is what the
-- dashboard, the docs and every other sequencer already promise; a step that
-- should deliberately open a fresh conversation turns it off.
ALTER TABLE sequences
    ADD COLUMN IF NOT EXISTS thread_reply boolean NOT NULL DEFAULT true;

-- The provider-side conversation handle the worker reports after a send. Gmail
-- only appends to an existing thread when the outbound message carries its
-- threadId; matching Subject and In-Reply-To are not enough. Empty for every
-- provider that has no such handle (SMTP, Graph), which is why it is a plain
-- text default '' rather than nullable.
ALTER TABLE tasks
    ADD COLUMN IF NOT EXISTS thread_id text NOT NULL DEFAULT '';

-- Existing steps keep the behaviour the dashboard and the docs described until
-- now: "a follow-up threads on the previous step's subject, and changing that
-- subject starts a new thread instead". A step whose subject differs from the
-- email step before it was written to open a fresh conversation, so it keeps
-- doing that; everything else replies in the thread, which is what it always
-- claimed to do and never did.
WITH ordered AS (
    SELECT id,
           btrim(subject) AS subject,
           LAG(btrim(subject)) OVER (
               PARTITION BY campaign_id ORDER BY position, created_at
           ) AS prev_subject
    FROM sequences
    WHERE kind = 'email'
)
UPDATE sequences s
SET thread_reply = false
FROM ordered o
WHERE o.id = s.id
  AND o.prev_subject IS NOT NULL
  AND o.subject <> ''
  AND o.subject <> o.prev_subject;
