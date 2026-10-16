-- Whether an account's email was last set by the account's own holder (R-045).
--
-- Linking by email trusts that exactly one active account has the address a
-- provider vouches for. An email the holder typed in themselves vouches for
-- nothing: anyone could set theirs to a colleague's and receive that
-- colleague's first sign-in through a provider that links by email. So linking
-- reads only an email an administrator, the installer or a provider set.
ALTER TABLE users ADD COLUMN email_set_by_self boolean NOT NULL DEFAULT false;

-- Before this, a local account's holder could set their own email and nothing
-- recorded that they had. The audit log is the record: the last event that set
-- the email says who set it. A local account's email is trusted only when that
-- event was someone else's — an administrator creating or editing the account.
-- One the holder set, or whose event is gone (archived or never written), is
-- treated as the holder's, which costs at most an explicit link where an email
-- link would have done. A provider's accounts take their email from the
-- provider and stay trusted.
UPDATE users u
SET email_set_by_self = true
WHERE u.adapter_id = 'idp_local'
  AND u.email IS NOT NULL
  AND NOT coalesce((
    SELECT coalesce(nullif(e.on_behalf_of, ''), e.principal_id) IS DISTINCT FROM u.id
    FROM audit_events e
    WHERE e.target_kind = 'user' AND e.target_id = u.id
      AND (e.action = 'user.create' OR (e.action = 'user.update' AND e.detail ? 'email'))
    ORDER BY e.occurred_at DESC, e.id DESC
    LIMIT 1
  ), false);
