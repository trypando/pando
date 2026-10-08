import { describe, expect, it } from 'vitest';

import {
  adapterRequest,
  advancedChanged,
  blankForm,
  boolValue,
  categoryLabel,
  effectiveText,
  fieldPlaceholder,
  formProblems,
  isShown,
  orderCategories,
  sortKinds,
} from './adapters';
import type { AdapterKind } from './adapters';

const anthropic: AdapterKind = {
  category: 'ai',
  kind: 'anthropic',
  name: 'Anthropic',
  description: 'Reads a repository and checks the plan detection made.',
  id_prefix: 'ai_',
  fields: [
    { key: 'api_key', label: 'API key', type: 'string', credential: true, required: true },
    { key: 'model', label: 'Model', type: 'string' },
    { key: 'base_url', label: 'Base URL', type: 'string' },
  ],
};

const trivy: AdapterKind = {
  category: 'scanner',
  kind: 'trivy',
  name: 'Trivy',
  description: 'Scans images.',
  id_prefix: 'scan_',
  fields: [
    { key: 'timeout_seconds', label: 'Timeout', type: 'int', placeholder: '300' },
    { key: 'offline', label: 'Offline', type: 'bool' },
  ],
};

describe('adding an adapter', () => {
  it('prefills an ID from the kind and makes the first of a category the default', () => {
    expect(blankForm(anthropic, true)).toEqual({ id: 'ai_anthropic', name: 'Anthropic', isDefault: true, values: {} });
    expect(blankForm(anthropic, false).isDefault).toBe(false);
  });

  it('sends credentials in credentials, never in config (R-190)', () => {
    const body = adapterRequest(anthropic, {
      ...blankForm(anthropic, true),
      values: { api_key: ' sk-ant-123 ', model: 'claude-sonnet-5', base_url: '' },
    });
    expect(body).toEqual({
      id: 'ai_anthropic',
      category: 'ai',
      kind: 'anthropic',
      name: 'Anthropic',
      config: { model: 'claude-sonnet-5' },
      credentials: { api_key: 'sk-ant-123' },
      is_default: true,
    });
  });

  it('leaves out an empty credential, which keeps the stored one', () => {
    const body = adapterRequest(anthropic, { ...blankForm(anthropic, false), values: { api_key: '' } }, true);
    expect(body.credentials).toBeUndefined();
    expect(body.config).toEqual({});
    expect(body.enabled).toBe(true);
  });

  it('sends whole numbers as numbers and a bool only when on', () => {
    const on = adapterRequest(trivy, { ...blankForm(trivy, true), values: { timeout_seconds: '120', offline: true } });
    expect(on.config).toEqual({ timeout_seconds: 120, offline: true });
    const off = adapterRequest(trivy, { ...blankForm(trivy, true), values: { offline: false } });
    expect(off.config).toEqual({});
  });

  it('says what is wrong in words that say how to fix it', () => {
    const form = { ...blankForm(trivy, true), id: ' ', values: { timeout_seconds: '2m' } };
    expect(formProblems(trivy, form)).toEqual({
      id: 'An adapter needs an ID, for example scan_trivy.',
      timeout_seconds: 'Timeout takes a whole number, such as 300.',
    });
  });

  it('requires a required credential unless one is already stored', () => {
    const form = blankForm(anthropic, true);
    expect(formProblems(anthropic, form)).toEqual({ api_key: 'API key is required.' });
    expect(formProblems(anthropic, form, ['api_key'])).toEqual({});
  });

  it('offers kinds by category, then name', () => {
    expect(sortKinds([trivy, anthropic]).map((k) => k.kind)).toEqual(['anthropic', 'trivy']);
    expect(categoryLabel('ai')).toBe('AI');
    expect(categoryLabel('routing')).toBe('Routing');
    // The twelfth category (issue #153), named as words rather than its key.
    expect(categoryLabel('image_registry')).toBe('Image registry');
  });
});

describe('adapter categories', () => {
  it('orders categories as an installation is built up, unknown ones last', () => {
    expect(orderCategories(['ai', 'runtime', 'zeta', 'routing', 'ai'])).toEqual(['runtime', 'routing', 'ai', 'zeta']);
    // Beside the builder: where what it builds goes.
    expect(orderCategories(['scanner', 'image_registry', 'builder'])).toEqual(['builder', 'image_registry', 'scanner']);
  });
});

describe('an empty field', () => {
  it('shows the default, which is what leaving it empty gets', () => {
    expect(
      fieldPlaceholder({ key: 'model', label: 'Model', type: 'string', default: 'claude-opus-5-5', placeholder: 'claude-x' }),
    ).toBe('claude-opus-5-5');
  });

  it('shows an example when there is no default', () => {
    expect(fieldPlaceholder({ key: 'base_domain', label: 'Base domain', type: 'string', placeholder: 'apps.example.com' })).toBe(
      'apps.example.com',
    );
  });
});

