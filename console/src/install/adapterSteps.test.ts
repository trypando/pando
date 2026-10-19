import { describe, expect, it } from 'vitest';

import {
  PRESET_OTHER,
  fillRefs,
  fillText,
  joinList,
  kindSteps,
  listItems,
  reviewText,
  runs,
  stepFields,
  stepProblems,
  unasked,
  withPreset,
  withSuggestions,
} from './adapters';
import type { AdapterKind } from './adapters';

const kind: AdapterKind = {
  category: 'routing',
  kind: 'example',
  name: 'Example',
  description: 'An example.',
  id_prefix: 'rte_',
  fields: [
    {
      key: 'method',
      label: 'Method',
      type: 'select',
      default: 'token',
      options: [
        { value: 'token', label: 'Token' },
        { value: 'key', label: 'Key' },
      ],
    },
    { key: 'token', label: 'Token', type: 'string', credential: true, shown_when: { key: 'method', values: ['token'] } },
    { key: 'zone', label: 'Zone', type: 'string', required: true },
    { key: 'console', label: 'Console', type: 'string', suggest: 'pando.{zone}' },
    { key: 'zones', label: 'Zones', type: 'string', list: true },
    { key: 'image', label: 'Image', type: 'string', default: 'x', advanced: true },
  ],
  steps: [
    { id: 'method', label: 'Method', title: 'Choose a method', fields: ['method'] },
    { id: 'token', label: 'Token', title: 'Create a token', fields: ['token'], shown_when: { key: 'method', values: ['token'] } },
    { id: 'splunk', label: 'Splunk', title: 'Splunk only', fields: ['zones'], presets: ['splunk'] },
    { id: 'zone', label: 'Zone', title: 'Choose a zone', fields: ['zone', 'console'] },
  ],
};

describe('guided setup steps', () => {
  it('skips a step another answer rules out, and keeps a provider to its own', () => {
    expect(kindSteps(kind, {}).map((s) => s.id)).toEqual(['method', 'token', 'splunk', 'zone']);
    expect(kindSteps(kind, { method: 'key' }, PRESET_OTHER).map((s) => s.id)).toEqual(['method', 'zone']);
    expect(kindSteps(kind, {}, 'splunk').map((s) => s.id)).toEqual(['method', 'token', 'splunk', 'zone']);
  });

  it('gives a kind without steps one step of its basic settings', () => {
    const plain = { ...kind, steps: null };
    expect(kindSteps(plain, {})).toEqual([
      { id: 'settings', label: 'Settings', title: 'Enter the Example settings', fields: ['method', 'token', 'zone', 'console', 'zones'] },
    ]);
    expect(kindSteps({ ...plain, fields: [] }, {})).toEqual([]);
  });

  it('checks only the step’s own settings on Continue', () => {
    const zone = kind.steps![3]!;
    const form = { id: 'rte_example', name: 'Example', isDefault: false, values: {} };
    expect(stepFields(kind, zone, {}).map((f) => f.key)).toEqual(['zone', 'console']);
    expect(stepProblems(kind, zone, form)).toEqual({ zone: 'Zone is required.' });
    expect(stepProblems(kind, kind.steps![0]!, form)).toEqual({});
  });
});

describe('suggestions and links', () => {
  it('fills {key} from what the adapter will read, or not at all', () => {
    expect(fillRefs(kind, { zone: 'example.com' }, 'https://{zone}/x')).toBe('https://example.com/x');
    expect(fillRefs(kind, {}, 'https://{zone}/x')).toBeUndefined();
    expect(fillRefs(kind, {}, 'method is {method}')).toBe('method is token');
  });

  it('follows a setting until the suggested one is edited', () => {
    expect(withSuggestions(kind, { zone: 'example.com' }).console).toBe('pando.example.com');
    expect(withSuggestions(kind, { zone: 'example.com', console: '' }).console).toBe('');
    expect(withSuggestions(kind, {}).console).toBeUndefined();
  });
});

describe('providers', () => {
  const splunk = { id: 'splunk', label: 'Splunk', values: { body: 'hec', url: 'https://splunk' } };
  const datadog = { id: 'datadog', label: 'Datadog', values: { body: 'json' } };

  it('fills in a provider’s values, and takes back the last one’s unless changed', () => {
    expect(withPreset({ token: 't' }, undefined, splunk)).toEqual({ token: 't', body: 'hec', url: 'https://splunk' });
    expect(withPreset({ body: 'hec', url: 'https://mine' }, splunk, datadog)).toEqual({ body: 'json', url: 'https://mine' });
    expect(withPreset({ body: 'hec' }, splunk, undefined)).toEqual({});
  });
});

describe('step text and values', () => {
  it('reads bold and code marks and nothing else', () => {
    expect(runs('Open **Settings** and run `make`. *not* a mark')).toEqual([
      { kind: 'text', text: 'Open ' },
      { kind: 'bold', text: 'Settings' },
      { kind: 'text', text: ' and run ' },
      { kind: 'code', text: 'make' },
      { kind: 'text', text: '. *not* a mark' },
    ]);
  });

  it('keeps a list as typed and sends it comma-separated', () => {
    expect(listItems(' a.org, b.org\nc.org ')).toEqual(['a.org', 'b.org', 'c.org']);
    expect(joinList([' a.org ', '', 'b.org'])).toBe('a.org, b.org');
  });

  it('states each setting as the adapter will read it', () => {
    const [method, token, zone] = kind.fields!;
    expect(reviewText(method!, undefined)).toBe('Token');
    expect(reviewText(token!, 'secret')).toBe('Entered');
    expect(reviewText(token!, '')).toBe('Not entered');
    expect(reviewText(zone!, '')).toBe('None');
    expect(reviewText({ key: 'b', label: 'B', type: 'bool', default: 'true' }, undefined)).toBe('Yes');
  });
});

describe('values the form supplies', () => {
  it('fills an identity provider’s address into step text, and leaves an unknown one as written', () => {
    const supplied = { callback_url: 'https://pando.example.com/api/v1/auth/providers/idp_1/callback' };
    expect(fillText(kind, {}, 'Set **Redirect URI** to `{callback_url}`.', supplied)).toBe(
      'Set **Redirect URI** to `https://pando.example.com/api/v1/auth/providers/idp_1/callback`.',
    );
    expect(fillText(kind, {}, 'Open https://{zone}/admin', supplied)).toBe('Open https://{zone}/admin');
    expect(fillRefs(kind, {}, '{callback_url}', supplied)).toBe(supplied.callback_url);
  });

  it('lists what a provider’s way through asks on no step', () => {
    const splunk = kindSteps(kind, {}, 'splunk');
    expect(unasked(kind, splunk, {}).map((f) => f.key)).toEqual([]);
    const other = kindSteps(kind, { method: 'key' }, PRESET_OTHER);
    expect(unasked(kind, other, { method: 'key' }).map((f) => f.key)).toEqual(['zones']);
    expect(unasked({ ...kind, steps: null }, [], {})).toEqual([]);
  });
});
