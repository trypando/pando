// An app's egress, as the console reads and writes it (R-182 – R-188).
//
// What the rules *are* is the server's answer — `GET /apps/{id}/egress` merges
// them, says where each part came from and which parts loosen the
// installation's (policy.EgressFor), and a dry-run save says the same of a
// change before it is made. Nothing here classifies a change: this file only
// puts those answers into words and turns the page's list back into the
// spec's `egress` object.

/** The spec's `egress` object (spec.Egress). */
export interface EgressSpec {
  mode?: string;
  list?: string[] | null;
  add?: string[] | null;
  remove?: string[] | null;
  /** Absent keeps the installation's switch; true turns blocking on; false off. */
  block_private?: boolean | null;
  /** Before issue #79. Read as `list`, never written. */
  allowlist?: string[] | null;
}

export interface Loosening {
  kind: string;
  entry?: string;
  message: string;
}

/** policy.EffectiveEgress: the rules an app runs with, merged (R-188). */
interface EffectiveEgress {
  mode: string;
  list?: Array<{ entry: string; from: string }> | null;
  app_mode?: string;
  app_list?: string[] | null;
  block_private: boolean;
  block_private_from?: string;
  loosenings?: Loosening[] | null;
  gate?: string;
  unused?: string[] | null;
  restricted: boolean;
}

interface InstallEgress {
  mode?: string;
  list?: string[] | null;
  block_private?: boolean;
  loosening?: string;
}

export interface EgressResponse {
  install: InstallEgress;
  effective: EffectiveEgress | null;
  spec: EgressSpec | null;
}

export type InstallMode = 'allow_all' | 'denylist' | 'allowlist';

export function installMode(mode: string | undefined): InstallMode {
  return mode === 'denylist' || mode === 'allowlist' ? mode : 'allow_all';
}

/** The page's fields. */
export interface EgressDraft {
  add: string[];
  remove: string[];
  ownMode: 'inherit' | 'allowlist' | 'denylist';
  ownList: string[];
  blockPrivate: 'inherit' | 'on' | 'off';
}

/**
 * The page's starting point, from the spec — read as the server's Normalize
 * reads it, so a spec written before issue #79 opens as what it now means: an
 * old allowlist is the app's own list, and the old block_private mode is the
 * switch turned on.
 */
export function draftOf(spec: EgressSpec | null | undefined): EgressDraft {
  const s = spec ?? {};
  const ownMode = s.mode === 'allowlist' || s.mode === 'denylist' ? s.mode : 'inherit';
  const list = (s.list ?? []).length > 0 ? (s.list ?? []) : (s.allowlist ?? []);
  let blockPrivate: EgressDraft['blockPrivate'] = 'inherit';
  if (s.block_private === true || (s.mode === 'block_private' && s.block_private == null)) blockPrivate = 'on';
  else if (s.block_private === false) blockPrivate = 'off';
  return {
    add: s.add ?? [],
    remove: s.remove ?? [],
    ownMode,
    ownList: ownMode === 'inherit' ? [] : list,
    blockPrivate,
  };
}

/** The spec's `egress` object for a draft. Empty parts are left out. */
export function specOf(d: EgressDraft): EgressSpec {
  const out: EgressSpec = { mode: d.ownMode };
  if (d.ownMode !== 'inherit' && d.ownList.length > 0) out.list = d.ownList;
  if (d.add.length > 0) out.add = d.add;
  if (d.remove.length > 0) out.remove = d.remove;
  if (d.blockPrivate !== 'inherit') out.block_private = d.blockPrivate === 'on';
  return out;
}

/** Whether a draft says the same as the spec it started from. */
export function sameDraft(a: EgressDraft, b: EgressDraft): boolean {
  return JSON.stringify(specOf(a)) === JSON.stringify(specOf(b));
}

/**
 * What `POST /apps/{id}/specs?dry_run=true` answers for a draft the server
 * would accept: the same checks a save runs, with nothing written. A draft it
 * would refuse comes back as the save's own refusal.
 */
export interface DryRunResult {
  dry_run: true;
  egress: EffectiveEgress;
  egress_changed: boolean;
  new_loosenings: Loosening[];
  approval: { required: boolean; reasons: Array<{ reason: string; message: string }> };
}

/**
 * What a loosening needs before it runs, by policy's gate (R-183): said beside
 * the address it loosens. One a verb allows needs nothing more of whoever
 * holds it, so it says only that it loosens.
 */
