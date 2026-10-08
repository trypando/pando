// The security score (R-310 – R-320, design 09 §6).
//
// On the app's overview, with its status, address and deploy log: the score is a
// property of what is running, and the person who has to act on it is the one
// reading this page rather than somebody three tabs away in settings.
//
// A badge carrying the number, never a grade or a color standing alone (R-320).
// "F" tells a deployer nothing they can act on and a red pill tells somebody who
// cannot see red nothing at all — so the color is the second signal and the
// number is the first.
//
// The findings are the app's, so anyone who can view the app can read them. Only
// somebody who can deploy can ask for a new scan, because the score decides
// whether the next deploy is allowed.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, StatusIndicator, Tag } from '@design';

import { api } from '@api/client';
import type { Finding, Report } from '@api/types.gen';
import { Quiet, messageOf } from '../install/Accounts';
import { MEASURE } from '../ui/layout';
import { relative } from '../ui/time';
import { ScoreBadge } from '../ui/ScoreBadge';
import { Table } from '../ui/Table';
import { LineSkeleton, Loading } from '../ui/Loading';
import { AppVerb, useCan } from './verbs';
import { securityInterval, useOnScreen } from '../ui/polling';

export function Security({
  appID,
  everything = false,
  watching = false,
}: {
  appID: string;
  /**
   * Whether something that may start a scan is under way — a deploy of a new
   * commit scans what it built — so the panel keeps asking until it has seen
   * the scan start and end, rather than waiting for a reload.
   */
  watching?: boolean;
  /**
   * Every finding, with no toggle: for a dialog opened to read them, where a
   * scrolling list is the point rather than a hazard to the Deploy button.
   */
  everything?: boolean;
}) {
  const queries = useQueryClient();
  // POST /security/scan is app.deploy, not app.view (see above).
  const canScan = useCan(AppVerb.Deploy);

  const scan = useMutation({
    mutationFn: () => api.post<Report>(`/apps/${appID}/security/scan`),
    onSuccess: () => queries.invalidateQueries({ queryKey: ['apps', appID, 'security'] }),
  });

  // Polled while a scan runs, whoever started it: the report says so
  // (scanning_since), so a scan from a deploy, detection, the CLI or MCP shows
  // here as well as one started by the button below (R-261).
  //
  // Every five seconds while a scan runs and every ten while a deploy that may
  // start one runs, and only while the section is on screen (issue #72): a
  // scan takes minutes, and the app screen reads the report again when the
  // deploy finishes.
  const [section, visible] = useOnScreen<HTMLElement>();
  const report = useQuery({
    queryKey: ['apps', appID, 'security'],
    queryFn: () => api.get<Report>(`/apps/${appID}/security`),
    refetchInterval: (query) =>
      securityInterval(Boolean(query.state.data?.scanning_since) || scan.isPending, watching, visible),
  });

  const scanning = Boolean(report.data?.scanning_since) || scan.isPending;

  const [all, setAll] = useState(false);

  const standing = report.data?.standing;
  const counts = report.data?.counts;
  const findings = report.data?.scan?.findings ?? [];

  // The five that cost the most, ranked by the server. A base image can carry
  // two hundred findings, and a list of two hundred on the page somebody
  // deploys from is a list nobody reads and a Deploy button nobody can find.
  // The rest are one click away, in a box with a bottom to it.
  const worst = report.data?.worst ?? findings.slice(0, 5);
  const shown = all || everything ? findings : worst;
  const hidden = everything ? 0 : findings.length - worst.length;

  return (
    <section ref={section} style={{ maxWidth: everything ? undefined : MEASURE }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-4)' }}>
        {!everything && <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>Security</h4>}
        {canScan && (
          <Button onClick={() => scan.mutate()} disabled={scanning}>
            {scanning ? 'Scanning' : 'Scan now'}
          </Button>
        )}
      </div>

      {scanning && (
        <div style={{ marginTop: 'var(--space-3)' }}>
          {/* The hollow ring, not a spinner: the design system's building
              status, which stays still so the page stays calm (readme,
              Motion). The last score stays below until the new one is in. */}
          <StatusIndicator
            status="building"
            label={
              report.data?.scanning_since
                ? `Scanning for known vulnerabilities · started ${relative(report.data.scanning_since).toLowerCase()}`
                : 'Scanning for known vulnerabilities'
            }
          />
        </div>
      )}

      {report.isError && <Banner tone="failed">{messageOf(report.error)}</Banner>}
      {scan.isError && <Banner tone="failed">{messageOf(scan.error)}</Banner>}

      <div style={{ marginTop: 'var(--space-4)' }}>
        {/* The verdict's two lines — badge and standing, then the counts —
            while the report loads. */}
        {report.isPending && (
          <Loading gap="var(--space-2)">
            <LineSkeleton width="36ch" />
            <LineSkeleton width="28ch" />
          </Loading>
        )}
        {standing && <Verdict report={report.data as Report} scanning={scanning} />}
      </div>

      {report.data?.scan?.error && (
        <div style={{ marginTop: 'var(--space-3)' }}>
          {/* R-318: a scanner that could not run is a thing to fix, and is not
              the same as an app that is clean. */}
          <Banner tone="failed">{report.data.scan.error}</Banner>
        </div>
      )}

      {report.data?.ignoring_unfixable && (
        <div style={{ marginTop: 'var(--space-2)' }}>
          {/* Said, not implied. A list somebody cannot explain the length of is
              a list they stop trusting. */}
          <Quiet>
            This installation leaves out findings with no fix available, from the score and from
            this list.
          </Quiet>
        </div>
      )}

      {counts && findings.length > 0 && (
        <div style={{ marginTop: 'var(--space-5)' }}>
          <div
            style={
              all
                ? {
                    // Bounded and scrolling, for the same reason the log is:
                    // a page that grows with its content pushes everything
                    // below it out of reach.
                    maxHeight: '50vh',
                    overflowY: 'auto',
                    overscrollBehavior: 'contain',
                  }
                : undefined
            }
          >
          <Table
            columns={[
              { key: 'id', header: 'Finding', width: 'minmax(0,22ch)', mono: true },
              {
                key: 'severity',
                header: 'Severity',
                width: '14ch',
                render: (row: Finding) => (
                  <StatusIndicator status={symbolFor(row.severity)} label={sentence(row.severity)} />
                ),
              },
              {
                key: 'title',
                header: 'What it is',
                // The one column that takes what is left, because it is the
                // one carrying a sentence.
                width: 'minmax(0,1fr)',
                render: (row: Finding) => (
                  <span
                    style={{
                      whiteSpace: 'normal',
                      padding: 'var(--space-2) 0',
                      // Three lines at most. A CVE description runs to a
                      // paragraph, and a table where one row is eight lines
                      // tall is a table nobody can scan. The whole of it is in
                      // the tooltip.
                      display: '-webkit-box',
                      WebkitBoxOrient: 'vertical',
                      WebkitLineClamp: 3,
                      overflow: 'hidden',
                    }}
                    title={row.title}
                  >
                    {row.title}
                  </span>
                ),
              },
              {
                key: 'fix',
                header: 'Fixed in',
                width: '22ch',
                muted: true,
                render: (row: Finding) => (
                  // A fix is often a list of branches — "1.24.13, 1.25.7,
                  // 1.26.0" — and the whole list in a tag is a tag as wide as
                  // the table. The first one is the answer to "what do I move
                  // to"; the rest are in the tooltip.
                  row.fix ? (
                    <span title={row.fix}>
                      <Tag mono>{(row.fix.split(',')[0] ?? row.fix).trim()}</Tag>
                      {row.fix.includes(',') ? ' …' : ''}
                    </span>
                  ) : (
                    'No fix yet'
                  )
                ),
              },
            ]}
            rows={shown}
          />
          </div>

          {hidden > 0 && (
            <div style={{ marginTop: 'var(--space-3)' }}>
              <Button variant="ghost" onClick={() => setAll(!all)}>
                {all ? 'Show the five worst' : `Show all ${findings.length} findings`}
              </Button>
            </div>
          )}
        </div>
      )}
    </section>
  );
}

