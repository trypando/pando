// Adding and changing an adapter from the console — the form's logic, kept out
// of the component so it can be tested without rendering one.
//
// The form is built from GET /adapters/kinds, which each adapter package fills
// in about itself (R-254: capabilities, and settings, are data). Nothing here
// knows what an Anthropic or a Traefik adapter takes; a kind added to Pando is
// configurable here without this file changing (R-261).

/** One choice a `select` setting offers. */
interface KindOption {
  value: string;
  label: string;
  /** What choosing it means, shown under a radio button. */
  description?: string;
}

/** One setting a kind of adapter takes. */
export interface KindField {
  key: string;
  label: string;
  help?: string;
  type: 'string' | 'int' | 'bool' | 'select';
  required?: boolean;
  /** A secret such as an API key: sent in `credentials`, never in `config`
   *  (R-190), and never shown again. */
  credential?: boolean;
  /** What the adapter uses when the setting is left empty. Shown in the
   *  empty field, so what it says is what happens. */
  default?: string;
  /** An example, for a setting with no default. */
  placeholder?: string;
  /** What a `select` offers. */
  options?: KindOption[];
  /** A `select` that also takes a value it does not list, typed in. */
  other?: boolean;
  /** Asks for a text area, such as credentials given as NAME=value lines. */
  multiline?: boolean;
  /** Shown only while another setting has one of these values. */
  shown_when?: { key: string; values: string[] };
  /** A setting most people never change, asked for under "Advanced
   *  settings". Never required, and always with a default. */
  advanced?: boolean;
}

/** Whether any advanced setting is set to something other than its default,
 *  so a form opens them rather than hiding what is configured. */
export function advancedChanged(kind: AdapterKind, values: Record<string, string | boolean>): boolean {
  return (kind.fields ?? []).some((f) => {
    if (!f.advanced || !(f.key in values)) return false;
    if (f.type === 'bool') return boolValue(f, values[f.key]) !== (f.default === 'true');
    const text = String(values[f.key]).trim();
    return text !== '' && text !== f.default;
  });
}

/** A bool setting as it stands: what was set, or the kind's default. */
export function boolValue(f: KindField, v: string | boolean | undefined): boolean {
  return typeof v === 'boolean' ? v : f.default === 'true';
}

/** A setting as the adapter will read it, as text: what was entered, or the
 *  kind's default. A bool is "true" or "false". */
export function effectiveText(f: KindField, v: string | boolean | undefined): string {
  if (f.type === 'bool') return String(boolValue(f, v));
  const text = typeof v === 'string' ? v.trim() : '';
  return text || f.default || '';
}

/** Whether a setting applies, given the others. A hidden one is neither
 *  checked nor sent: a DNS provider means nothing without DNS certificates. */
export function isShown(kind: AdapterKind, f: KindField, values: Record<string, string | boolean>): boolean {
  if (!f.shown_when) return true;
  const on = (kind.fields ?? []).find((x) => x.key === f.shown_when!.key);
  if (!on) return true;
  return f.shown_when.values.includes(effectiveText(on, values[on.key]));
}

/** The text an empty field shows: the default when there is one, since
 *  that is what leaving it empty gets, and an example otherwise. */
export function fieldPlaceholder(f: KindField): string | undefined {
  return f.default || f.placeholder;
}

/** A kind of adapter this build of Pando can run. */
export interface AdapterKind {
  category: string;
  kind: string;
  name: string;
  description: string;
  id_prefix: string;
  fields: KindField[] | null;
}

/** What the form holds: text for every field, whatever its type, and a
 *  boolean for a bool — so a half-typed number is kept as typed. */
export interface AdapterForm {
  id: string;
  name: string;
  isDefault: boolean;
  values: Record<string, string | boolean>;
}

export interface AdapterRequest {
  id: string;
  category: string;
  kind: string;
  name: string;
  config: Record<string, string | number | boolean>;
  credentials?: Record<string, string>;
  is_default: boolean;
  enabled?: boolean;
}

/** The key a kind is chosen by in the form's select. */
export function kindKey(k: { category: string; kind: string }): string {
  return `${k.category}/${k.kind}`;
}

/** Category names as a person reads them. `ai` is an initialism; the rest are
 *  words, in sentence case like everything else. */
export function categoryLabel(category: string): string {
  if (category === 'ai') return 'AI';
  if (category === 'image_registry') return 'Image registry';
  if (category === 'audit_sink') return 'Audit sink';
  return category.charAt(0).toUpperCase() + category.slice(1);
}

// The categories in the order an installation is built up: where apps run and
// how they are reached first, then what they are built with and what they
// lean on, then the extras. One not listed here sorts after these, by name.
const CATEGORY_ORDER = ['runtime', 'routing', 'builder', 'image_registry', 'services', 'secrets', 'backup', 'scanner', 'ai', 'notify', 'audit_sink', 'identity'];

