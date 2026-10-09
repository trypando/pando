// Two audiences, one app (R-264, R-265).
//
// Root is the launcher. The admin console is not a separate build — a user with
// no administrative verbs simply never reaches those routes, which is what
// makes a non-technical user's first experience a page of tiles rather than a
// dashboard.
//
// Routing is a path switch rather than TanStack Router. Design 08 §1.2 lists
// typed routes as a [P] choice; this overrides it, because the router's value
// is in a large route tree and there are three shapes here — the launcher, a
// section, and an app with a tab. Revisit when there is a tree to type.
//
// It is a real path switch now. It used to be this comment over a useState,
// which is not a path switch at all: a reload dropped whoever was looking at
// the console back on the launcher, the back button did nothing, and there was
// no way to send anybody a link to an app.

import { useRef } from 'react';
import { Skeleton } from '@design';

import { useAdministrative, usePrincipal } from './principal';
import { useTheme } from '../ui/theme';
import { useRoute } from './route';
import type { Route } from './route';
import { ChangePassword, Login, Passcode } from '../auth/Login';
import { returnTo } from '../auth/return-to';
import { reservedPrefix } from '@api/client';
import { passcodeApp } from '../auth/passcode';
import { Launcher } from '../launcher/Launcher';
import { AdminConsole } from '../admin/AdminConsole';
import { Settings } from '../settings/Settings';
import { Sheet } from '../ui/Sheet';
import { HeadingSkeleton, Loading } from '../ui/Loading';

export function App() {
  // Before anything decides what to render: the sign-in page and the error
  // states are the console too, and they were the screens most likely to be
  // met in the dark.
  useTheme();

  const principal = usePrincipal();
  const isAdmin = useAdministrative();
  const [route, go] = useRoute();
  // Where settings was opened from. A ref, not the history stack: someone who
  // opened settings by its address has nothing behind them in this console,
  // and history.back() would take them off Pando altogether.
  const cameFrom = useRef<Route | null>(null);

  // The passcode page shares the sign-in page's address, and comes before
  // anything that depends on who this is: a visitor who has a passcode needs no
  // account, and somebody signed in who lacks access to the app is sent here by
  // the proxy just the same.
  const passcodeFor = passcodeApp(window.location.search);
  if (passcodeFor) {
    return <Passcode appID={passcodeFor} signedIn={principal.isSuccess} />;
  }

  // Who this is decides which page this is — the launcher, the console or the
  // sign-in form — so nothing more specific than a page can be drawn yet: the
  // paper, a heading's place and a block under it, held back like every
  // skeleton so a quick answer shows nothing at all.
  if (principal.isPending) {
    return (
      <div style={{ minHeight: '100vh', background: 'var(--paper)' }}>
        <Sheet heading={<HeadingSkeleton />}>
          <Loading>
            <Skeleton height="12rem" radius="md" />
          </Loading>
        </Sheet>
      </div>
    );
  }

  // Not signed in. The proxy sends an unauthenticated visitor here (R-023), and
  // so does an expired session on an open page. Both land on the same form
  // rather than a link to one: this used to render a link to /login, and /login
  // rendered this component, so the only way into a fresh install was the API.
  if (principal.isError) {
    return <Login />;
  }

  // R-046: the generated first-run credential must be changed before anything
  // else. Placed here rather than inside the launcher so there is no screen
  // that can be reached around it — a "you should change your password" banner
  // is a suggestion, and R-046 says must.
  if (principal.data?.must_change_password) {
    return <ChangePassword username={principal.data.username} />;
  }

  // Signed in, on the reserved path. On an app's own hostname that path
  // answers signing in and nothing else (issue #78), so the console cannot be
  // drawn here: go where the sign-in was for, or to this address's root —
  // the app on its own hostname, the launcher on Pando's.
  if (reservedPrefix() !== '') {
    window.location.replace(returnTo(window.location.search, window.location.href) ?? '/');
    return null;
  }

  // Signing out lands on the sign-in form at the root, not at whatever admin
  // address was open — the next person to sign in on this browser should start
  // on their own launcher. Replace, so Back does not return to a page that
  // would only answer 401.
  const signedOut = () => go({ view: 'launcher', section: 'apps' }, true);

  // Settings is a page of its own, reached from either half of the console,
  // and its back arrow returns to whichever one that was.
  if (route.view === 'settings') {
    return (
      <Settings
        onBack={() => go(cameFrom.current ?? { view: 'launcher', section: 'apps' })}
        onSignedOut={signedOut}
      />
    );
  }
  const openSettings = () => {
    cameFrom.current = route;
    go({ view: 'settings', section: 'apps' });
  };

  // Someone who lands on /admin without the verbs for it gets the launcher,
  // and the address bar is corrected to say so — replace, not push, so the
  // back button does not bounce them between a page they cannot see and one
  // they can. The server refuses the requests underneath regardless; this is
  // only about not showing a shell that answers 403 to everything (R-265).
  // The API screen is open to anyone signed in, so the console's shell is too
  // — with everything else in it hidden, which is what the sidebar already does
  // per verb. Somebody who lands on /admin with no verbs still goes back to the
  // launcher; only /admin/api lets them stay.
  if (route.view === 'admin' && (isAdmin || route.section === 'api')) {
    return (
      <AdminConsole
        route={route}
        go={go}
        administrative={isAdmin}
        onLeave={() => go({ view: 'launcher', section: 'apps' })}
        onSettings={openSettings}
      />
    );
  }
  if (route.view === 'admin' && !isAdmin && !principal.isPending) {
    go({ view: 'launcher', section: 'apps' }, true);
  }

  return (
    <Launcher
      onAdmin={isAdmin ? () => go({ view: 'admin', section: 'apps' }) : undefined}
      onSettings={openSettings}
      onManage={isAdmin ? (appID) => go({ view: 'admin', section: 'apps', appID }) : undefined}
    />
  );
}
