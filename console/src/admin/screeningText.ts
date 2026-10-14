// Wording for the screening section of detection review (design 10 §5).
//
// A refused amendment has no server-written summary — only an applied one does —
// so the console says what it proposed. Kept as a plain function beside the
// component so the wording is tested rather than eyeballed.

import type { Amendment, Outcome } from '@api/types.gen';

/** What an amendment proposed, in one plain sentence. */
export function describeAmendment(a: Amendment): string {
  const where = a.workload ? ` for ${a.workload}` : '';
  switch (a.kind) {
    case 'set_command':
      return `Start${where} with ${(a.command ?? []).join(' ') || a.value || 'a new command'}`;
    case 'set_env':
      return `Set ${a.key} to ${a.value ?? ''}${where}`;
    case 'set_port':
      return `Serve${where} on port ${a.port}`;
    case 'set_health':
      return `Check health at ${a.path}${a.port ? ` on port ${a.port}` : ''}`;
    case 'add_slot':
      return `Add ${a.key} as a ${a.slot_type ?? 'unknown'} dependency`;
    case 'set_build_context':
      return `Build from the ${a.path} directory`;
    case 'set_dockerfile':
      return `Build with ${a.path}`;
    case 'set_static_dir':
      return `Serve the files in ${a.path}`;
    case 'add_volume':
      return `Keep ${a.path} in storage${where}`;
    case 'answer_question':
      return `Answer ${a.key} with ${a.value ?? ''}`;
    case 'add_warning':
      return 'Add a note to this plan';
    default:
      return `A change Pando does not recognize (${a.kind})`;
  }
}

/**
 * Whether the section is worth showing at all.
 *
 * An install with no AI adapter is not a degraded install (R-335), and a
 * detection that neither failed nor asked anything does not call one (R-336),
 * so a screening that never ran for either reason says nothing. Any other
 * reason it did not run — host policy, a provider that failed — is shown,
 * because the person who configured the adapter will want to know it did not
 * do its job.
 */
export function screeningVisible(outcome: Outcome | undefined): outcome is Outcome {
  if (!outcome) return false;
  if (outcome.ran) return true;
  return !!outcome.skip_code && !QUIET.has(outcome.skip_code);
}

const QUIET = new Set(['not_configured', 'not_needed', 'blocked']);

export function filesRead(count: number): string {
  return `Files read (${count})`;
}
