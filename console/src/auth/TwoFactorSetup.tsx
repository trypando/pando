// Two-factor authentication for a local account (R-405).
//
// A local account signs in with a password Pando stores; setting this up adds
// a code from an authenticator app to every password sign-in. An account from
// an identity provider never sees any of it: its second factor, if it has one,
// is the provider's.
//
// The page here is the one offered after signing in — once, and skippable —
// or, where host policy requires it, the only page there is until it is done:
// the server answers everything else POLICY_TWO_FACTOR_REQUIRED, and App puts
// this in front of the console for the same reason ChangePassword is (R-046).
// Settings sets it up, replaces it and turns it off with the same pieces, in
// dialogs (settings/TwoFactorSettings).
//
// The recovery codes are shown once, when it is turned on. Continuing is held
// until the person says they saved them, because there is no second look: the
// server keeps only their digests.

import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Checkbox, Input } from '@design';

import { api } from '@api/client';
import { SignOut } from '../ui/SignOut';
import { Frame, digitsOnly, messageOf, withRemedy } from './Login';
import { copyText } from '../ui/clipboard';

/** A pending authenticator: what the app is given to add the account. */
export interface Enrollment {
  secret: string;
  uri: string;
  qr_svg: string;
}

interface RecoveryCodesResponse {
  recovery_codes: string[];
}

/** The current password, sent only when the session is old enough that the
 *  server asks for it again (`password_required`). */
export function withPassword(password: string): { current_password?: string } {
  return password === '' ? {} : { current_password: password };
}

/** Setting up or replacing the authenticator: ask for a secret, then confirm
 *  it with the first code the app shows. */
export function useEnrollment() {
  const [password, setPassword] = useState('');
  const [code, setCode] = useState('');

  const begin = useMutation({
    mutationFn: () => api.post<Enrollment>('/me/two-factor/totp', withPassword(password)),
  });
  const confirm = useMutation({
    mutationFn: () => api.post<RecoveryCodesResponse>('/me/two-factor/totp/confirm', { code }),
  });

  let step: 'start' | 'scan' | 'codes' = 'start';
  if (confirm.data) step = 'codes';
  else if (begin.data) step = 'scan';

  return {
    step,
    password,
    code,
    begin,
    confirm,
    // A rejection belongs to the value that caused it (see ChangePassword).
    setPassword: (v: string) => {
      if (begin.isError) begin.reset();
      setPassword(v);
    },
    setCode: (v: string) => {
      if (confirm.isError) confirm.reset();
      setCode(digitsOnly(v));
    },
  };
}

/**
 * Where two-factor authentication is required and cannot be set up at this
 * address: an app's own hostname, where the app's script could read the key
 * and the recovery codes (R-405, issue #78). Sends the person to Pando's own
 * address, which the server knows when PANDO_SERVER_EXTERNAL_URL is set; when
 * it is not, says where that is in words, since the person reached the console
 * there to have an account at all.
 */
export function TwoFactorElsewhere({ consoleURL }: Readonly<{ consoleURL?: string }>) {
  return (
    <Frame
      heading="Set up two-factor authentication"
      lede="This installation requires two-factor authentication. Set it up in Pando's console, then come back to this app."
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        {consoleURL ? (
          <Button variant="primary" fullWidth onClick={() => window.location.assign(consoleURL)}>
            Open Pando
          </Button>
        ) : (
          <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
            Open Pando at the address you use for its console, not this app&rsquo;s, and sign in there. It asks you
            to set up an authenticator app first.
          </p>
        )}
        <SignOut />
      </div>
    </Frame>
  );
}

/**
 * The page offered after signing in, or required before anything else.
 *
 * `onStarted` and `onDone` let App keep this page up once it has begun: GET /me
 * stops saying "set it up" the moment it is on, and without them the recovery
 * codes would be taken off the screen by the next refetch of it.
 */
