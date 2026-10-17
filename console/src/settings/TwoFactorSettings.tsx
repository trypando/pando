// Two-factor authentication, in your settings (R-405).
//
// Local accounts only: Settings draws this when GET /me has `two_factor`, which
// it has for nobody else. Setting it up, replacing the authenticator, making
// new recovery codes and turning it off are each a dialog, built from the same
// pieces as the page offered after signing in (auth/TwoFactorSetup). Each asks
// for the password again when the session is old enough that the server does.

import { useEffect, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Dialog } from '@design';

import { api } from '@api/client';
import type { TwoFactorStatus } from '../app/principal';
import { withRemedy } from '../auth/Login';
import {
  CurrentPassword,
  RecoveryCodes,
  SavedCodes,
  ScanCode,
  useEnrollment,
  withPassword,
} from '../auth/TwoFactorSetup';

type Open = 'enroll' | 'codes' | 'off' | null;

export function TwoFactorSettings({ status, username }: { status: TwoFactorStatus; username?: string }) {
  const [open, setOpen] = useState<Open>(null);
  const close = () => setOpen(null);

  return (
    <section>
      <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-3)' }}>Two-factor authentication</h4>
      <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: '0 0 var(--space-3)' }}>
        {status.enabled
          ? `On. Signing in asks for a code from your authenticator app. ${remaining(status.recovery_codes_remaining)}`
          : 'Off. Signing in asks only for your password.'}
      </p>
      <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-3)' }}>
        {status.enabled ? (
          <>
            <Button variant="secondary" onClick={() => setOpen('enroll')}>
              Replace authenticator
            </Button>
            <Button variant="secondary" onClick={() => setOpen('codes')}>
              New recovery codes
            </Button>
            {!status.required && (
              <Button variant="secondary" onClick={() => setOpen('off')}>
                Turn off
              </Button>
            )}
          </>
        ) : (
          <Button variant="secondary" onClick={() => setOpen('enroll')}>
            Set up
          </Button>
        )}
      </div>
      {status.required && status.enabled && (
        <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 'var(--space-2) 0 0' }}>
          This installation requires two-factor authentication for local accounts, so it can’t be turned off.
        </p>
      )}
      {open === 'enroll' && (
        <EnrollDialog
          replacing={status.enabled}
          passwordRequired={status.password_required}
          username={username}
          onClose={close}
        />
      )}
      {open === 'codes' && (
        <NewCodesDialog passwordRequired={status.password_required} username={username} onClose={close} />
      )}
      {open === 'off' && <TurnOffDialog passwordRequired={status.password_required} onClose={close} />}
    </section>
  );
}

/** How many recovery codes are left, and what to do when that is few. */
export function remaining(n: number): string {
  if (n === 0) return 'No recovery codes left. Make new ones in case you lose your phone.';
  if (n === 1) return '1 recovery code left. Make new ones in case you lose your phone.';
  if (n <= 3) return `${n} recovery codes left. Make new ones in case you lose your phone.`;
  return `${n} recovery codes left.`;
}

/** Set up, or replace, the authenticator. */
function EnrollDialog({
  replacing,
  passwordRequired,
  username,
  onClose,
}: {
  replacing: boolean;
  passwordRequired: boolean;
  username?: string;
  onClose: () => void;
}) {
  const enroll = useEnrollment();
  const queries = useQueryClient();
  const { begin } = enroll;

  // Nothing to ask first when the session is recent: straight to the code.
  const asked = useRef(false);
  useEffect(() => {
    if (passwordRequired || asked.current) return;
    asked.current = true;
    begin.mutate();
  }, [passwordRequired, begin]);

  if (enroll.step === 'codes' && enroll.confirm.data) {
    return <CodesShown codes={enroll.confirm.data.recovery_codes} username={username} onDone={onClose} />;
  }

  if (enroll.step === 'scan' && enroll.begin.data) {
    return (
      <Dialog
        open
        title="Scan this code"
        description={
          replacing
            ? 'Scan it with the new authenticator app. The current one keeps working until you enter a code from this one.'
            : 'Open your authenticator app, add an account, and scan the code.'
        }
        onClose={onClose}
        footer={
          <>
            <Button variant="ghost" onClick={onClose}>
              Cancel
            </Button>
            <Button
              variant="primary"
              disabled={enroll.confirm.isPending || enroll.code.length !== 6}
              onClick={() =>
                enroll.confirm.mutate(undefined, {
                  onSuccess: () => void queries.invalidateQueries({ queryKey: ['me'] }),
                })
              }
            >
              {enroll.confirm.isPending ? 'Turning on' : 'Turn on'}
            </Button>
          </>
        }
      >
        <ScanCode
          enrollment={enroll.begin.data}
          code={enroll.code}
          onCode={enroll.setCode}
          error={enroll.confirm.isError ? withRemedy(enroll.confirm.error) : undefined}
        />
      </Dialog>
    );
  }

  return (
    <Dialog
      open
      title={replacing ? 'Replace your authenticator' : 'Set up two-factor authentication'}
      description="Signing in will ask for a code from an authenticator app as well as your password."
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={begin.isPending || (passwordRequired && enroll.password === '')}
            onClick={() => begin.mutate()}
          >
            {begin.isPending ? 'Setting up' : 'Continue'}
          </Button>
        </>
      }
    >
      {passwordRequired ? (
        <CurrentPassword
          value={enroll.password}
          onChange={enroll.setPassword}
          error={begin.isError ? withRemedy(begin.error) : undefined}
        />
      ) : (
        begin.isError && <Banner tone="failed">{withRemedy(begin.error)}</Banner>
      )}
    </Dialog>
  );
}

