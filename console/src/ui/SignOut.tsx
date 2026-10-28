// Sign out.
//
// DELETE /sessions revokes the session server-side and clears the cookie; the
// console only asks for it. Then every cached query is reset rather than
// invalidated: invalidating would refetch /me, get a 401, and show the sign-in
// form — but the previous person's apps and accounts would still be sitting in
// the cache for whoever signs in next on this browser.

import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Button } from '@design';

import { api } from '@api/client';

import { markSignedOut } from '../auth/sso';

export function SignOut({ onSignedOut }: { onSignedOut?: () => void }) {
  const queries = useQueryClient();

  const signOut = useMutation({
    mutationFn: () => api.del<void>('/sessions'),
    onSuccess: () => {
      // The sign-in page shown next would otherwise start the only provider,
      // which still has them signed in, and undo this (R-172).
      markSignedOut();
      onSignedOut?.();
      void queries.resetQueries();
    },
  });

  return (
    <div style={{ display: 'flex', flexDirection: 'column', alignItems: 'flex-start', gap: 'var(--space-2)' }}>
      <Button variant="secondary" onClick={() => signOut.mutate()} disabled={signOut.isPending}>
        {signOut.isPending ? 'Signing out' : 'Sign out'}
      </Button>
      {signOut.isError && (
        <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
          Pando couldn’t sign you out: {signOut.error.message} Try again.
        </p>
      )}
    </div>
  );
}
