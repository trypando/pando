// The logic behind the onboarding page (AppOnboarding.tsx): which parts of a
// proposal an AI screening put there, and what the variables form sends with
// accept. Plain functions, so the mapping is tested rather than eyeballed.
//
// What the screening changed is read from `screening.applied` — the server's
// record of the amendments that landed (design 10 §5) — never inferred from the
// spec alone. The spec does carry a source on some entries (an env entry or a
// volume marked "screened"), but only the amendment carries the reason and the
// files it rests on, and those are what somebody checking the suggestion needs.

import type { Amendment, AppSpec, Outcome } from '@api/types.gen';
import { looksSensitive } from './sensitive';

/** The warning code an AI screener's notes carry (design 01 §2.8). */
export const SCREENING_ADVISORY = 'WARN_SCREENING_ADVISORY';

/** A usable environment variable name. The same pattern the server checks. */
const ENV_KEY = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** Where each applied amendment lands on the page, keyed by what it touched. */
export interface Suggestions {
  /** By workload name. */
  command: Record<string, Amendment>;
  /** By workload name. */
  port: Record<string, Amendment>;
  /** By `workload/KEY`. */
  env: Record<string, Amendment>;
  health?: Amendment;
  build: { context?: Amendment; dockerfile?: Amendment; static_dir?: Amendment };
  /** By slot key. */
  slots: Record<string, Amendment>;
  /** By mount path. */
  volumes: Record<string, Amendment>;
  /** By warning message. */
  warnings: Record<string, Amendment>;
  /** Questions the screening answered, which detection then stopped asking. */
  answers: Array<{ key: string; value: string; amendment?: Amendment }>;
  /** How many changes landed, for the line under the header. */
  count: number;
}

export function envKey(workload: string, key: string): string {
  return `${workload}/${key}`;
}

/** The workload an amendment with no workload means: the primary (design 10 §3). */
export function primaryWorkload(spec: AppSpec | undefined): string {
  const workloads = spec?.workloads ?? [];
  const primary = workloads.find((w) => w.primary);
  if (primary) return primary.name;
  return workloads.length === 1 ? workloads[0]!.name : '';
}

export function suggestions(outcome: Outcome | undefined, spec: AppSpec | undefined): Suggestions {
  const found: Suggestions = {
    command: {},
    port: {},
    env: {},
    build: {},
    slots: {},
    volumes: {},
    warnings: {},
    answers: [],
    count: 0,
  };
  if (!outcome?.ran) return found;

  const primary = primaryWorkload(spec);
  const where = (a: Amendment) => a.workload?.trim() || primary;

  for (const { amendment: a } of outcome.applied ?? []) {
    found.count++;
    switch (a.kind) {
      case 'set_command':
        found.command[where(a)] = a;
        break;
      case 'set_port':
        found.port[where(a)] = a;
        break;
      case 'set_env':
        found.env[envKey(where(a), (a.key ?? '').trim())] = a;
        break;
      case 'set_health':
        found.health = a;
        break;
      case 'set_build_context':
        found.build.context = a;
        break;
      case 'set_dockerfile':
        found.build.dockerfile = a;
        break;
      case 'set_static_dir':
        found.build.static_dir = a;
        break;
      case 'add_slot':
        found.slots[(a.key ?? '').trim()] = a;
        break;
      case 'add_volume':
        found.volumes[cleanPath(a.path ?? '')] = a;
        break;
      case 'add_warning':
        // The server writes the value as the warning's message, or the reason
        // when there was no value (screening/apply.go addWarning).
        found.warnings[(a.value ?? '').trim() || (a.reason ?? '').trim()] = a;
        break;
      case 'answer_question':
        // Counted here; listed from `outcome.answers` below, which is what
        // detection actually took.
        break;
    }
  }

  const applied = (outcome.applied ?? []).map((c) => c.amendment);
  for (const [key, value] of Object.entries(outcome.answers ?? {})) {
    const amendment = applied.find((a) => a.kind === 'answer_question' && (a.key ?? '').trim() === key);
    found.answers.push({ key, value, amendment });
  }
  return found;
}

/** A mount path as the server compares them: no trailing slash. */
function cleanPath(p: string): string {
  const trimmed = p.trim();
  return trimmed.length > 1 ? trimmed.replace(/\/+$/, '') : trimmed;
}