export function looseningNote(gate: string | undefined): string {
  switch (gate) {
    case 'forbidden':
      return 'Not allowed on this host';
    case 'approval':
      return 'Needs approval to deploy';
    default:
      return 'Loosens this host’s rules';
  }
}

/** Where an address in the list comes from, and so what can be done to it here. */
type LineFrom = 'host' | 'added' | 'own' | 'excluded';

/** One address in the page's single list. */
export interface EgressLine {
  entry: string;
  from: LineFrom;
  /** It loosens the host's rules (R-182). */
  loosens: boolean;
}

/** What the host's rules are, as the line that opens the page. */
export function hostLine(mode: InstallMode): string {
  if (mode === 'allowlist') return 'This host only allows the addresses below.';
  if (mode === 'denylist') return 'This host blocks the addresses below.';
  return 'This host allows any address.';
}

/** What the app's own list does, said under the host's line; null without one. */
export function ownLine(draft: EgressDraft): string | null {
  if (draft.ownMode === 'allowlist') return 'This app only connects to the addresses it allows.';
  if (draft.ownMode === 'denylist') return 'This app never connects to the addresses it blocks.';
  return null;
}

/**
 * One list of addresses: the host's, read-only here, then what this app adds
 * and keeps of its own. The host's are listed only where its mode has a list.
 */
export function egressLines(install: InstallEgress, draft: EgressDraft, loosenings: Loosening[]): EgressLine[] {
  const loose = new Set(loosenings.map((l) => l.entry ?? ''));
  const excluded = new Set(draft.remove);
  const host: EgressLine[] =
    installMode(install.mode) === 'allow_all'
      ? []
      : (install.list ?? []).map((entry) => ({
          entry,
          from: excluded.has(entry) ? 'excluded' : 'host',
          loosens: excluded.has(entry) && loose.has(entry),
        }));
  const added: EgressLine[] = draft.add.map((entry) => ({ entry, from: 'added', loosens: loose.has(entry) }));
  const own: EgressLine[] =
    draft.ownMode === 'inherit' ? [] : draft.ownList.map((entry) => ({ entry, from: 'own', loosens: false }));
  return [...host, ...added, ...own];
}

/** Whether nothing restricts the app at all: no host list, nothing of its own. */
export function unrestricted(install: InstallEgress, draft: EgressDraft): boolean {
  return (
    installMode(install.mode) === 'allow_all' &&
    draft.ownMode === 'inherit' &&
    draft.add.length === 0 &&
    draft.remove.length === 0
  );
}

export type AddAction = 'allow' | 'block';

/**
 * The buttons an address can be added with, by where the rules stand. With a
 * list of its own the app adds to that; otherwise to the host's — allowing
 * under an allowlist, which loosens it, and blocking under a denylist. With
 * neither, either button starts the app's own list.
 */
export function addActions(mode: InstallMode, draft: EgressDraft): AddAction[] {
  if (draft.ownMode === 'allowlist') return ['allow'];
  if (draft.ownMode === 'denylist') return ['block'];
  if (mode === 'allowlist') return ['allow'];
  if (mode === 'denylist') return ['block'];
  return ['allow', 'block'];
}

/** The draft with an address added the way its button says. */
export function withEntry(mode: InstallMode, draft: EgressDraft, entry: string, action: AddAction): EgressDraft {
  if (draft.ownMode !== 'inherit') return { ...draft, ownList: [...draft.ownList, entry] };
  if (mode !== 'allow_all') return { ...draft, add: [...draft.add, entry] };
  return { ...draft, ownMode: action === 'allow' ? 'allowlist' : 'denylist', ownList: [entry] };
}

/** The draft without one of the app's addresses. Its own list, emptied, is no list. */
export function withoutEntry(draft: EgressDraft, line: EgressLine): EgressDraft {
  if (line.from === 'added') return { ...draft, add: draft.add.filter((e) => e !== line.entry) };
  if (line.from === 'excluded') return { ...draft, remove: draft.remove.filter((e) => e !== line.entry) };
  if (line.from !== 'own') return draft;
  const ownList = draft.ownList.filter((e) => e !== line.entry);
  return ownList.length === 0 ? { ...draft, ownMode: 'inherit', ownList } : { ...draft, ownList };
}

/** Whether private addresses are blocked for the app, by its draft and the host. */
export function blocksPrivate(install: InstallEgress, draft: EgressDraft): boolean {
  if (draft.blockPrivate === 'on') return true;
  if (draft.blockPrivate === 'off') return false;
  return Boolean(install.block_private);
}
