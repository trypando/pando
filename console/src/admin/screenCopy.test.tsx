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
import { Blocked, DetectionFailed, DetectionReview } from './DetectionReview';
import { Egress } from './Egress';
import { RegistryCredential } from './RegistryCredential';
import { Resources } from './Resources';
import { Sharing } from './Sharing';
import { AppVerb } from './verbs';

const app = { id: 'app_1', name: 'inventory', pinned_spec_id: 'spec_1' } as App;

// One of each way a dependency is filled.
const slots = [
  { key: 'DATABASE_URL', type: 'postgres', required: true, resolution: { mode: 'provisioned' } },
  { key: 'REDIS_URL', type: 'redis', resolution: { mode: 'bound' } },
  { key: 'SMTP_URL', type: 'smtp', resolution: { mode: 'literal', secret_ref: 'sec_1' } },
];

describe('how a dependency is filled', () => {
  it('reads Provisioned, External or Value on the app’s settings', () => {
    const html = render(<Resources app={app} />, [[['slots', 'app_1'], { slots }]]);
    expect(html).toContain('>Provisioned<');
    expect(html).toContain('>External<');
    expect(html).toContain('>Value<');
  });

  it('reads the same words in the detection review', () => {
    const detection = {
      winning_bid: { detector: 'dockerfile', strategy: 'dockerfile', confidence: 0.9, evidence: [] },
      draft_spec: { workloads: [{ name: 'web' }], slots },
    };
    const html = render(<DetectionReview appID="app_1" reviewed={false} />, [
      [['apps', 'app_1', 'detection'], { status: 'ready', answers: {}, detection }],
    ]);
    expect(html).toContain('>Provisioned<');
    expect(html).toContain('>External<');
    expect(html).toContain('>Value<');
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

describe('egress, by the list’s mode', () => {
  const egress = (mode: string, appMode: string) =>
    render(<Egress appID="app_1" />, [
      [['apps', 'app_1', 'egress'], { install: { mode }, spec: null, effective: { mode, list: [], app_mode: appMode, app_list: [], block_private: false, restricted: false } }],
    ]);

  it.each([
    ['denylist', 'allowlist', 'Blocked', 'No destinations blocked.', 'App allowlist', 'No destinations allowed.'],
    ['allowlist', 'denylist', 'Allowed', 'No destinations allowed.', 'App denylist', 'No destinations blocked.'],
  ])('reads an installation %s with an app %s', (mode, appMode, header, empty, appWords, appEmpty) => {
    const html = egress(mode, appMode);
    expect(html).toContain(`>${header}<`);
    expect(html).toContain(empty);
    expect(html).toContain(`${appWords}, applied on top of the installation’s rules.`);
    expect(html).toContain(appEmpty);
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
