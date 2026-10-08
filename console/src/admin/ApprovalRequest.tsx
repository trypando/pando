// One deploy waiting for approval, and the way to answer it (R-154 – R-156).
//
// Shown on the app — its overview and its list of deploys — and on the
// Approvals screen, which lists every waiting request somebody may see. The
// same component in both places, so a request reads the same wherever it is
// found and Approve does the same thing.
//
// What a person deciding needs is on it: why the deploy needs approval, in the
// server's words (R-105), how many approvals it has and needs, when it stops
// waiting, and what anybody has said so far. Approve and Reject are offered
// only to somebody the server says may decide (`can_decide`). The endpoint
// checks the verb either way; this spares a person a button that is refused.

import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { invalidateApp } from './appList';
import { Banner, Button, Input, StatusIndicator, Tooltip } from '@design';

import { api } from '@api/client';
import { refusal } from '../install/Accounts';
import { relative } from '../ui/time';
import { decider, expiry, progress } from './approval';
import type { ApprovalDeployment } from './approval';

export function ApprovalRequest({
  deployment,
  heading,
}: {
  deployment: ApprovalDeployment;
  /** What the request is for, above it — the app's name on the Approvals
   *  screen. Absent on the app's own screens, which already say. */
  heading?: React.ReactNode;
}) {
  const queries = useQueryClient();
  const [comment, setComment] = useState('');
  const [answered, setAnswered] = useState<string | null>(null);
  const d = deployment;
  const path = `/apps/${d.app_id}/deployments/${d.id}`;

  const decide = useMutation({
    mutationFn: (verb: 'approve' | 'reject') =>
      api.post<ApprovalDeployment>(`${path}/${verb}`, comment.trim() ? { comment: comment.trim() } : {}),
    onSuccess: (after, verb) => {
      setComment('');
      setAnswered(
        verb === 'reject'
          ? 'Rejected. The deploy won’t run.'
          : after?.status === 'awaiting_approval'
            ? `Approved. ${progress(after)}`
            : 'Approved. The deploy has started.',
      );
      // The app moves to deploying once enough people approve, so its record
      // and the list both change, as well as this request.
      void invalidateApp(queries, d.app_id);
      void queries.invalidateQueries({ queryKey: ['approvals'] });
    },
  });

  const decisions = d.approvals ?? [];
  const reasons = d.approval_reasons ?? [];

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      {heading}
      <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: 'var(--space-3)' }}>
        <StatusIndicator status="info" label="Waiting for approval" />
        <span style={CAPTION}>
          Asked {relative(d.started_at).toLowerCase()} by{' '}
          {d.requested_by_name ? (
            d.requested_by_name
          ) : (
            <span style={{ font: 'var(--type-code-sm)' }}>{d.created_by}</span>
          )}
        </span>
      </div>

      {reasons.length > 0 && (
        <div>
          <p style={{ ...BODY, margin: '0 0 var(--space-1)' }}>This deploy needs approval because:</p>
          <ul style={{ ...BODY, margin: 0, paddingLeft: 'var(--space-5)' }}>
            {reasons.map((r) => (
              <li key={r.reason + r.message}>{r.message}</li>
            ))}
          </ul>
        </div>
      )}

      <p style={{ ...BODY, margin: 0 }}>
        {progress(d)}{' '}
        {d.approval_expires_at ? (
          <Tooltip content={d.approval_expires_at}>
            <span>{expiry(d.approval_expires_at)}</span>
          </Tooltip>
        ) : (
          expiry(undefined)
        )}
      </p>

      {decisions.length > 0 && (
        <ul style={{ ...BODY, margin: 0, paddingLeft: 'var(--space-5)' }}>
          {decisions.map((a) => (
            <li key={a.principal_id}>
              {decider(a)} {a.decision === 'approve' ? 'approved' : 'rejected'} {relative(a.decided_at).toLowerCase()}
              {a.comment ? `: “${a.comment}”` : '.'}
            </li>
          ))}
        </ul>
      )}

      {answered && <Banner tone="running">{answered}</Banner>}
      {decide.isError && <Banner tone="failed">{refusal(decide.error)}</Banner>}

      {d.can_decide && !answered ? (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)', maxWidth: '52ch' }}>
          <Input
            label="Comment, optional"
            value={comment}
            helper="Recorded with your answer in the audit log, and shown to whoever asked."
            onChange={(e) => setComment(e.target.value)}
          />
          <div style={{ display: 'flex', gap: 'var(--space-3)' }}>
            <Button variant="secondary" disabled={decide.isPending} onClick={() => decide.mutate('approve')}>
              Approve
            </Button>
            <Button variant="ghost" disabled={decide.isPending} onClick={() => decide.mutate('reject')}>
              Reject
            </Button>
          </div>
        </div>
      ) : (
        !d.can_decide && (
          <p style={{ ...CAPTION, margin: 0 }}>
            You can’t answer this request. Somebody holding app.deploy.approve on this app, or
            install.deploys.approve, can.
          </p>
        )
      )}
    </div>
  );
}

const BODY: React.CSSProperties = { font: 'var(--type-body-ui)', color: 'var(--ink)' };
const CAPTION: React.CSSProperties = { font: 'var(--type-caption)', color: 'var(--ink-secondary)' };
