// @vitest-environment jsdom
import { afterEach, describe, expect, it, vi } from 'vitest';

import { SignIn } from './SignIn';
import type { ProviderKind } from './signInForm';
import { mount, reply } from '../ui/testing';
import type { Mounted } from '../ui/testing';

const oidc: ProviderKind = {
  category: 'identity',
  kind: 'oidc',
  name: 'OpenID Connect',
  description: 'Sign in with an OpenID Connect provider.',
  id_prefix: 'idp_',
  fields: [
    { key: 'issuer', label: 'Issuer URL', type: 'string', required: true },
    { key: 'client_id', label: 'Client ID', type: 'string', required: true },
    { key: 'hosted_domain', label: 'Google Workspace domain', type: 'string', presets: ['google'] },
  ],
  presets: [
    { id: 'okta', label: 'Okta', values: { issuer: 'https://{your-okta-domain}' } },
    { id: 'google', label: 'Google Workspace', values: { issuer: 'https://accounts.google.com' } },
  ],
  other_preset: 'Another provider',
  steps: [
    {
      id: 'app',
      label: 'Application',
      title: 'Create an app integration',
      do: [{ text: 'Set **Sign-in redirect URIs** to `{callback_url}`.' }],
      fields: ['issuer', 'client_id'],
    },
    { id: 'domain', label: 'Domain', title: 'Choose the domain', presets: ['google'], fields: ['hosted_domain'] },
  ],
};

const saml: ProviderKind = { ...oidc, kind: 'saml', name: 'SAML 2.0', description: 'Sign in with SAML.', steps: null, presets: null, other_preset: undefined };

let screen: Mounted | undefined;
afterEach(() => {
  screen?.unmount();
  screen = undefined;
  vi.unstubAllGlobals();
});

describe('adding a sign-in provider one step at a time', () => {
  it('shows the real redirect URI while it is set up, and is created with it', async () => {
    const fetch = vi.fn(async () => reply({ id: 'idp_x' }, 201));
    vi.stubGlobal('fetch', fetch);
    screen = mount(<SignIn canEdit onClearTest={() => undefined} />, [
      [
        ['identity-providers'],
        {
          providers: [],
          kinds: [oidc, saml],
          endpoints: {
            callback_url: 'https://pando.example.com/api/v1/auth/providers/{id}/callback',
            entity_id: 'https://pando.example.com/api/v1/auth/providers/{id}/metadata',
          },
        },
      ],
      [['policy'], {}],
    ]);
    const s = screen;
    await s.press('Add identity provider');
    expect(s.text()).toContain('Add identity provider');
    await s.click('OpenID Connect');
    await s.press('Continue');
    expect(s.text()).toContain('Choose a provider');
    await s.click('Okta');
    await s.press('Continue');

    const id = /providers\/(idp_[0-9A-Z]{26})\/callback/.exec(s.text())?.[1];
    expect(id).toBeDefined();
    expect(s.text()).toContain('Add OpenID Connect provider');
    await s.press('Continue');
    expect(s.text()).toContain('Replace {your-okta-domain} with your own value.');
    await s.fill('Issuer URL (required)', 'https://acme.okta.com');
    await s.fill('Client ID (required)', 'pando');
    await s.press('Continue');

    // Named from the provider chosen; the Google domain is Google's alone.
    expect((document.getElementById('f-name-required-') as HTMLInputElement).value).toBe('Okta');
    await s.fill('Name (required)', '');
    await s.press('Continue');
    expect(s.text()).toContain('A provider needs a name.');
    await s.fill('Name (required)', 'Okta');
    await s.press('Continue');
    expect(s.text()).toContain('It starts off. Test it with a real sign-in, then turn it on.');
    expect(s.text()).not.toContain('Google Workspace domain');
    await s.press('Add provider');

    const [, init] = fetch.mock.calls[0] as unknown as [string, RequestInit];
    expect(JSON.parse(String(init.body))).toMatchObject({
      id,
      kind: 'oidc',
      name: 'Okta',
      config: { issuer: 'https://acme.okta.com', client_id: 'pando' },
    });
  });

  it('offers every setting at once instead', async () => {
    screen = mount(<SignIn canEdit onClearTest={() => undefined} />, [
      [['identity-providers'], { providers: [], kinds: [oidc] }],
      [['policy'], {}],
    ]);
    await screen.press('Add identity provider');
    await screen.click('OpenID Connect');
    await screen.press('Continue');
    await screen.click('Google Workspace');
    await screen.press('Continue');
    expect(screen.text()).toContain('Create an app integration');
    await screen.press('Show all settings');
    expect(screen.text()).toContain('Protocol');
    expect(screen.text()).toContain('Just-in-time provisioning');
  });
});