/** New recovery codes, which end the old ones. */
function NewCodesDialog({
  passwordRequired,
  username,
  onClose,
}: {
  passwordRequired: boolean;
  username?: string;
  onClose: () => void;
}) {
  const [password, setPassword] = useState('');
  const queries = useQueryClient();
  const make = useMutation({
    mutationFn: () =>
      api.post<{ recovery_codes: string[] }>('/me/two-factor/recovery-codes', withPassword(password)),
    onSuccess: () => void queries.invalidateQueries({ queryKey: ['me'] }),
  });

  if (make.data) {
    return <CodesShown codes={make.data.recovery_codes} username={username} onDone={onClose} />;
  }

  return (
    <Dialog
      open
      title="Make new recovery codes"
      description="Your current recovery codes stop working."
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={make.isPending || (passwordRequired && password === '')}
            onClick={() => make.mutate()}
          >
            {make.isPending ? 'Making codes' : 'Make new codes'}
          </Button>
        </>
      }
    >
      <PasswordOrError
        passwordRequired={passwordRequired}
        password={password}
        onPassword={(v) => {
          if (make.isError) make.reset();
          setPassword(v);
        }}
        error={make.isError ? withRemedy(make.error) : undefined}
      />
    </Dialog>
  );
}

/** Turn it off: password sign-in alone again. */
function TurnOffDialog({ passwordRequired, onClose }: { passwordRequired: boolean; onClose: () => void }) {
  const [password, setPassword] = useState('');
  const queries = useQueryClient();
  const off = useMutation({
    mutationFn: () => api.del<void>('/me/two-factor', withPassword(password)),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['me'] });
      onClose();
    },
  });

  return (
    <Dialog
      open
      title="Turn off two-factor authentication"
      description="Signing in will ask only for your password. Your authenticator app’s codes and your recovery codes stop working."
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="destructive"
            disabled={off.isPending || (passwordRequired && password === '')}
            onClick={() => off.mutate()}
          >
            {off.isPending ? 'Turning off' : 'Turn off'}
          </Button>
        </>
      }
    >
      <PasswordOrError
        passwordRequired={passwordRequired}
        password={password}
        onPassword={(v) => {
          if (off.isError) off.reset();
          setPassword(v);
        }}
        error={off.isError ? withRemedy(off.error) : undefined}
      />
    </Dialog>
  );
}

/** The password field when the server asks for it, carrying the refusal; the
 *  refusal on its own otherwise. */
function PasswordOrError({
  passwordRequired,
  password,
  onPassword,
  error,
}: {
  passwordRequired: boolean;
  password: string;
  onPassword: (v: string) => void;
  error?: string;
}) {
  if (passwordRequired) return <CurrentPassword value={password} onChange={onPassword} error={error} />;
  return error ? <Banner tone="failed">{error}</Banner> : null;
}

/**
 * Recovery codes, shown once. Done waits for the person to say they saved
 * them, and so does closing: there is no second look.
 */
function CodesShown({ codes, username, onDone }: { codes: string[]; username?: string; onDone: () => void }) {
  const [saved, setSaved] = useState(false);
  return (
    <Dialog
      open
      title="Save your recovery codes"
      description="If you lose your phone, sign in with one of these instead. Each works once, and Pando doesn’t show them again."
      onClose={() => {
        if (saved) onDone();
      }}
      footer={
        <Button variant="primary" disabled={!saved} onClick={onDone}>
          Done
        </Button>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        <RecoveryCodes codes={codes} username={username} />
        <SavedCodes saved={saved} onSaved={setSaved} />
      </div>
    </Dialog>
  );
}
