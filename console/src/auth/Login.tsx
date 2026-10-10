// Sign in, and on a new installation, set up.
//
// The console had no login screen at all: `/login` rendered the launcher, which
// rendered a link to `/login`. This is that screen.
//
// A new installation has no account, and nothing to sign in with. It used to
// generate an administrator password and print it to the server log; then the
// first person to reach this page created the administrator account. Now that
// person also needs the one-time setup token Pando prints to its log (R-046,
// issue #130), so the administrator is somebody who can read Pando's log, with
// a password they chose — and the server refuses a second attempt the moment
// one account exists, so there is exactly one administrator made this way.
//
// An external identity provider begins with a redirect, so each one the
// installation has turned on is a button here rather than a second page —
// providers are alternatives to this form, not alternatives to signing in
// (issue #51). Host policy may turn the form itself off, leaving the buttons.

import { useEffect, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Input, Logo, Skeleton } from '@design';

import { api, RequestFailed } from '@api/client';

import { FieldSkeleton, HeadingSkeleton, LineSkeleton, Loading } from '../ui/Loading';
import { TopoMap } from '../ui/TopoBackground';
import { signInInstead } from './passcode';
import { returnTo } from './return-to';
import { failedSignIn, providerStart } from './sso';
import type { SignInOptions } from './sso';

export function Login() {
  // Why the setup form was taken away, when somebody else finished first.
  const [taken, setTaken] = useState<string>();

  const setup = useQuery({
    queryKey: ['setup'],
    queryFn: () => api.get<{ needed: boolean }>('/setup'),
  });

  // Not a form yet: the page does not know which form it is, and showing the
  // sign-in form for a moment on a new installation invites typing into it. So
  // the outline both forms share — a heading, two labeled fields, a button —
  // with nothing in it to type into.
  if (setup.isPending) {
    return (
      <Frame heading={<HeadingSkeleton width="14ch" />}>
        <Loading gap="var(--space-4)">
          {[0, 1].map((n) => (
            <div key={n} style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
              <LineSkeleton width="10ch" font="var(--type-label)" />
              <FieldSkeleton />
            </div>
          ))}
          <Skeleton height="var(--control-console)" />
        </Loading>
      </Frame>
    );
  }

  // A failure to ask falls back to signing in, which is right on every
  // installation but a new one — and on a new one, signing in says why not.
  if (setup.data?.needed) {
    return (
      <Setup
        onTaken={async (message) => {
          const again = await setup.refetch();
          if (again.data?.needed === false) {
            setTaken(message);
            return true;
          }
          return false;
        }}
      />
    );
  }

  return <SignIn notice={taken} />;
}

function SignIn({ notice }: { notice?: string }) {
  const [username, setUsername] = useState('');
  const [password, setPassword] = useState('');
  const queries = useQueryClient();

  // The ways in this installation offers. Asked, not assumed: a failure to
  // ask falls back to the password form, which is what every installation
  // had before providers existed.
  const options = useQuery({
    queryKey: ['sign-in-options'],
    queryFn: () => api.get<SignInOptions>('/auth/options'),
  });
  const providers = options.data?.providers ?? [];
  const passwords = options.data?.password_sign_in ?? true;

  // Why a provider sign-in came back here, in the server's words.
  const failedID = failedSignIn(window.location.search);
  const failed = useQuery({
    queryKey: ['sign-in-failure', failedID],
    queryFn: () => api.get<{ message: string; remedy?: string }>(`/auth/failures/${failedID}`),
    enabled: failedID !== null,
    retry: false,
  });

  const signIn = useMutation({
    mutationFn: () => api.post<unknown>('/sessions', { username, password }),
    onSuccess: () => {
      // Somewhere to go back to, when the proxy sent them here (R-023). This
      // page is reachable on an app's own hostname at /.pando/login, and
      // without this the person who was trying to open an app signs in and
      // lands on a page of tiles, one click from where they already were.
      const next = returnTo(window.location.search, window.location.href);
      if (next) {
        window.location.assign(next);
        return;
      }

      // Otherwise the cookie is set and everything downstream reads GET /me.
      // Invalidating rather than navigating keeps this a single-page flow and
      // means the first thing the person sees is their own apps.
      void queries.invalidateQueries();
    },
  });

  // A rejection belongs to the value that caused it, and stops applying the
  // moment that value changes.
  const edit = (set: (v: string) => void) => (value: string) => {
    if (signIn.isError) signIn.reset();
    set(value);
  };

  return (
    <Frame>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          signIn.mutate();
        }}
        style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}
      >
        {notice && <Banner tone="info">{notice}</Banner>}
        {failed.data && (
          <Banner tone="failed">
            {failed.data.remedy ? `${failed.data.message} ${failed.data.remedy}` : failed.data.message}
          </Banner>
        )}
        {providers.map((p, i) => (
          // One primary action per view: the first provider when password
          // sign-in is off, the password form's button otherwise.
          <Button
            key={p.id}
            type="button"
            variant={!passwords && i === 0 ? 'primary' : 'secondary'}
            fullWidth
            onClick={() => window.location.assign(providerStart(p.id, window.location.search, window.location.href))}
          >
            {`Sign in with ${p.name}`}
          </Button>
        ))}
        {passwords && providers.length > 0 && (
          <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
            Or sign in with a Pando username and password.
          </p>
        )}
        {passwords && (
          <>
            <Input
              label="Username"
              value={username}
              autoComplete="username"
              autoFocus={providers.length === 0}
              onChange={(e) => edit(setUsername)(e.target.value)}
            />
            <Input
              label="Password"
              type="password"
              value={password}
              autoComplete="current-password"
              onChange={(e) => edit(setPassword)(e.target.value)}
              // The server's message, shown as written. It is held to the R-105
              // standard, and paraphrasing it here would undo that in the UI layer.
              error={signIn.isError ? messageOf(signIn.error) : undefined}
            />
            <Button
              type="submit"
              variant="primary"
              fullWidth
              disabled={signIn.isPending || username === '' || password === ''}
            >
              {signIn.isPending ? 'Signing in' : 'Sign in'}
            </Button>
          </>
        )}
      </form>
    </Frame>
  );
}