/** One row of the variables form. */
export interface VariableRow {
  /** Stable across edits, for React. */
  id: string;
  /** The workload it belongs to. Empty for a row added during review, which
   *  the server puts on the primary workload. */
  workload: string;
  key: string;
  value: string;
  secret: boolean;
  /** What detection put there. Absent for a row added during review. */
  original?: string;
  /**
   * The slot this variable is filled from, when it is one. A value typed for
   * it fills the slot at accept: as a secret (spec.FillSlotLiteral) or, left
   * plain, in the spec (spec.FillSlotValue).
   */
  slot?: RowSlot;
  /** The value came from the app's address (valueFromAddress), not the repo. */
  fromAddress?: boolean;
}

interface RowSlot {
  key: string;
  type: string;
  required: boolean;
  /** A service Pando runs — a database, a cache — rather than a bare value. */
  service: boolean;
  /** Pando creates it at the first deploy, and fills the variable then. */
  provisioned: boolean;
}

/**
 * The rows the form starts with: every variable the proposal declares that
 * a person could give a value — plain ones, and ones filled from a slot.
 *
 * A slot-filled variable is a row too. A key named with no value in
 * .env.example is a slot the deploy is refused without (R-132), and the
 * review is where somebody has the value to hand; a database's URL can be
 * pointed at one they already run instead of the one Pando would create. One
 * already filled from a secret is not something detection produces. A name
 * that reads like a credential starts as a secret when it has no value yet —
 * the same default the Environment tab takes.
 */
export function variableRows(spec: AppSpec | undefined, address?: string): VariableRow[] {
  const slots = spec?.slots ?? [];
  return (spec?.workloads ?? []).flatMap((w) =>
    (w.env ?? [])
      .filter((e) => !e.secret_ref)
      .map((e) => {
        // A slot already filled with a readable value — by an AI adapter,
        // most often — shows it; one filled with a secret cannot.
        const slotOf = e.slot_ref ? slots.find((s) => s.key === e.slot_ref) : undefined;
        const value = e.value ?? (slotOf?.resolution?.mode === 'bound' ? (slotOf.resolution.target ?? '') : '');
        const row: VariableRow = {
          id: envKey(w.name, e.key),
          workload: w.name,
          key: e.key,
          value,
          secret: value === '' && looksSensitive(e.key),
          original: value,
        };
        if (e.slot_ref) {
          const slot = slots.find((s) => s.key === e.slot_ref);
          const service = Boolean(slot && slot.type && slot.type !== 'unknown');
          row.slot = {
            key: e.slot_ref,
            type: slot?.type ?? 'unknown',
            required: Boolean(slot?.required),
            service,
            provisioned: slot?.resolution?.mode === 'provisioned',
          };
          // A default, not a lock: a service's address carries its password,
          // so it starts secret; anything else starts secret when its name
          // reads like a credential. A value left plain is stored in the spec
          // where it can be read back (spec.FillSlotValue).
          row.secret = service || row.secret;
        }
        // The app's own URL or domain, from where Pando will serve it: a
        // default the person can change, sent with the accept like any value
        // they set (its original stays empty).
        const fromAddress = row.value === '' && !row.slot?.service ? valueFromAddress(row.key, address) : undefined;
        if (fromAddress) {
          row.value = fromAddress;
          row.secret = false;
          row.fromAddress = true;
        }
        return row;
      }),
  );
}

/**
 * The app's address as a full URL with no trailing slash. The server writes a
 * port-mode address protocol-relative ("//localhost:9003/"), because it cannot
 * know how the browser reached it; the browser can.
 */