export function TwoFactorSetup({
  required = false,
  passwordRequired = false,
  username,
  onStarted,
  onDone,
}: Readonly<{
  required?: boolean;
  passwordRequired?: boolean;
  username?: string;
  onStarted?: () => void;
  onDone?: () => void;
}>) {
  const enroll = useEnrollment();
  const [saved, setSaved] = useState(false);
  const queries = useQueryClient();

  // GET /me first, then let go: the answer from before still says to set it
  // up, and letting go on it would draw this page again from the start.
  const finish = () => {
    void queries.invalidateQueries({ queryKey: ['me'] }).then(() => onDone?.());
  };

  const skip = useMutation({
    mutationFn: () => api.post<void>('/me/two-factor/skip'),
    onSuccess: finish,
  });

  if (enroll.step === 'codes' && enroll.confirm.data) {
    return (
      <Frame
        heading="Save your recovery codes"
        lede="Two-factor authentication is on. If you lose your phone, sign in with one of these codes instead. Each one works once, and Pando doesn’t show them again."
      >
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
          <RecoveryCodes codes={enroll.confirm.data.recovery_codes} username={username} />
          <SavedCodes saved={saved} onSaved={setSaved} />
          <Button variant="primary" fullWidth disabled={!saved} onClick={finish}>
            Continue
          </Button>
        </div>
      </Frame>
    );
  }

  if (enroll.step === 'scan' && enroll.begin.data) {
    return (
      <Frame
        heading="Scan this code"
        lede="Open your authenticator app, add an account, and scan the code. Then enter the 6-digit code the app shows."
      >
        <form
          onSubmit={(e) => {
            e.preventDefault();
            enroll.confirm.mutate();
          }}
          style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}
        >
          <ScanCode
            enrollment={enroll.begin.data}
            code={enroll.code}
            onCode={enroll.setCode}
            error={enroll.confirm.isError ? withRemedy(enroll.confirm.error) : undefined}
          />
          <Button
            type="submit"
            variant="primary"
            fullWidth
            disabled={enroll.confirm.isPending || enroll.code.length !== 6}
          >
            {enroll.confirm.isPending ? 'Turning on' : 'Turn on'}
          </Button>
        </form>
      </Frame>
    );
  }

  return (
    <Frame
      heading={required ? 'Set up two-factor authentication' : 'Add two-factor authentication'}
      lede={
        required
          ? 'This installation requires two-factor authentication for local accounts. Set it up to continue. You need an authenticator app on your phone or in your password manager.'
          : 'Signing in can ask for a code from an authenticator app as well as your password, so a stolen password isn’t enough to sign in as you.'
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          enroll.begin.mutate(undefined, { onSuccess: () => onStarted?.() });
        }}
        style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}
      >
        {username !== undefined && (
          <Input label="Username" value={username} readOnly autoComplete="username" />
        )}
        {passwordRequired && (
          <CurrentPassword
            value={enroll.password}
            onChange={enroll.setPassword}
            error={enroll.begin.isError ? withRemedy(enroll.begin.error) : undefined}
          />
        )}
        {!passwordRequired && enroll.begin.isError && <Banner tone="failed">{withRemedy(enroll.begin.error)}</Banner>}
        {skip.isError && <Banner tone="failed">{messageOf(skip.error)}</Banner>}
        <Button
          type="submit"
          variant="primary"
          fullWidth
          disabled={enroll.begin.isPending || (passwordRequired && enroll.password === '')}
        >
          {enroll.begin.isPending ? 'Setting up' : 'Set up'}
        </Button>
        {required ? (
          <SignOut />
        ) : (
          <Button type="button" variant="ghost" fullWidth disabled={skip.isPending} onClick={() => skip.mutate()}>
            Not now
          </Button>
        )}
      </form>
    </Frame>
  );
}

/** The person saying they kept the codes, which is what continuing waits for. */
export function SavedCodes({ saved, onSaved }: Readonly<{ saved: boolean; onSaved: (saved: boolean) => void }>) {
  return (
    <Checkbox
      label="I saved these recovery codes"
      description="Somewhere other than this browser, such as a password manager."
      checked={saved}
      onChange={(e) => onSaved(e.target.checked)}
    />
  );
}