/**
 * The passcode page, for an app shared with everyone behind a passcode.
 *
 * The proxy sends a visitor who has not entered it here, the same way it sends
 * one to sign in for a private app (R-023) — same address, `?passcode=<app>`
 * added — and the form takes the sign-in form's place in the same frame. The
 * right passcode sets a cookie for the app, and the visitor goes back to where
 * they were headed. Nobody signs in: this is still the anonymous grant (R-075),
 * with one thing asked first.
 *
 * Somebody with an account may be able to open the app as themselves, so the
 * sign-in form is one quiet step away, keeping where they were going.
 */
export function Passcode({ appID, signedIn }: { appID: string; signedIn: boolean }) {
  const [passcode, setPasscode] = useState('');

  const app = useQuery({
    queryKey: ['passcode', appID],
    queryFn: () => api.get<{ app_id: string; name: string }>(`/apps/${appID}/passcode`),
    retry: false,
  });

  const enter = useMutation({
    mutationFn: () => api.post<void>(`/apps/${appID}/passcode`, { passcode }),
    // Where the proxy said they were going, checked like sign-in's (R-172),
    // or the app's own front page — this page is on its hostname.
    onSuccess: () => window.location.assign(returnTo(window.location.search, window.location.href) ?? '/'),
  });

  const signIn = signedIn ? null : (
    <Button
      variant="ghost"
      fullWidth
      onClick={() => window.location.assign(signInInstead(window.location.pathname, window.location.search))}
    >
      Sign in instead
    </Button>
  );

  if (app.isPending) {
    return (
      <Frame heading={<HeadingSkeleton width="16ch" />}>
        <Loading gap="var(--space-4)">
          <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
            <LineSkeleton width="8ch" font="var(--type-label)" />
            <FieldSkeleton />
          </div>
          <Skeleton height="var(--control-console)" />
        </Loading>
      </Frame>
    );
  }

  if (app.isError) {
    const missing = app.error instanceof RequestFailed && app.error.status === 404;
    return (
      <Frame
        heading={missing ? 'No passcode needed' : 'Enter the passcode'}
        lede={
          missing
            ? 'This app doesn’t ask for a passcode. If it was shared with you, sign in to open it.'
            : withRemedy(app.error)
        }
      >
        {signIn}
      </Frame>
    );
  }

  const edit = (value: string) => {
    if (enter.isError) enter.reset();
    setPasscode(value);
  };

  return (
    <Frame heading="Enter the passcode" lede={`${app.data.name} asks for a passcode. Whoever shared it with you has it.`}>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          enter.mutate();
        }}
        style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}
      >
        <Input
          label="Passcode"
          type="password"
          value={passcode}
          autoComplete="off"
          autoFocus
          onChange={(e) => edit(e.target.value)}
          // The server's message as written: a wrong passcode and too many
          // tries each say what to do next (R-105).
          error={enter.isError ? withRemedy(enter.error) : undefined}
        />
        <Button type="submit" variant="primary" fullWidth disabled={enter.isPending || passcode === ''}>
          {enter.isPending ? 'Checking' : 'Continue'}
        </Button>
        {signIn}
      </form>
    </Frame>
  );
}

