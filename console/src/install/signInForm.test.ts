import { describe, expect, it } from 'vitest';

import {
  duration,
  outcomeText,
  placeholderIn,
  presetValues,
  problems,
  providerRequest,
  revocationText,
} from './signInForm';
import type { ProviderKind } from './signInForm';

const oidc: ProviderKind = {
  category: 'identity',
  kind: 'oidc',
  name: 'OpenID Connect',
  description: '',
  id_prefix: 'idp_',
  fields: [
    { key: 'issuer', label: 'Issuer URL', type: 'string', required: true },
    { key: 'client_id', label: 'Client ID', type: 'string', required: true },
    { key: 'client_secret', label: 'Client secret', type: 'string', credential: true },
    { key: 'disable_userinfo', label: 'Skip userinfo', type: 'bool' },
  ],
  presets: [{ id: 'entra', label: 'Microsoft Entra ID', values: { issuer: 'https://login.microsoftonline.com/{tenant-id}/v2.0' } }],
};

describe('revocationText', () => {
  it('states the window rather than implying it is instant', () => {
    expect(revocationText('oidc', { max_lifetime_seconds: 43200, mode: 'expiry_only', window_seconds: 43200 })).toContain(
      'up to 12 hours',
    );
    expect(revocationText('oidc', { max_lifetime_seconds: 43200, mode: 'push', window_seconds: 120 })).toContain(
      'within 2 minutes',
    );
    expect(revocationText('local', { max_lifetime_seconds: 43200, mode: 'push', window_seconds: 120 })).toContain(
      'Suspending an account here',
    );
  });

  it('reads durations the way people say them', () => {
    expect(duration(120)).toBe('2 minutes');
    expect(duration(3600)).toBe('1 hour');
    expect(duration(86400 * 2)).toBe('2 days');
    expect(duration(90)).toBe('90 seconds');
  });
});

describe('the provider form', () => {
  it('starts from a preset and asks for its placeholder to be filled', () => {
    const values = presetValues(oidc, oidc.presets![0]);
    expect(placeholderIn(values)).toEqual({ field: 'issuer', placeholder: '{tenant-id}' });
    expect(problems(oidc, 'Entra', { ...values, client_id: 'x' }).issuer).toBe('Replace {tenant-id} with your own value.');
  });

  it('keeps a stored secret when the field is left empty', () => {
    const values = { issuer: 'https://idp.example.com', client_id: 'pando' };
    expect(problems(oidc, 'Okta', values, ['client_secret'])).toEqual({});
    expect(problems(oidc, '', values).name).toContain('needs a name');
  });

  it('sends a secret as a credential and nowhere else (R-190)', () => {
    const body = providerRequest(
      oidc,
      ' Okta ',
      { issuer: 'https://idp.example.com', client_id: 'pando', client_secret: 's3cret', disable_userinfo: false },
      { jit: true, linkByEmail: false },
      true,
    );
    expect(body).toEqual({
      kind: 'oidc',
      name: 'Okta',
      config: { issuer: 'https://idp.example.com', client_id: 'pando' },
      credentials: { client_secret: 's3cret' },
      jit_provisioning: true,
      link_by_email: false,
    });
  });
});

describe('outcomeText', () => {
  it('says what a real sign-in would do', () => {
    expect(outcomeText({ kind: 'created', display_name: 'Dana', groups_from: 'sign_in' })).toContain('create a new account for Dana');
    expect(outcomeText({ kind: 'refused', message: 'No account.', remedy: 'Ask.', groups_from: 'sign_in' })).toBe('No account. Ask.');
  });
});
