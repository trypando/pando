// An app's egress, as the console reads and writes it (R-182 – R-188).
//
// What the rules *are* is the server's answer — `GET /apps/{id}/egress` merges
// them, says where each part came from and which parts loosen the
// installation's (policy.EgressFor). Nothing here classifies a change: this
// file only puts that answer into words and turns the editor's fields back
// into the spec's `egress` object.

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

interface Loosening {
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

/** The effective mode, in words. */
export function modeWords(mode: string | undefined): string {
  switch (installMode(mode)) {
    case 'denylist':
      return 'Anywhere except the destinations listed';
    case 'allowlist':
      return 'Only the destinations listed';
    default:
      return 'Anywhere';
  }
}

/** The app's own list, in words. */
export function appModeWords(mode: string | undefined): string | null {
  if (mode === 'allowlist') return 'On top of that, only these';
  if (mode === 'denylist') return 'On top of that, never these';
  return null;
}

/** What a loosening needs, by policy's gate (R-183). */
export function gateWords(gate: string | undefined): { status: 'failed' | 'info'; label: string; detail: string } {
  switch (gate) {
    case 'forbidden':
      return {
        status: 'failed',
        label: 'Not allowed',
        detail: 'The installation’s policy doesn’t allow loosening its rules. The next deploy is refused until this is taken out.',
      };
    case 'approval':
      return {
        status: 'info',
        label: 'Needs a deploy approval',
        detail: 'The deploy that carries it waits until it is approved.',
      };
    default:
      return {
        status: 'info',
        label: 'Needs app.egress.loosen',
        detail: 'Allowed to somebody holding app.egress.loosen on this app.',
      };
  }
}

const fromLabels: Record<string, string> = { app: 'This app', install: 'Installation' };

export function fromWords(from: string | undefined): string {
  return (from && fromLabels[from]) ?? '';
}

/** The editor's fields. */
export interface EgressDraft {
  add: string[];
  remove: string[];
  ownMode: 'inherit' | 'allowlist' | 'denylist';
  ownList: string[];
  blockPrivate: 'inherit' | 'on' | 'off';
}

/**
 * The editor's starting point, from the spec — read as the server's
 * Normalize reads it, so a spec written before issue #79 opens as what it now
 * means: an old allowlist is the app's own list, and the old block_private
 * mode is the switch turned on.
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
 * The two edits to the installation's list, named for what each does under
 * its mode, and whether that edit loosens (R-182). Under allow-all neither
 * does anything, which the server reports as unused.
 */
export function listEdits(mode: InstallMode): {
  add: { label: string; loosens: boolean };
  remove: { label: string; loosens: boolean };
} | null {
  if (mode === 'allowlist') {
    return {
      add: { label: 'Also allow', loosens: true },
      remove: { label: 'Don’t allow, though the installation does', loosens: false },
    };
  }
  if (mode === 'denylist') {
    return {
      add: { label: 'Also block', loosens: false },
      remove: { label: 'Allow, though the installation blocks it', loosens: true },
    };
  }
  return null;
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
 * The server's decision on a draft, as sentences: what it newly loosens, and
 * whether deploying it would need approval. Empty when the draft changes
 * nothing worth saying.
 */
export function dryRunWords(r: DryRunResult): string[] {
  const out: string[] = [];
  if (r.new_loosenings.length === 0) {
    if (r.egress_changed) out.push('This stays within the installation’s rules.');
  } else {
    out.push(...r.new_loosenings.map((l) => l.message));
  }
  if (r.approval.required) {
    out.push(`Deploying it needs approval. ${r.approval.reasons.map((reason) => reason.message).join(' ')}`);
  }
  return out;
}
