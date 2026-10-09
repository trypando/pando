// The Sign-in screen's logic (issue #51): building a provider request from the
// form, and saying in words what a provider's revocation window and a test
// sign-in's outcome mean. Kept out of the component so it can be tested
// without rendering one.
//
// The form is built from the kinds GET /identity-providers returns, as the
// adapter form is from GET /adapters/kinds: nothing here knows what Okta or
// SAML takes (R-261).

import { boolValue, isShown } from './adapters';
import type { AdapterKind } from './adapters';

/** A known provider's starting settings. */
export interface Preset {
  id: string;
  label: string;
  help?: string;
  values?: Record<string, string>;
}

/** A kind of identity provider, with its presets. */
export interface ProviderKind extends AdapterKind {
  presets?: Preset[] | null;
}

/** A provider's revocation, as the server computes it (R-047, R-050). */
export interface Revocation {
  max_lifetime_seconds: number;
  mode: string;
  window_seconds: number;
}

/** How long, in the largest unit that reads naturally. */
export function duration(seconds: number): string {
  const plural = (n: number, unit: string) => `${n} ${unit}${n === 1 ? '' : 's'}`;
  if (seconds % 86400 === 0 && seconds >= 86400) return plural(seconds / 86400, 'day');
  if (seconds % 3600 === 0 && seconds >= 3600) return plural(seconds / 3600, 'hour');
  if (seconds % 60 === 0 && seconds >= 60) return plural(seconds / 60, 'minute');
  return plural(seconds, 'second');
}

/**
 * What removing someone at this provider does to their access here, and how
 * soon. Stated, never implied (design 06 §3.1): a provider without push keeps
 * people in for as long as their session lasts.
 */
export function revocationText(kind: string, r: Revocation | undefined): string {
  if (!r) return '';
  const session = `Sessions last ${duration(r.max_lifetime_seconds)}.`;
  if (kind === 'local') {
    return `${session} Suspending an account here ends its access within ${duration(r.window_seconds)}.`;
  }
  if (r.mode === 'push') {
    return `${session} The provider pushes changes through SCIM, so removing someone there ends their access here within ${duration(r.window_seconds)}.`;
  }
  return `${session} Removing someone at the provider ends their access here when their session ends: up to ${duration(r.window_seconds)}. Turn on SCIM to make it immediate.`;
}

/** The form: text for each field, a boolean for a bool. */
export type Values = Record<string, string | boolean>;

/** A preset's settings as the form's values. */
export function presetValues(kind: ProviderKind, preset: Preset | undefined): Values {
  const out: Values = {};
  for (const [key, value] of Object.entries(preset?.values ?? {})) {
    const field = (kind.fields ?? []).find((f) => f.key === key);
    out[key] = field?.type === 'bool' ? value === 'true' : value;
  }
  return out;
}

/** A provider's stored settings as the form's values. */
export function storedValues(config: unknown): Values {
  const out: Values = {};
  if (!config || typeof config !== 'object') return out;
  for (const [key, value] of Object.entries(config as Record<string, unknown>)) {
    if (typeof value === 'boolean') out[key] = value;
    else if (typeof value === 'string' || typeof value === 'number') out[key] = String(value);
  }
  return out;
}

/** A placeholder left in a value, such as "{tenant-id}". */
export function placeholderIn(values: Values): { field: string; placeholder: string } | null {
  for (const [field, v] of Object.entries(values)) {
    const m = typeof v === 'string' ? /\{[^}]+\}/.exec(v) : null;
    if (m) return { field, placeholder: m[0] };
  }
  return null;
}

/** What is wrong with the form, by field key. Empty when it can be sent. */
export function problems(kind: ProviderKind, name: string, values: Values, stored: string[] = []): Record<string, string> {
  const out: Record<string, string> = {};
  if (!name.trim()) out.name = 'A provider needs a name. People see it on the sign-in page.';
  for (const f of kind.fields ?? []) {
    if (f.type === 'bool' || !isShown(kind, f, values)) continue;
    const v = values[f.key];
    const text = typeof v === 'string' ? v.trim() : '';
    if (text === '' && f.required && !(f.credential && stored.includes(f.key))) {
      out[f.key] = `${f.label} is required.`;
    }
    const hole = /\{[^}]+\}/.exec(text);
    if (hole) out[f.key] = `Replace ${hole[0]} with your own value.`;
  }
  return out;
}

export interface ProviderRequest {
  kind?: string;
  name: string;
  config: Record<string, string | boolean>;
  credentials?: Record<string, string>;
  jit_provisioning: boolean;
  link_by_email: boolean;
  enabled?: boolean;
}

/**
 * The body of POST or PATCH /identity-providers. Secrets go in `credentials`
 * and nowhere else (R-190); an empty credential keeps the stored one. A bool
 * is sent only when it differs from its default.
 */
export function providerRequest(
  kind: ProviderKind,
  name: string,
  values: Values,
  flags: { jit: boolean; linkByEmail: boolean },
  creating: boolean,
): ProviderRequest {
  const config: Record<string, string | boolean> = {};
  const credentials: Record<string, string> = {};
  for (const f of kind.fields ?? []) {
    if (!isShown(kind, f, values)) continue;
    const v = values[f.key];
    if (f.type === 'bool') {
      const on = boolValue(f, v);
      if (on !== (f.default === 'true')) config[f.key] = on;
      continue;
    }
    const text = typeof v === 'string' ? v.trim() : '';
    if (text === '') continue;
    if (f.credential) credentials[f.key] = text;
    else config[f.key] = text;
  }
  const body: ProviderRequest = {
    name: name.trim(),
    config,
    jit_provisioning: flags.jit,
    link_by_email: flags.linkByEmail,
  };
  if (creating) body.kind = kind.kind;
  if (Object.keys(credentials).length > 0) body.credentials = credentials;
  return body;
}

/** A test sign-in's outcome, as the server reports it. */
export interface Outcome {
  kind: string;
  username?: string;
  display_name?: string;
  message?: string;
  remedy?: string;
  groups?: string[] | null;
  groups_from: string;
}

/** What a real sign-in would have done, in a sentence. */
export function outcomeText(o: Outcome | undefined): string {
  if (!o) return '';
  const who = o.display_name || o.username || 'this person';
  switch (o.kind) {
    case 'existing':
      return `A real sign-in would reach ${who}'s existing account.`;
    case 'linked_by_email':
      return `A real sign-in would link this identity to ${who}'s account by its verified email, and sign in there.`;
    case 'created':
      return `A real sign-in would create a new account for ${who}. It would have no access until it is given some, or is in a group that has some.`;
    default:
      return o.remedy ? `${o.message ?? ''} ${o.remedy}`.trim() : (o.message ?? 'A real sign-in would be refused.');
  }
}

/** Where a person's groups come from at this provider. */
export function groupsText(o: Outcome | undefined, groups: string[]): string {
  if (!o) return '';
  if (o.groups_from === 'scim') {
    return 'SCIM sets this provider’s group memberships, so the groups in the sign-in are not used.';
  }
  if (groups.length === 0) {
    return 'The sign-in carried no groups. Check the groups claim or attribute if you expected some.';
  }
  return `Each sign-in sets the person’s groups from this provider to: ${groups.join(', ')}.`;
}