const CATEGORY_NOTES: Record<string, string> = {
  runtime: 'Where apps run: it starts, stops and watches the containers each app is made of.',
  routing: 'How apps are reached: their addresses, and the edge that sends requests through Pando to them.',
  builder: 'What turns an app\u2019s source into an image Pando can run.',
  image_registry:
    'Where Pando pushes built images when a runtime pulls them rather than taking them directly. A single Docker host needs none. Used from the moment it is saved.',
  services: 'The databases and caches apps declare, such as PostgreSQL or Redis, provisioned beside each app.',
  secrets: 'Where secret values are kept, encrypted. Pando stores what this hands back, never the plain value.',
  backup: 'Where backups of Pando and of each app\u2019s storage are written.',
  scanner: 'What checks apps for known vulnerabilities, for the security score.',
  ai: 'An AI provider, such as Anthropic. It performs the AI functions chosen on it: repairing plans, drafting access and policy, searching the audit log, answering from the reference.',
  notify: 'Where notifications go, such as the console itself.',
  audit_sink:
    'Where a copy of the audit log goes: every audit event is sent off the installation as it is written, to a syslog collector or a SIEM\u2019s HTTPS endpoint. Adding one needs the install.audit.export permission, and the Audit log screen shows each one.',
  identity: 'Where accounts come from and how people sign in.',
  source: 'How Pando reads private repositories: a connection to GitHub, GitLab, Azure DevOps, Bitbucket, Gitea or any git host.',
};

/** One line on what a category of adapter is for. */
export function categoryNote(category: string): string {
  return CATEGORY_NOTES[category] ?? '';
}

/** Categories in the order the screen shows them. */
export function orderCategories(categories: Iterable<string>): string[] {
  const rank = (c: string) => {
    const i = CATEGORY_ORDER.indexOf(c);
    return i >= 0 ? i : CATEGORY_ORDER.length;
  };
  return [...new Set(categories)].sort((a, b) => rank(a) - rank(b) || a.localeCompare(b));
}

/** The kinds in the order the select offers them: by category, then by name. */
export function sortKinds(kinds: AdapterKind[]): AdapterKind[] {
  return [...kinds].sort(
    (a, b) => categoryLabel(a.category).localeCompare(categoryLabel(b.category)) || a.name.localeCompare(b.name),
  );
}

/** A new adapter's form, prefilled from its kind. */
export function blankForm(kind: AdapterKind, firstOfCategory: boolean): AdapterForm {
  return {
    id: `${kind.id_prefix}${kind.kind}`,
    name: kind.name,
    // The first adapter of a category is the one anything that needs that
    // category will use, so it is the default unless someone says otherwise.
    isDefault: firstOfCategory,
    values: {},
  };
}

/**
 * What is wrong with the form, by field key, in words that say how to fix it.
 * Empty when it can be sent.
 *
 * `credentialsSet` names the credentials already stored for the adapter being
 * changed: a required one of those may be left empty, which keeps it.
 */
export function formProblems(
  kind: AdapterKind,
  form: AdapterForm,
  credentialsSet: string[] = [],
): Record<string, string> {
  const problems: Record<string, string> = {};
  if (!form.id.trim()) problems.id = `An adapter needs an ID, for example ${kind.id_prefix}${kind.kind}.`;
  for (const f of kind.fields ?? []) {
    const v = form.values[f.key];
    if (f.type === 'bool' || !isShown(kind, f, form.values)) continue;
    const text = typeof v === 'string' ? v.trim() : '';
    if (text === '') {
      const kept = f.credential && credentialsSet.includes(f.key);
      if (f.required && !kept) problems[f.key] = `${f.label} is required.`;
      continue;
    }
    if (f.type === 'int' && !/^-?\d+$/.test(text)) {
      problems[f.key] = `${f.label} takes a whole number, such as ${[f.default, f.placeholder].find((x) => x && /^\d+$/.test(x)) ?? '30'}.`;
    }
  }
  return problems;
}

/**
 * The body of POST /adapters for a form.
 *
 * Credentials go in `credentials` and nowhere else — the server refuses them in
 * `config`, which is stored in the clear (R-190). An empty field is left out:
 * an empty credential keeps the stored one (a "" would remove it), and an empty
 * setting is the adapter's own default. Ints go as JSON numbers, because the
 * adapter reads its config as typed JSON. A bool is sent only when it differs
 * from its default, since the default is what an absent one means — which for
 * most is off, and for some, such as Pando running Traefik, is on. A setting
 * hidden by another's value is not sent.
 *
 * Call formProblems first; a field it would refuse is not sent correctly here.
 */
export function adapterRequest(kind: AdapterKind, form: AdapterForm, enabled?: boolean): AdapterRequest {
  const config: AdapterRequest['config'] = {};
  const credentials: Record<string, string> = {};
  for (const f of kind.fields ?? []) {
    const v = form.values[f.key];
    if (!isShown(kind, f, form.values)) continue;
    if (f.type === 'bool') {
      const on = boolValue(f, v);
      if (on !== (f.default === 'true')) config[f.key] = on;
      continue;
    }
    const text = typeof v === 'string' ? v.trim() : '';
    if (text === '') continue;
    if (f.credential) credentials[f.key] = text;
    else if (f.type === 'int') config[f.key] = parseInt(text, 10);
    else config[f.key] = text;
  }

  const body: AdapterRequest = {
    id: form.id.trim(),
    category: kind.category,
    kind: kind.kind,
    name: form.name.trim() || kind.name,
    config,
    is_default: form.isDefault,
  };
  if (Object.keys(credentials).length > 0) body.credentials = credentials;
  // Sent when changing an existing adapter, so saving one that was turned off
  // does not quietly turn it back on: the server's default is enabled.
  if (enabled !== undefined) body.enabled = enabled;
  return body;
}