/** The password again, for a session old enough that the server asks. */
export function CurrentPassword({
  value,
  onChange,
  error,
}: Readonly<{
  value: string;
  onChange: (v: string) => void;
  error?: string;
}>) {
  return (
    <Input
      label="Current password"
      type="password"
      value={value}
      autoComplete="current-password"
      autoFocus
      helper="You signed in a while ago, so Pando asks for it again."
      error={error}
      onChange={(e) => onChange(e.target.value)}
    />
  );
}

/**
 * The QR code an authenticator app scans, the key for one that cannot scan,
 * and the field for the first code it shows.
 *
 * The QR code is the server's SVG, dark on its own light ground and quiet zone,
 * so it scans the same in the dark theme.
 */
export function ScanCode({
  enrollment,
  code,
  onCode,
  error,
}: Readonly<{
  enrollment: Enrollment;
  code: string;
  onCode: (code: string) => void;
  error?: string;
}>) {
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      <img
        src={`data:image/svg+xml;charset=utf-8,${encodeURIComponent(enrollment.qr_svg)}`}
        alt="QR code to scan with your authenticator app"
        style={{
          width: '12rem',
          height: '12rem',
          border: 'var(--border-width) solid var(--rule)',
          borderRadius: 'var(--radius-md)',
        }}
      />
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
        <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
          Can’t scan it? Enter this key in the app instead.
        </span>
        <code
          style={{
            font: 'var(--type-code)',
            color: 'var(--ink)',
            overflowWrap: 'anywhere',
            userSelect: 'all',
          }}
        >
          {groupKey(enrollment.secret)}
        </code>
      </div>
      <Input
        label="Code from the app"
        value={code}
        mono
        inputMode="numeric"
        autoComplete="one-time-code"
        error={error}
        onChange={(e) => onCode(e.target.value)}
      />
    </div>
  );
}

/** A base32 key in groups of four, the way authenticator apps print them. */
export function groupKey(secret: string): string {
  return (secret.match(/.{1,4}/g) ?? []).join(' ');
}

/** The recovery codes as a downloadable text file. */
export function recoveryCodesText(codes: string[], username?: string, host = window.location.host): string {
  const who = username ? ` for ${username}` : '';
  return [
    `Pando recovery codes${who} at ${host}`,
    'Each code signs you in once, in place of a code from your authenticator app.',
    '',
    ...codes,
    '',
  ].join('\n');
}

/** The codes, once, with ways to keep them. */
export function RecoveryCodes({ codes, username }: Readonly<{ codes: string[]; username?: string }>) {
  const [copied, setCopied] = useState<'yes' | 'no' | null>(null);

  async function copy() {
    if (await copyText(codes.join('\n'))) {
      setCopied('yes');
      window.setTimeout(() => setCopied(null), 2_000);
    } else {
      setCopied('no');
    }
  }

  function download() {
    const url = URL.createObjectURL(new Blob([recoveryCodesText(codes, username)], { type: 'text/plain' }));
    const link = document.createElement('a');
    link.href = url;
    link.download = 'pando-recovery-codes.txt';
    link.click();
    URL.revokeObjectURL(url);
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      <ol
        aria-label="Recovery codes"
        style={{
          display: 'grid',
          gridTemplateColumns: 'repeat(2, max-content)',
          gap: 'var(--space-2) var(--space-6)',
          margin: 0,
          padding: 'var(--space-4)',
          listStyle: 'none',
          background: 'var(--paper-sunken)',
          border: 'var(--border-width) solid var(--rule)',
          borderRadius: 'var(--radius-md)',
          font: 'var(--type-code)',
          color: 'var(--ink)',
          userSelect: 'all',
        }}
      >
        {codes.map((c) => (
          <li key={c}>{c}</li>
        ))}
      </ol>
      <div style={{ display: 'flex', gap: 'var(--space-3)' }}>
        <Button type="button" variant="secondary" onClick={() => void copy()}>
          {copied === 'yes' ? 'Copied' : 'Copy'}
        </Button>
        <Button type="button" variant="secondary" onClick={download}>
          Download
        </Button>
      </div>
      {copied === 'no' && (
        <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
          Your browser blocked copying. Select the codes above and copy them, or download them.
        </span>
      )}
    </div>
  );
}