function Verdict({ report, scanning }: { report: Report; scanning: boolean }) {
  const { standing, counts, scanner } = report;
  const score = standing.score;

  if (standing.verdict === 'inert') {
    return (
      <Quiet>
        This installation requires a security score of {standing.threshold}, and has no scanner
        configured — so nothing is scored and nothing is enforced.
      </Quiet>
    );
  }

  if (score === null || score === undefined) {
    // The first scan is under way, and the line above says so.
    if (scanning) return null;
    return (
      <Quiet>
        {scanner
          ? 'This app has not been scanned yet.'
          : 'This installation does not scan apps, so there is no score.'}
      </Quiet>
    );
  }

  const taken = report.scan?.ran_at ? relative(report.scan.ran_at) : '';

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)' }}>
        <ScoreBadge score={score} verdict={standing.verdict as never} threshold={standing.threshold} full />
        <StatusIndicator
          status={standing.verdict === 'insecure' ? 'failed' : 'running'}
          label={
            standing.verdict === 'insecure'
              ? `Below this installation's requirement of ${standing.threshold}`
              : standing.threshold > 0
                ? `Meets this installation's requirement of ${standing.threshold}`
                : 'No requirement is set'
          }
        />
      </div>

      <Quiet>
        {counts.critical} critical, {counts.high} high, {counts.medium} medium,{' '}
        {counts.low + counts.unknown} low
        {taken ? ` · scanned ${taken.toLowerCase()}` : ''}
        {/* A scan with no revision is the one detection took of the source,
            before there was a configuration to attach it to. Saying so is the
            difference between a number somebody trusts and one they wonder
            about. */}
        {report.scan && !report.scan.spec_id ? ' · from the source, before this configuration' : ''}
      </Quiet>

      {standing.verdict === 'insecure' && standing.stop_at && (
        // R-316: the deadline, at the moment it starts, to the person who has
        // to act on it.
        <Banner tone="failed">
          Pando will stop this app on {new Date(standing.stop_at).toLocaleString()} unless its score
          reaches {standing.threshold}.
        </Banner>
      )}
    </div>
  );
}

/** Severity onto the design system's symbols. Never color alone. */
function symbolFor(severity: string): 'failed' | 'building' | 'stopped' | 'info' {
  switch (severity) {
    case 'critical':
    case 'high':
      return 'failed';
    case 'medium':
      return 'building';
    case 'low':
      return 'stopped';
    default:
      return 'info';
  }
}

function sentence(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}