export function absoluteAddress(address: string | undefined, protocol = 'http:'): string | undefined {
  if (!address) return undefined;
  const full = address.startsWith('//') ? `${protocol}${address}` : address;
  if (!/^https?:\/\//.test(full)) return undefined;
  return full.replace(/\/+$/, '');
}

// Names that mean "this app's own public URL" or "its own domain". Matched on
// the last part of the name, so APP_BASE_URL and NEXTAUTH_URL are URLs and
// APP_DOMAIN is a domain. HOST is not here: it is the address an app binds to.
const OWN_URL = /(^|_)(BASE_URL|PUBLIC_URL|APP_URL|SITE_URL|EXTERNAL_URL|ROOT_URL|ORIGIN|NEXTAUTH_URL)$/;
const OWN_DOMAIN = /(^|_)(DOMAIN|HOSTNAME|PUBLIC_HOST|SERVER_NAME)$/;

/**
 * What a variable should hold when its name says it is the app's own URL or
 * domain, from the address Pando will serve the app at — undefined for any
 * other name. A person deploying a generated app rarely knows what "domain"
 * means here, and Pando does.
 */
function valueFromAddress(key: string, address: string | undefined): string | undefined {
  if (!address) return undefined;
  if (OWN_URL.test(key)) return address;
  if (OWN_DOMAIN.test(key)) {
    try {
      return new URL(address).hostname;
    } catch {
      return undefined;
    }
  }
  return undefined;
}

/**
 * A random value for a variable that just needs one: an encryption key, a
 * session secret, a password nobody types. 32 bytes from the browser's
 * cryptographic generator, as URL-safe base64 with no padding — 43 characters
 * that survive a shell, a URL and a .env file unquoted.
 */
export function randomSecret(bytes = 32): string {
  const raw = new Uint8Array(bytes);
  crypto.getRandomValues(raw);
  let binary = '';
  for (const b of raw) binary += String.fromCharCode(b);
  return btoa(binary).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
}

/**
 * Slot-filled values a deploy would be refused without (R-132): required, not
 * created by Pando, not marked optional by the person, and empty.
 */
export function neededValues(rows: VariableRow[], optional: Set<string> = new Set()): VariableRow[] {
  return rows.filter(
    (r) => r.slot && r.slot.required && !optional.has(r.slot.key) && !r.slot.provisioned && !r.value.trim(),
  );
}

/** What the person changed on a detected row, kept apart from the rows. */
export interface RowEdit {
  value?: string;
  secret?: boolean;
}

/**
 * The form's rows: what detection has found so far, with the person's edits
 * laid over it, then the rows they added.
 *
 * Edits are held by row id (workload and name) rather than as a copy of the
 * rows, because the proposal keeps arriving while somebody types: the trial run
 * and the screening both finish after the variables first appear. A copy taken
 * at the first poll would drop whatever the screening added; edits keyed by
 * name survive every later poll, and a value somebody typed wins over one the
 * screening proposed for the same variable.
 */
export function mergeRows(detected: VariableRow[], edits: Record<string, RowEdit>, added: VariableRow[]): VariableRow[] {
  return [
    ...detected.map((row) => {
      const edit = edits[row.id];
      if (!edit) return row;
      return {
        ...row,
        value: edit.value ?? row.value,
        secret: edit.secret ?? row.secret,
        // A value the person typed over is theirs, not the address's.
        fromAddress: row.fromAddress && (edit.value === undefined || edit.value === row.value),
      };
    }),
    ...added,
  ];
}

/** What accept sends for each variable (POST /apps/{id}/detection/accept). */
export interface ValueInput {
  key: string;
  value: string;
  secret?: boolean;
  workload?: string;
}

/**
 * The `values` accept carries: only what the person changed.
 *
 * A row left as detection found it is not sent, so accepting without touching
 * the form pins exactly the proposal. An empty secret is not sent either —
 * storing nothing as a secret would put a reference in the spec that resolves
 * to an empty string, and read as though the value had been set.
 */
export function valuesPayload(rows: VariableRow[]): ValueInput[] {
  const out: ValueInput[] = [];
  for (const row of rows) {
    const key = row.key.trim();
    if (!key) continue;
    if (row.original === undefined) {
      if (row.value === '') continue;
    } else if (row.value === row.original && !(row.secret && row.value !== '')) {
      continue;
    }
    if (row.secret && row.value === '') continue;

    const input: ValueInput = { key, value: row.value };
    if (row.secret) input.secret = true;
    if (row.workload) input.workload = row.workload;
    out.push(input);
  }
  return out;
}

/**
 * Why a row cannot be sent, by row id. Only rows added during review are
 * checked: a detected row's name came from the repository and is not editable.
 */
export function rowProblems(rows: VariableRow[]): Record<string, string> {
  const problems: Record<string, string> = {};
  const detected = new Set(rows.filter((r) => r.original !== undefined).map((r) => r.key));
  const seen = new Set<string>();
  for (const row of rows) {
    if (row.original !== undefined) continue;
    const key = row.key.trim();
    if (!key) {
      if (row.value !== '') problems[row.id] = 'Give this variable a name.';
      continue;
    }
    if (!ENV_KEY.test(key)) {
      problems[row.id] = 'A name uses letters, digits and underscores, and does not start with a digit.';
    } else if (detected.has(key)) {
      problems[row.id] = `${key} is already in the list above. Set its value there.`;
    } else if (seen.has(key)) {
      problems[row.id] = `${key} is in the list twice.`;
    }
    seen.add(key);
  }
  return problems;
}
