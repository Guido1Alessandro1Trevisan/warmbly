-- The pool spam score only ever went up, and it grew with volume rather than
-- with misbehaviour: +5 a placement, +10 a complaint, no denominator. A busy
-- healthy mailbox and a small struggling one land on the same number, so no
-- threshold can separate them, which is why no band ever read it (#491). The
-- rate bands judge the same events with a sample floor, and last_health_score
-- carries the severity they decided.
DROP TRIGGER IF EXISTS warmup_reputation_mirror ON public.warmup_pool_participants;

CREATE OR REPLACE FUNCTION public.warmup_reputation_mirror() RETURNS trigger
    LANGUAGE plpgsql
AS $$
DECLARE
    v_org   uuid;
    v_email text;
    worst   record;
BEGIN
    -- The organization join matters: when a workspace is deleted its mailboxes
    -- cascade while the organization row is already gone, and a mirror row
    -- written then would violate its own foreign key and abort the deletion.
    SELECT a.organization_id, lower(btrim(a.email))
      INTO v_org, v_email
      FROM public.email_accounts a
      JOIN public.organizations o ON o.id = a.organization_id
     WHERE a.id = NEW.email_account_id;
    IF v_org IS NULL THEN
        RETURN NULL;
    END IF;

    SELECT p.health_state, p.blocked_at, p.blocked_until, p.blocked_reason,
           p.last_health_score, p.last_health_reason
      INTO worst
      FROM public.warmup_pool_participants p
      JOIN public.email_accounts a ON a.id = p.email_account_id
     WHERE a.organization_id = v_org AND lower(btrim(a.email)) = v_email
     ORDER BY CASE p.health_state
                  WHEN 'blocked' THEN 5
                  WHEN 'quarantined' THEN 4
                  WHEN 'throttled' THEN 3
                  WHEN 'watch' THEN 2
                  WHEN 'healthy' THEN 1
                  ELSE 0
              END DESC,
              (p.health_state = 'blocked' AND p.blocked_until IS NULL) DESC,
              p.blocked_until DESC NULLS LAST
     LIMIT 1;

    -- A mailbox in good standing has no row: there is no longer a score that
    -- can outlive the sentence it was recorded alongside.
    IF worst.health_state IS NULL OR worst.health_state = 'healthy' THEN
        DELETE FROM public.warmup_reputation_ledger
         WHERE organization_id = v_org AND email = v_email;
        RETURN NULL;
    END IF;

    INSERT INTO public.warmup_reputation_ledger
        (organization_id, email, health_state, blocked_at, blocked_until, blocked_reason,
         last_health_score, last_health_reason, recorded_at, standing_until)
    VALUES
        (v_org, v_email, worst.health_state, worst.blocked_at, worst.blocked_until, worst.blocked_reason,
         worst.last_health_score, worst.last_health_reason, now(),
         CASE WHEN worst.health_state = 'blocked' AND worst.blocked_until IS NULL THEN NULL
              ELSE GREATEST(COALESCE(worst.blocked_until, now()), now())
         END)
    ON CONFLICT (organization_id, email) DO UPDATE SET
        health_state       = EXCLUDED.health_state,
        blocked_at         = EXCLUDED.blocked_at,
        blocked_until      = EXCLUDED.blocked_until,
        blocked_reason     = EXCLUDED.blocked_reason,
        last_health_score  = EXCLUDED.last_health_score,
        last_health_reason = EXCLUDED.last_health_reason,
        recorded_at        = EXCLUDED.recorded_at,
        standing_until     = EXCLUDED.standing_until;
    RETURN NULL;
END;
$$;

ALTER TABLE public.warmup_pool_participants DROP COLUMN IF EXISTS spam_score;
ALTER TABLE public.warmup_reputation_ledger DROP COLUMN IF EXISTS spam_score;

CREATE TRIGGER warmup_reputation_mirror
    AFTER INSERT OR UPDATE OF health_state, blocked_at, blocked_until, blocked_reason,
                              last_health_score, last_health_reason
    ON public.warmup_pool_participants
    FOR EACH ROW EXECUTE FUNCTION public.warmup_reputation_mirror();

-- A ledger row that carried a score and no sentence now carries nothing.
DELETE FROM public.warmup_reputation_ledger WHERE health_state = 'healthy';
