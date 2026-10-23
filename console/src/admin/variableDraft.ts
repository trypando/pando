// What the Variables page of an app's Settings edits, and what saving it
// writes. Plain functions, so the mapping from a draft to a revision is tested
// rather than eyeballed.
//
// Two kinds of value, and the difference is not cosmetic. A plain value goes
// into the spec, which is exportable and meant to be safe to hand to somebody
// (design 01 §5). A secret goes through the secrets adapter and the spec
// carries only a reference (R-190, R-191). A stored secret's value is never
// read back (R-194): its row shows dots, and typing replaces it.

import type { AppSpec, EnvEntry, Workload } from '@api/types.gen';
import { looksSensitive } from './sensitive';

/** One variable as the page holds it. */
export interface SettingsVar {
  /** `workload/KEY` for one the spec has; `added-N` for one added here. */
  id: string;
  workload: string;
  key: string;
  /** The plain value; empty for a stored secret, whose value is not read. */
  value: string;
  secret: boolean;
  /** A secret the spec already refers to. */
  stored: boolean;
  /** Filled from this dependency (slot) rather than given a value. */
  slot?: string;
  /** Added on this page and not saved yet. */
  added?: boolean;
}

/** What the person changed, kept apart from what the spec says. */
export interface VarDraft {
  edits: Record<string, { value?: string; secret?: boolean }>;
  removed: string[];
  added: SettingsVar[];
}

export const emptyDraft: VarDraft = { edits: {}, removed: [], added: [] };

/** A usable environment variable name. The same pattern the server checks. */
const ENV_KEY = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** Every variable the spec declares, by workload, in the spec's order. */
export function specVars(spec: AppSpec | undefined): SettingsVar[] {
  return (spec?.workloads ?? []).flatMap((w: Workload) =>
    (w.env ?? []).map((e: EnvEntry) => ({
      id: `${w.name}/${e.key}`,
      workload: w.name,
      key: e.key,
      value: e.secret_ref ? '' : (e.value ?? ''),
      secret: Boolean(e.secret_ref),
      stored: Boolean(e.secret_ref),
      ...(e.slot_ref ? { slot: e.slot_ref } : {}),
    })),
  );
}

/** The page's rows: the spec's, with the draft laid over them. */
export function draftVars(base: SettingsVar[], draft: VarDraft): SettingsVar[] {
  const removed = new Set(draft.removed);
  return [
    ...base
      .filter((v) => !removed.has(v.id))
      .map((v) => {
        const edit = draft.edits[v.id];
        return edit ? { ...v, value: edit.value ?? v.value, secret: edit.secret ?? v.secret } : v;
      }),
    ...draft.added,
  ];
}

/** Whether an edit leaves a variable as the spec has it. */
function unchanged(v: SettingsVar, edit: VarDraft['edits'][string] | undefined): boolean {
  if (!edit) return true;
  const value = edit.value ?? v.value;
  const secret = edit.secret ?? v.secret;
  // A stored secret with nothing typed keeps its value.
  return value === v.value && secret === v.secret;
}

/** How many changes the draft holds, for the Save bar. */
export function changeCount(base: SettingsVar[], draft: VarDraft): number {
  const removed = new Set(draft.removed);
  const edited = base.filter((v) => !removed.has(v.id) && !unchanged(v, draft.edits[v.id])).length;
  const added = draft.added.filter((v) => v.key.trim() !== '' || v.value !== '').length;
  return edited + removed.size + added;
}

/** A new variable in a workload, as Add variable makes one. */
export function newVar(id: string, workload: string): SettingsVar {
  return { id, workload, key: '', value: '', secret: false, stored: false, added: true };
}

/**
 * Whether a plain value's name reads like a credential. Warned, not
 * prevented: which variables are credentials is the person's knowledge, but a
 * key stored as a plain value is in every export of the app (R-190, R-191).
 */
export function looksLikeCredential(v: Pick<SettingsVar, 'key' | 'secret'>): boolean {
  return !v.secret && looksSensitive(v.key);
}

/** Why a row can't be saved, by row id. Saving waits until there are none. */
export function varProblems(rows: SettingsVar[]): Record<string, string> {
  const problems: Record<string, string> = {};
  const seen = new Set<string>();
  for (const row of rows) {
    const key = row.key.trim();
    const name = `${row.workload}/${key}`;
    if (row.added) {
      if (!key) {
        problems[row.id] = 'Give this variable a name.';
        continue;
      }
      if (!ENV_KEY.test(key)) {
        problems[row.id] = 'A name uses letters, digits and underscores, and does not start with a digit.';
      } else if (seen.has(name)) {
        problems[row.id] = `${row.workload} already has ${key}.`;
      }
    }
    seen.add(name);
    // A stored secret's value is not read back, so it can't become plain
    // without being typed again.
    if (row.stored && !row.secret && row.value === '') {
      problems[row.id] = `Type ${key}'s value to store it as a plain value.`;
    }
  }
  return problems;
}

/** One write a save makes to the secrets adapter before the revision. */
export interface SecretWrite {
  key: string;
  value: string;
}

/**
 * The revision a save writes, and the secrets it stores first. Variables
 * filled from a dependency and untouched variables are carried over as the
 * spec has them, source and all.
 */
export function saveVars(
  spec: AppSpec,
  base: SettingsVar[],
  draft: VarDraft,
): { body: AppSpec; secrets: SecretWrite[] } {
  const removed = new Set(draft.removed);
  const secrets: SecretWrite[] = [];
  const entryFor = (v: SettingsVar, value: string, secret: boolean, prior?: EnvEntry): EnvEntry => {
    if (secret) {
      if (value !== '') secrets.push({ key: v.key, value });
      else if (prior?.secret_ref) return prior;
      return { key: v.key, secret_ref: v.key, source: 'user' };
    }
    return { key: v.key, value, source: 'user' };
  };

  const workloads = (spec.workloads ?? []).map((w: Workload) => {
    const env: EnvEntry[] = [];
    for (const e of w.env ?? []) {
      const id = `${w.name}/${e.key}`;
      if (removed.has(id)) continue;
      const v = base.find((b) => b.id === id);
      const edit = draft.edits[id];
      if (!v || unchanged(v, edit)) {
        env.push(e);
        continue;
      }
      env.push(entryFor(v, edit?.value ?? v.value, edit?.secret ?? v.secret, e));
    }
    for (const v of draft.added) {
      if (v.workload !== w.name || v.key.trim() === '') continue;
      env.push(entryFor({ ...v, key: v.key.trim() }, v.value, v.secret));
    }
    return { ...w, env };
  });
  return { body: { ...spec, workloads }, secrets };
}
