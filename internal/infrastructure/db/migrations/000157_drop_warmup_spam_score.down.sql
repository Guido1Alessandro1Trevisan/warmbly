-- Reverses 000157. The columns return at 0: the accumulated scores are gone,
-- as they are for any dropped column.
DROP TRIGGER IF EXISTS warmup_reputation_mirror ON public.warmup_pool_participants;

ALTER TABLE public.warmup_pool_participants
    ADD COLUMN IF NOT EXISTS spam_score integer DEFAULT 0 NOT NULL;
ALTER TABLE public.warmup_pool_participants
    ADD CONSTRAINT valid_spam_score CHECK (spam_score >= 0 AND spam_score <= 100);

ALTER TABLE public.warmup_reputation_ledger
    ADD COLUMN IF NOT EXISTS spam_score integer DEFAULT 0 NOT NULL;
ALTER TABLE public.warmup_reputation_ledger
    ADD CONSTRAINT warmup_reputation_ledger_spam_score_check CHECK (spam_score >= 0 AND spam_score <= 100);

CREATE OR REPLACE FUNCTION public.warmup_reputation_mirror() RETURNS trigger
    LANGUAGE plpgsql
AS $$
DECLARE
    v_org   uuid;
    v_email text;
    worst   record;
    v_score integer;
BEGIN
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

    SELECT COALESCE(MAX(p.spam_score), 0)
      INTO v_score
      FROM public.warmup_pool_participants p
      JOIN public.email_accounts a ON a.id = p.email_account_id
     WHERE a.organization_id = v_org AND lower(btrim(a.email)) = v_email;

    IF worst.health_state IS NULL OR (worst.health_state = 'healthy' AND v_score = 0) THEN
        DELETE FROM public.warmup_reputation_ledger
         WHERE organization_id = v_org AND email = v_email;
        RETURN NULL;
    END IF;

    INSERT INTO public.warmup_reputation_ledger
        (organization_id, email, spam_score, health_state, blocked_at, blocked_until, blocked_reason,
         last_health_score, last_health_reason, recorded_at, standing_until)
    VALUES
        (v_org, v_email, v_score, worst.health_state, worst.blocked_at, worst.blocked_until, worst.blocked_reason,
         worst.last_health_score, worst.last_health_reason, now(),
         CASE WHEN worst.health_state = 'blocked' AND worst.blocked_until IS NULL THEN NULL
              ELSE GREATEST(COALESCE(worst.blocked_until, now()), now())
         END)
    ON CONFLICT (organization_id, email) DO UPDATE SET
        spam_score         = EXCLUDED.spam_score,
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

CREATE TRIGGER warmup_reputation_mirror
    AFTER INSERT OR UPDATE OF spam_score, health_state, blocked_at, blocked_until, blocked_reason,
                              last_health_score, last_health_reason
    ON public.warmup_pool_participants
    FOR EACH ROW EXECUTE FUNCTION public.warmup_reputation_mirror();
