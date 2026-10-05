// Your settings: the signed-in person's own preferences, and signing out.
//
// A page of its own rather than a section of the admin console. Nothing here
// is administration, everyone signed in reaches it, and it is opened from the
// launcher and the console alike — so it wears neither one's chrome, and its
// back arrow returns to whichever it was opened from. The address is under
// /admin only because that prefix is already reserved against app slugs
// (consoleRoutes in httpapi); a new top-level path would be one more slug an
// app could not have (R-023).
//
// The theme is per browser rather than per account (see ui/theme.ts), which is
// why this screen says so: someone who picks dark here and finds a different
// laptop still light has not found a bug.

import { Icon, IconButton, Logo, Radio } from '@design';

import { usePrincipal } from '../app/principal';
import { NotificationPreferences } from './NotificationPreferences';
import { useTheme } from '../ui/theme';
import type { Preference } from '../ui/theme';
import { MEASURE } from '../ui/layout';
import { Sheet } from '../ui/Sheet';
import { LineSkeleton } from '../ui/Loading';
import { SignOut } from '../ui/SignOut';
import { TopoBackground } from '../ui/TopoBackground';

const THEMES: { value: Preference; label: string; description?: string }[] = [
  { value: 'system', label: 'Match this device', description: 'Follows your device’s light or dark setting.' },
  { value: 'light', label: 'Light' },
  { value: 'dark', label: 'Dark' },
];

export function Settings({
  onBack,
  onSignedOut,
}: {
  onBack: () => void;
  onSignedOut?: () => void;
}) {
  const me = usePrincipal();
  const { preference, set } = useTheme();

  const who = me.data?.display_name || me.data?.username || me.data?.email;

  return (
    <div style={{ minHeight: '100vh', background: 'var(--paper)', position: 'relative', isolation: 'isolate' }}>
      <TopoBackground seed="settings" />
      <header
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 'var(--space-3)',
          padding: 'var(--space-5) var(--console-padding)',
          borderBottom: 'var(--border-width) solid var(--rule)',
        }}
      >
        <IconButton label="Back" onClick={onBack}>
          <Icon name="arrow-left" size={16} />
        </IconButton>
        <Logo size={20} />
      </header>

      <main style={{ maxWidth: 'var(--console-max)', margin: '0 auto' }}>
        <Sheet heading="Your settings">
          <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-7)', maxWidth: MEASURE }}>
            <section>
              <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-3)' }}>Appearance</h4>
              <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: '0 0 var(--space-3)' }}>
                Saved in this browser only.
              </p>
              <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
                {THEMES.map((t) => (
                  <Radio
                    key={t.value}
                    name="theme"
                    value={t.value}
                    checked={preference === t.value}
                    onChange={() => set(t.value)}
                    label={t.label}
                    description={t.description}
                  />
                ))}
              </div>
            </section>

            <section>
              <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-3)' }}>Notifications</h4>
              <NotificationPreferences />
            </section>

            <section>
              <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-3)' }}>Account</h4>
              {/* Usually already loaded by the time this page opens; when it is
                  not, the line keeps its place so Sign out does not move. */}
              {me.isPending && (
                <div style={{ marginBottom: 'var(--space-3)' }}>
                  <LineSkeleton width="28ch" />
                </div>
              )}
              {who && (
                <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: '0 0 var(--space-3)' }}>
                  Signed in as <strong style={{ color: 'var(--ink)' }}>{who}</strong>.
                </p>
              )}
              <SignOut onSignedOut={onSignedOut} />
            </section>
          </div>
        </Sheet>
      </main>
    </div>
  );
}
