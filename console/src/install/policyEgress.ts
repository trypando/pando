// The installation's egress rules and deploy approval, as the Policy screen
// reads and writes them (R-181 – R-185, R-154 – R-156).
//
// Kept apart from the screen so the one non-obvious reading — the field from
// before issue #79 — is asserted in a test rather than remembered.

export type InstallEgressMode = 'allow_all' | 'denylist' | 'allowlist';
export type Loosening = 'verb' | 'approval' | 'forbidden';

/** The policy fields this file reads. */
export interface EgressPolicyFields {
  egress_mode?: string;
  egress_list?: string[] | null;
  egress_allowlist?: string[] | null;
  egress_block_private?: boolean;
  egress_loosening?: string;
}

/**
 * The install's mode and list in force.
 *
 * The same reading as the server's EgressInstallMode: with no mode set, a
 * non-empty egress_allowlist — the setting from before there was a mode — is
 * an allowlist, so a policy written then keeps meaning what its author wrote.
 */
export function installEgress(doc: EgressPolicyFields): { mode: InstallEgressMode; list: string[] } {
  switch (doc.egress_mode) {
    case 'denylist':
    case 'allowlist':
      return { mode: doc.egress_mode, list: doc.egress_list ?? [] };
    case undefined:
    case '':
      if ((doc.egress_allowlist ?? []).length > 0) {
        return { mode: 'allowlist', list: doc.egress_allowlist ?? [] };
      }
  }
  return { mode: 'allow_all', list: [] };
}

/**
 * The policy patch for a change to the install's mode or list.
 *
 * Always written in today's fields, and the old one cleared: leaving both set
 * would make the old one a second answer to the same question that a reader of
 * the file has to know is ignored.
 */
export function egressPatch(
  mode: InstallEgressMode,
  list: string[],
): { egress_mode: InstallEgressMode; egress_list: string[]; egress_allowlist: undefined } {
  return { egress_mode: mode, egress_list: list, egress_allowlist: undefined };
}

/** The loosening rule in force, with the server's default applied. */
export function looseningRule(doc: EgressPolicyFields): Loosening {
  return doc.egress_loosening === 'approval' || doc.egress_loosening === 'forbidden' ? doc.egress_loosening : 'verb';
}

/** One entry per line, blank lines and surrounding spaces dropped. */
export function lines(text: string): string[] {
  return text
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean);
}

export const EGRESS_MODES: ReadonlyArray<readonly [InstallEgressMode, string, string]> = [
  ['allow_all', 'Anywhere', 'Any destination. The default.'],
  ['denylist', 'Anywhere except the list', 'Every destination except those listed below.'],
  ['allowlist', 'Only the list', 'Only the destinations listed below.'],
];

export const LOOSENING: ReadonlyArray<readonly [Loosening, string, string]> = [
  ['verb', 'Allowed for people with permission', 'Anyone holding app.egress.loosen on the app. The owner role holds it.'],
  ['approval', 'Need a deploy approval', 'Anyone who can change an app’s egress can propose a loosening. The deploy waits for approval.'],
  ['forbidden', 'Not allowed', 'A deploy that would loosen these rules is refused.'],
];

/** The helper under the install's list: every form an entry can take (R-185). */
export const ENTRY_FORMS =
  'One destination per line: a hostname (api.example.com), a wildcard of its subdomains (*.example.com), an IP address, or a CIDR range (10.0.0.0/8). Any of them may end in a port (api.example.com:443, [2001:db8::1]:443). * on its own means everywhere.';

/** How many approvals a deploy needs, with zero read as the server reads it. */
export function approvalsNeeded(count: number | undefined): number {
  return count && count > 0 ? count : 1;
}
