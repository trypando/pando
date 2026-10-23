import { describe, expect, it, vi } from 'vitest';

// The API client reads the page's address when it loads. Rendered on the
// server here, so there is no page: give it the least it needs.
vi.hoisted(() => {
  (globalThis as { window?: unknown }).window ??= { location: { pathname: '/' } };
});

import type { App } from '@api/types.gen';
import { serverRender as render } from '../ui/testing';
import { AppImage } from './AppImage';
import { AppOverview } from './AppOverview';
import { DeploySection } from './DeploySettings';
import { Blocked, DetectionFailed } from './DetectionReview';
import { RegistryCredential } from './RegistryCredential';
import { SettingsDependencies } from './SettingsDependencies';
import { Sharing } from './Sharing';
import { AppVerb } from './verbs';

const app = { id: 'app_1', name: 'inventory', pinned_spec_id: 'spec_1' } as App;

// One of each way a dependency is filled, and the two ways it is not.
const slots = [
  { key: 'DATABASE_URL', type: 'postgres', required: true, resolution: { mode: 'provisioned' } },
  { key: 'REDIS_URL', type: 'redis', resolution: { mode: 'bound', target: 'redis://cache:6379' } },
  { key: 'SMTP_URL', type: 'smtp', resolution: { mode: 'literal', secret_ref: 'sec_1' } },
  { key: 'QUEUE_URL', type: 'redis' },
  { key: 'STRIPE_KEY', type: 'unknown', required: true },
];

describe('how a dependency is filled (R-131, R-132)', () => {
  const page = (verbs: string[] = []) => render(<SettingsDependencies appID="app_1" />, [[['slots', 'app_1'], { slots }]], verbs);

  it('says where each stands, a missing one in the warning color and never red', () => {
    const html = page();
    for (const said of ['Pando runs it', 'Connected', 'Value set', 'Optional, not set up', 'Needed to deploy']) {
      expect(html).toContain(`>${said}<`);
    }
    expect(html).toContain('color:var(--contour-text)');
    expect(html).not.toContain('var(--marker');
  });

  it('offers the three ways as one control, with the chosen one checked', () => {
    const html = page([AppVerb.SpecEdit, AppVerb.SecretsWrite]);
    expect(html).toContain('role="radiogroup" aria-label="How DATABASE_URL is filled"');
    expect(html.match(/role="radio"/g)).toHaveLength(15);
    expect(html.match(/aria-checked="true"/g)).toHaveLength(3);
    // Where its data lives, the existing one it is pointed at, a value never shown.
    expect(html).toContain('Runs beside this app');
    expect(html).toContain('value="redis://cache:6379"');
    expect(html).toContain('placeholder="••••••••••••"');
  });

  it('offers no way Pando can’t take: it doesn’t run a mail server or a bare value', () => {
    const html = page([AppVerb.SpecEdit]);
    // Each card's first way, Pando runs one, is offered only where Pando can.
    const first = [...html.matchAll(/role="radio" aria-checked="(?:true|false)"( disabled="")?[^>]*>Pando runs one</g)].map((m) => Boolean(m[1]));
    expect(first).toEqual([false, false, true, false, true]);
    // A value is a secret, which needs app.secrets.write.
    expect(html).toMatch(/disabled=""[^>]*>Enter a value</);
  });

  it('changes nothing for somebody who can only look', () => {
    expect(page()).not.toMatch(/role="radio" aria-checked="(?:true|false)"(?! disabled)/);
  });

  it('says so when the app uses nothing', () => {
    const html = render(<SettingsDependencies appID="app_1" />, [[['slots', 'app_1'], { slots: [] }]]);
    expect(html).toContain('This app doesn’t use any databases or other services.');
    expect(html).not.toContain('radiogroup');
  });
});

describe('what the overview asks of somebody', () => {
  // The words are the server's (GET /notices, R-412); the overview lists them.
  it('lists the notices from the server', () => {
    const html = render(<AppOverview app={app} onGo={() => {}} />, [
      [['apps', 'app_1', 'notices'], { notices: [{ key: 'variables.unset', message: 'API_KEY is declared with no value.', fingerprint: 'f', ignored: false }] }],
    ]);
    expect(html).toContain('Needs your attention');
    expect(html).toContain('API_KEY is declared with no value.');
  });
});