/**
 * A new installation's first account.
 *
 * It needs the setup token Pando printed to its log (R-046, issue #130), so
 * that being first to this page is not enough to become the administrator.
 *
 * `onTaken` is asked whenever the server refuses, and answers whether the
 * refusal was somebody else finishing setup first. A refusal is otherwise
 * about this form — a wrong token, a short password, an unusable username —
 * and stays on it.
 */
function Setup({ onTaken }: { onTaken: (message: string) => Promise<boolean> }) {
  const [token, setToken] = useState('');
  const [username, setUsername] = useState('admin');
  const [displayName, setDisplayName] = useState('');
  const [password, setPassword] = useState('');
  const [confirm, setConfirm] = useState('');
  const queries = useQueryClient();

  const mismatch = confirm !== '' && password !== confirm;

  const create = useMutation({
    mutationFn: () =>
      api.post<unknown>('/setup', { setup_token: token, username, display_name: displayName, password }),
    // The server set the session cookie; everything downstream reads GET /me,
    // exactly as after signing in.
    onSuccess: () => void queries.invalidateQueries(),
    onError: (e) => void onTaken(withRemedy(e)),
  });

  const edit = (set: (v: string) => void) => (value: string) => {
    if (create.isError) create.reset();
    set(value);
  };

  // A refused token is shown on the token field, with where to find it; every
  // other refusal on the password field, as before.
  const tokenRefused =
    create.isError && create.error instanceof RequestFailed && create.error.code === 'AUTH_INVALID';

  return (
    <Frame
      heading="Set up Pando"
      lede="This installation has no accounts yet. Create the administrator account with the setup token Pando printed to its log."
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          create.mutate();
        }}
        style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}
      >
        <Input
          label="Setup token"
          value={token}
          autoComplete="off"
          spellCheck={false}
          autoFocus
          helper="Pando printed it to its log when it started, as setup_token. With Docker Compose, run docker compose logs pando."
          error={tokenRefused ? withRemedy(create.error) : undefined}
          onChange={(e) => edit(setToken)(e.target.value.trim())}
        />
        <Input
          label="Username"
          value={username}
          autoComplete="username"
          onChange={(e) => edit(setUsername)(e.target.value)}
        />
        <Input
          label="Name"
          value={displayName}
          autoComplete="name"
          helper="Optional. Shown in the console and in the audit log."
          onChange={(e) => edit(setDisplayName)(e.target.value)}
        />
        <Input
          label="Password"
          type="password"
          value={password}
          autoComplete="new-password"
          helper="At least 10 characters."
          // The server's refusal, on the field it is most often about (see
          // ChangePassword). A username it cannot use says so in its own words.
          error={!mismatch && create.isError && !tokenRefused ? messageOf(create.error) : undefined}
          onChange={(e) => edit(setPassword)(e.target.value)}
        />
        <Input
          label="Password again"
          type="password"
          value={confirm}
          autoComplete="new-password"
          onChange={(e) => edit(setConfirm)(e.target.value)}
          error={mismatch ? 'These two passwords are different.' : undefined}
        />
        <Button
          type="submit"
          variant="primary"
          fullWidth
          disabled={create.isPending || token === '' || username === '' || password === '' || password !== confirm}
        >
          {create.isPending ? 'Setting up' : 'Create account'}
        </Button>
      </form>
    </Frame>
  );
}

/**
 * The forced password change (R-046).
 *
 * An account arrives here when somebody else chose its password: an
 * administrator who added it or reset it and handed the password over, or
 * whoever set PANDO_ADMIN_PASSWORD for an installation's first account. A
 * password known to two people is a handover token rather than a password —
 * and until this screen existed, the flag saying it had to be changed was
 * something nothing could clear.
 */