// A cut of the Traefik kind: a bool that defaults on, a choice, and settings
// that apply only under some of its answers.
const traefik: AdapterKind = {
  category: 'routing',
  kind: 'traefik',
  name: 'Traefik',
  description: 'Gives apps their own hostnames.',
  id_prefix: 'rte_',
  fields: [
    {
      key: 'certificates',
      label: 'Certificates',
      type: 'select',
      default: 'none',
      options: [
        { value: 'http', label: 'One per hostname' },
        { value: 'dns', label: 'One wildcard for the base domain' },
        { value: 'none', label: 'None' },
      ],
    },
    { key: 'acme_email', label: 'Certificate email', type: 'string', shown_when: { key: 'certificates', values: ['http', 'dns'] } },
    {
      key: 'dns_provider',
      label: 'DNS provider',
      type: 'select',
      other: true,
      required: true,
      options: [{ value: 'cloudflare', label: 'Cloudflare' }],
      shown_when: { key: 'certificates', values: ['dns'] },
    },
    { key: 'dns_credentials', label: 'DNS provider credentials', type: 'string', credential: true, multiline: true, shown_when: { key: 'certificates', values: ['dns'] } },
    { key: 'managed', label: 'Pando runs Traefik', type: 'bool', default: 'true' },
    { key: 'entrypoint', label: 'Entry point', type: 'string', shown_when: { key: 'managed', values: ['false'] } },
  ],
};

describe('settings that depend on others', () => {
  const field = (key: string) => traefik.fields!.find((f) => f.key === key)!;

  it('shows a setting only under the answers it applies to, counting defaults', () => {
    expect(isShown(traefik, field('acme_email'), {})).toBe(false);
    expect(isShown(traefik, field('acme_email'), { certificates: 'http' })).toBe(true);
    expect(isShown(traefik, field('entrypoint'), {}), 'managed defaults on').toBe(false);
    expect(isShown(traefik, field('entrypoint'), { managed: false })).toBe(true);
  });

  it('neither checks nor sends a hidden setting', () => {
    const form = { ...blankForm(traefik, true), values: { certificates: 'none', dns_credentials: 'CF_DNS_API_TOKEN=x' } };
    expect(formProblems(traefik, form)).toEqual({});
    expect(adapterRequest(traefik, form).credentials).toBeUndefined();

    const dns = { ...form, values: { certificates: 'dns', dns_credentials: 'CF_DNS_API_TOKEN=x' } };
    expect(formProblems(traefik, dns)).toEqual({ dns_provider: 'DNS provider is required.' });
    expect(adapterRequest(traefik, dns).credentials).toEqual({ dns_credentials: 'CF_DNS_API_TOKEN=x' });
  });

  it('sends a bool that defaults on when it is turned off, and not when left on (R-174)', () => {
    expect(boolValue(field('managed'), undefined)).toBe(true);
    expect(adapterRequest(traefik, { ...blankForm(traefik, true), values: {} }).config).not.toHaveProperty('managed');
    expect(adapterRequest(traefik, { ...blankForm(traefik, true), values: { managed: false } }).config.managed).toBe(false);
  });

  it('sends a provider typed in under Other as it was typed', () => {
    const form = { ...blankForm(traefik, true), values: { certificates: 'dns', dns_provider: ' ovh ' } };
    expect(adapterRequest(traefik, form).config.dns_provider).toBe('ovh');
    expect(effectiveText(field('certificates'), undefined)).toBe('none');
  });
});

describe('advanced settings', () => {
  const scanner: AdapterKind = {
    ...trivy,
    fields: [
      { key: 'timeout_seconds', label: 'Timeout', type: 'int', default: '600', advanced: true },
      { key: 'offline', label: 'Offline', type: 'bool', advanced: true },
      { key: 'note', label: 'Note', type: 'string' },
    ],
  };

  it('opens them when one is set to something other than its default', () => {
    expect(advancedChanged(scanner, {})).toBe(false);
    expect(advancedChanged(scanner, { timeout_seconds: '600', note: 'x' })).toBe(false);
    expect(advancedChanged(scanner, { timeout_seconds: ' ' })).toBe(false);
    expect(advancedChanged(scanner, { timeout_seconds: '900' })).toBe(true);
    expect(advancedChanged(scanner, { offline: false })).toBe(false);
    expect(advancedChanged(scanner, { offline: true })).toBe(true);
  });

  it('can be left empty: a kind is added from its basic settings alone', () => {
    const form = blankForm(scanner, true);
    expect(formProblems(scanner, form)).toEqual({});
    expect(adapterRequest(scanner, form).config).toEqual({});
  });
});
