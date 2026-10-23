// What the app asks of the person reading it (R-412).
//
// One list, computed by the server (GET /apps/{id}/notices), in place of the
// banners and inline warnings that used to sit above and below everything
// else. Each row is one sentence, the action that fixes it, and Ignore — which
// hides it from this person until what it says changes. Drawn only while at
// least one is not ignored.
//
// The failed and deploying banners and the approval request are not here: they
// are the app's state, not something to do.

import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Badge, Banner, Button, Icon } from '@design';

import { api } from '@api/client';
import type { Notice } from '@api/types.gen';
import { messageOf } from '../install/Accounts';
import { useOpenPanel } from './AppPanel';
import { NOT_DEPLOYED } from './PanelDeployments';
import { AppVerb, useCan } from './verbs';

export function noticesKey(appID: string) {
  return ['apps', appID, 'notices'];
}

/**
 * Where each spec warning is fixed. A warning a person cannot act on from
 * where they read it is how people learn to dismiss warnings, so every code
 * the console has a screen for carries the way to it.
 */
export const FIXES: Record<string, { label: string; tab: string; focus?: string }> = {
  WARN_NO_PERSISTENT_VOLUME: { label: 'Add storage', tab: 'settings', focus: 'storage' },
  WARN_COMPOSE_CONSTRUCT_REWRITTEN: { label: 'Review the configuration', tab: 'settings', focus: 'configuration' },
  WARN_UNDECLARED_DEPENDENCY_SUSPECTED: { label: 'Open dependencies', tab: 'settings', focus: 'dependencies' },
};

interface Action {
  label: string;
  run: () => void;
}

export function Attention({
  appID,
  onGo,
  onFindings,
}: Readonly<{
  appID: string;
  /** A tab, and the section on it. */
  onGo: (tab: string, focus?: string) => void;
  /** Brings the Security section into view. */
  onFindings: () => void;
}>) {
  const queries = useQueryClient();
  const canEdit = useCan(AppVerb.SpecEdit);
  const openDeployments = useOpenPanel('deployments');

  const notices = useQuery({
    queryKey: noticesKey(appID),
    queryFn: () => api.get<{ notices: Notice[] | null }>(`/apps/${appID}/notices`),
  });

  const ignore = useMutation({
    mutationFn: (key: string) => api.put<Notice>(`/apps/${appID}/notices/${encodeURIComponent(key)}/ignore`),
    // The row goes as soon as the server has it. The answer is the notice as
    // it now reads, ignored.
    onSuccess: (_, key) =>
      queries.setQueryData<{ notices: Notice[] | null }>(noticesKey(appID), (old) =>
        old ? { notices: (old.notices ?? []).map((n) => (n.key === key ? { ...n, ignored: true } : n)) } : old,
      ),
  });

  const open = (notices.data?.notices ?? []).filter((n) => !n.ignored);
  if (open.length === 0) return null;

  // What fixes each notice, offered only to somebody who can follow it through.
  const actionFor = (key: string): Action | undefined => {
    if (key === 'config.unshipped') {
      return openDeployments && { label: 'Review changes', run: () => openDeployments({ view: 'deployments', deployment: NOT_DEPLOYED }) };
    }
    if (key === 'security.below_threshold') return { label: 'See findings', run: onFindings };
    if (!canEdit) return undefined;
    if (key === 'variables.unset') return { label: 'Fill them in', run: () => onGo('settings', 'variables') };
    if (key === 'backup.not_taken') return { label: 'Open storage', run: () => onGo('settings', 'storage') };
    const fix = key.startsWith('warning.') ? FIXES[key.slice('warning.'.length)] : undefined;
    return fix && { label: fix.label, run: () => onGo(fix.tab, fix.focus) };
  };

  return (
    <section aria-labelledby="attention" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      <h4 id="attention" style={{ font: 'var(--type-h4)', margin: 0, display: 'flex', alignItems: 'center', gap: 'var(--space-2)' }}>
        Needs your attention <Badge count={open.length} />
      </h4>
      <ul
        style={{
          listStyle: 'none',
          margin: 0,
          padding: 0,
          background: 'var(--paper-raised)',
          border: 'var(--border-width) solid var(--rule)',
          borderRadius: 'var(--radius-md)',
        }}
      >
        {open.map((n, i) => {
          const action = actionFor(n.key);
          return (
            <li
              key={n.key}
              style={{
                display: 'flex',
                alignItems: 'center',
                flexWrap: 'wrap',
                gap: 'var(--space-3)',
                padding: 'var(--space-3) var(--space-4)',
                borderTop: i === 0 ? undefined : 'var(--border-width) solid var(--rule)',
              }}
            >
              {n.key === 'config.unshipped' ? (
                <Icon name="info" color="var(--water)" />
              ) : (
                <Icon name="triangle-alert" color="var(--contour-text)" />
              )}
              <span style={{ flex: '1 1 24rem', font: 'var(--type-body-ui)', color: 'var(--ink)' }}>{n.message}</span>
              {action && (
                <Button variant="secondary" onClick={action.run}>
                  {action.label}
                </Button>
              )}
              <Button
                variant="ghost"
                icon={<Icon name="eye-off" />}
                title="Hide this until it changes"
                disabled={ignore.isPending && ignore.variables === n.key}
                onClick={() => ignore.mutate(n.key)}
              >
                Ignore
              </Button>
            </li>
          );
        })}
      </ul>
      {ignore.isError && <Banner tone="failed">{messageOf(ignore.error)}</Banner>}
    </section>
  );
}