describe('who has access', () => {
  const sharing = (grants: unknown[]) =>
    render(<Sharing appID="app_1" appName="inventory" />, [[['apps', 'app_1', 'grants', '', {}], { pages: [{ grants }], pageParams: [''] }]]);

  it('says what making it public means, on the switch itself (R-077)', () => {
    const html = sharing([{ id: 'gr_1', plane: 'data', principal_kind: 'anonymous', passcode: true }]);
    expect(html).toContain('Accessible to anyone on the internet');
    expect(html).toContain('Visitors need the passcode to open it');
  });

  it('lists a person’s role and their use of the app in separate columns', () => {
    const html = sharing([
      { id: 'gr_1', plane: 'control', principal_kind: 'user', principal_id: 'usr_1', principal_name: 'dana', role_name: 'owner' },
      { id: 'gr_2', plane: 'data', principal_kind: 'user', principal_id: 'usr_1', principal_name: 'dana' },
    ]);
    expect(html).toContain('>Opens it<');
    expect(html).toContain('>Manages it<');
    expect(html).toContain('>Owner<');
    expect(html).toContain('Yes');
  });

  it('lists a role alone for a person who can’t use the app', () => {
    const html = sharing([{ id: 'gr_1', plane: 'control', principal_kind: 'user', principal_id: 'usr_1', principal_name: 'dana', role_name: 'viewer' }]);
    expect(html).toContain('>Viewer<');
    expect(html).toContain('>No<');
    expect(html).not.toContain('Yes');
  });
});

describe('a registry credential', () => {
  const credential = (summary: unknown) =>
    render(<RegistryCredential appID="app_1" />, [[['apps', 'app_1', 'registry-credential'], summary]], [AppVerb.SecretsWrite]);

  it.each([
    [{ set: false }, 'None.', 'Add credential'],
    [{ set: true, credential: { kind: 'ecr', access_key_id: 'AKIA1', region: 'us-east-1' } }, 'AWS access key AKIA1 in us-east-1.', 'Replace'],
    [{ set: true, credential: { kind: 'ecr' } }, 'AWS access key .', 'Replace'],
    [{ set: true, credential: { kind: 'basic', username: 'robot' } }, 'Signed in as robot.', 'Replace'],
    [{ set: true }, 'Signed in as .', 'Replace'],
  ])('describes %j', (summary, described, button) => {
    const html = credential(summary);
    expect(html).toContain(described);
    expect(html).toContain(`>${button}<`);
  });
});

describe('auto-deploy settings', () => {
  const deploy = (ref: string, view: unknown) =>
    render(
      <DeploySection app={app} />,
      [
        [['apps', 'app_1', 'specs'], { revisions: [{ id: 'spec_1', revision: 1 }], pinned_spec_id: 'spec_1' }],
        [['apps', 'app_1', 'spec', 1], { id: 'spec_1', revision: 1, body: { source: { type: 'git', ref }, deploy: { auto_deploy: { enabled: true } } } }],
        [['apps', 'app_1', 'auto-deploy'], { deployed: { enabled: true }, webhook_url: 'https://pando.test/hook', ...(view as object) }],
      ],
      [AppVerb.SpecEdit],
    );

  it('names the branch it defaults to, and when it last looked', () => {
    const html = deploy('main', { webhook_secret_set: true, last_check: { app_id: 'app_1', checked_at: new Date().toISOString() } });
    expect(html).toContain('Defaults to main.');
    expect(html).toContain('Replace webhook secret');
    expect(html).toContain('Last checked');
  });

  it('says nothing about a default without one, and that it hasn’t looked yet', () => {
    const html = deploy('', { webhook_secret_set: false });
    expect(html).not.toContain('Defaults to');
    expect(html).toContain('Generate webhook secret');
    expect(html).toContain('Not checked yet.');
  });
});

describe('detection that stopped', () => {
  it('shows a trial run’s output only when there is one', () => {
    const blocked = { message: 'It can’t run.' };
    expect(render(<Blocked proposal={{ blocked, trial_log: ['exit 1'] } as never} />)).toContain('Trial run output');
    expect(render(<Blocked proposal={{ blocked } as never} />)).not.toContain('Trial run output');
  });

  it.each([
    [true, 'Trying again'],
    [false, 'Try again'],
  ])('offers to try again, retrying: %s', (retrying, label) => {
    expect(render(<DetectionFailed onRetry={() => {}} retrying={retrying} />)).toContain(`>${label}<`);
  });

  it('offers nothing to somebody who can’t retry', () => {
    expect(render(<DetectionFailed retrying={false} />)).not.toContain('Try again');
  });
});

describe('an app’s icon', () => {
  it('offers a change only to somebody who can make it', () => {
    expect(render(<AppImage app={app} />, [], [AppVerb.SpecEdit])).toContain('aria-label="Change image"');
    expect(render(<AppImage app={app} />)).not.toContain('Change image');
  });
});