export function ChangePassword({ username }: { username?: string }) {
  const [current, setCurrent] = useState('');
  const [next, setNext] = useState('');
  const [confirm, setConfirm] = useState('');
  const queries = useQueryClient();

  const mismatch = confirm !== '' && next !== confirm;

  const change = useMutation({
    mutationFn: () =>
      api.post<void>('/me/password', { current_password: current, new_password: next }),
    onSuccess: () => void queries.invalidateQueries(),
  });

  // Editing any field clears the last failure.
  //
  // Without this, a rejection outlives the value that caused it. Type something
  // too short, get "A password needs at least 10 characters", then type
  // something longer — and the message is still there, because it belongs to
  // the mutation and the mutation has not run again. The form now says the new
  // password is too short when it is not, and the only way to find out
  // otherwise is to submit anyway and disbelieve the screen.
  const edit = (set: (v: string) => void) => (value: string) => {
    if (change.isError) change.reset();
    set(value);
  };

  return (
    <Frame
      heading="Choose a password"
      // Said plainly, and only once. The person is holding a string somebody
      // gave them; they do not need to be told that this is for security.
      lede="You signed in with a password somebody else set. Choose your own."
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          change.mutate();
        }}
        style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}
      >
        {username !== undefined && (
          <Input label="Username" value={username} readOnly autoComplete="username" />
        )}
        <Input
          label="Current password"
          type="password"
          value={current}
          autoComplete="current-password"
          autoFocus
          onChange={(e) => edit(setCurrent)(e.target.value)}
        />
        <Input
          label="New password"
          type="password"
          value={next}
          autoComplete="new-password"
          helper="At least 10 characters. A short phrase you'll remember works well."
          // The server's refusal belongs here, on the field it is about. It
          // used to render under "New password again", so a message about the
          // new password's length appeared beneath a field whose only job is
          // to match — two fields' worth of confusion from one error.
          error={!mismatch && change.isError ? messageOf(change.error) : undefined}
          onChange={(e) => edit(setNext)(e.target.value)}
        />
        <Input
          label="New password again"
          type="password"
          value={confirm}
          autoComplete="new-password"
          onChange={(e) => edit(setConfirm)(e.target.value)}
          error={mismatch ? 'These two passwords are different.' : undefined}
        />
        <Button
          type="submit"
          variant="primary"
          fullWidth
          disabled={change.isPending || current === '' || next === '' || next !== confirm}
        >
          {change.isPending ? 'Saving' : 'Save password'}
        </Button>
      </form>
    </Frame>
  );
}

/**
 * The shared card. Left-aligned, one column.
 *
 * This used to carry a note saying there was no contour illustration here
 * because the logo was already doing that work. That stopped being true when
 * the real logo arrived: the mark is the "pando." wordmark with a marker-red
 * full stop, not the three nested contours the spec had described, and the
 * design system's own readme records that the logo is no longer one of the
 * places the contour figure appears.
 *
 * So the one screen every person sees before anything else carried no trace of
 * the brand's single bold idea. It gets the map as a picture, in colour. On a
 * wide window the land rises on the right and falls away before it reaches
 * the form on the left; on a narrow one it rises at the top and falls away
 * above the form. Either way the map ends where its lowest contour does — no
 * panel edge — and the form sits on plain paper, never over a line.
 */
function Frame({
  heading = 'Sign in to Pando',
  lede,
  children,
}: {
  /** A string is the page's h1; anything else — a loading outline — stands in
   *  its place without being announced as a heading. */
  heading?: React.ReactNode;
  lede?: string;
  children: React.ReactNode;
}) {
  const wide = useWide();

  return (
    <div
      style={{
        minHeight: '100vh',
        background: 'var(--paper)',
        position: 'relative',
        isolation: 'isolate',
        overflow: 'hidden',
        display: 'flex',
        alignItems: wide ? 'center' : 'flex-start',
      }}
    >
      <TopoMap seed="sign-in" recede={wide ? 'left' : 'down'} />
      <div
        style={{
          width: wide ? '44%' : '100%',
          display: 'flex',
          justifyContent: 'center',
          padding: wide ? 'var(--space-8)' : '42vh var(--space-5) var(--space-6)',
        }}
      >
        <div style={{ width: '100%', maxWidth: '36ch' }}>
          <div style={{ marginBottom: 'var(--space-6)' }}>
            <Logo size={24} />
          </div>
          {typeof heading === 'string' ? (
            heading && <h1 style={{ font: 'var(--type-h3)', color: 'var(--ink)', margin: 0 }}>{heading}</h1>
          ) : (
            heading
          )}
          {lede && (
            <p
              style={{
                font: 'var(--type-body-ui)',
                color: 'var(--ink-secondary)',
                margin: 'var(--space-3) 0 0',
              }}
            >
              {lede}
            </p>
          )}
          <div style={{ marginTop: 'var(--space-6)' }}>{children}</div>
        </div>
      </div>
    </div>
  );
}

/** Wide enough for the form and the map side by side. In em, so it follows the
 *  reader's text size rather than a device's pixel count. */
function useWide(): boolean {
  const query = '(min-width: 60em)';
  const [wide, setWide] = useState(() => window.matchMedia(query).matches);
  useEffect(() => {
    const m = window.matchMedia(query);
    const on = () => setWide(m.matches);
    m.addEventListener('change', on);
    return () => m.removeEventListener('change', on);
  }, []);
  return wide;
}

function messageOf(error: unknown): string {
  if (error instanceof RequestFailed) return error.message;
  return 'Pando could not reach the server. Check that it is running and try again.';
}

/** The message and, when the server gave one, what to do about it. */
function withRemedy(error: unknown): string {
  const message = messageOf(error);
  const remedy = error instanceof RequestFailed ? error.remedy : undefined;
  return remedy ? `${message} ${remedy}` : message;
}
