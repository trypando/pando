// The security score in the app's header (R-310, R-320): the shield and the
// number, colored against the installation's minimum, taking somebody to the
// Security section. Read from the same report the section reads, so the two
// cost one request.
//
// Absent while there is nothing to say: no score yet, and no scanner that
// would ever take one.

import { useQuery } from '@tanstack/react-query';

import { api } from '@api/client';
import type { Report } from '@api/types.gen';
import { ScoreBadge, type Verdict } from '../ui/ScoreBadge';

export function HeaderScore({ appID, onOpen }: Readonly<{ appID: string; onOpen: () => void }>) {
  const report = useQuery({
    queryKey: ['apps', appID, 'security'],
    queryFn: () => api.get<Report>(`/apps/${appID}/security`),
  });
  const r = report.data;
  if (!r) return null;
  const score = r.awaiting_image ? undefined : r.standing.score;
  if ((score === undefined || score === null) && !r.scanner) return null;

  const below = r.standing.verdict === 'insecure';
  let label = 'Not scanned yet. See security.';
  if (score !== undefined && score !== null) {
    label = below ? `Security score ${score}, below this installation's requirement of ${r.standing.threshold}` : `Security score ${score}`;
  }

  return (
    <>
      <span aria-hidden="true" style={{ width: 'var(--border-width)', height: 'var(--space-4)', background: 'var(--rule-strong)' }} />
      <button
        type="button"
        onClick={onOpen}
        aria-label={label}
        style={{ padding: 0, border: 0, background: 'none', cursor: 'pointer', display: 'inline-flex' }}
      >
        <ScoreBadge score={score} verdict={r.standing.verdict as Verdict} threshold={r.standing.threshold} size={20} />
      </button>
    </>
  );
}
