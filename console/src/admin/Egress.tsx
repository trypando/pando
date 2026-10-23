// The Egress page of an app's Settings: where this app may connect out to
// (R-182 – R-188).
//
// One list of addresses. What the host sets is shown with a lock and is
// read-only here; what this app adds can be removed; an addition that loosens
// the host's rules says what it needs (R-183). Rules are read from `GET
// /apps/{id}/egress`, which any holder of app.view may ask, so an owner who
// cannot read host policy still edits against rules they can see. Each change
// writes the spec's `egress` the ordinary way: a new revision, taking effect
// at the next deployment.
//
// Changing anything is offered on app.egress.tighten or app.egress.loosen
// (R-184). Which change needs which is the server's decision: an address
// typed to add is put to a dry-run save, which runs every check a save runs
// and writes nothing, and its answer is shown before Allow is pressed.
//
// The console no longer offers to exclude one of the host's addresses for one
// app. The API still does (R-182), and an exclusion made there is listed here
// and can be undone.

import { useEffect, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Card, Icon, Input, Tooltip } from '@design';

import { api } from '@api/client';
import { messageOf, refusal } from '../install/Accounts';
import { InlineWarning } from '../ui/InlineWarning';
import { LineSkeleton, Loading } from '../ui/Loading';
import {
  addActions,
  blocksPrivate,
  draftOf,
  egressLines,
  hostLine,
  installMode,
  looseningNote,
  ownLine,
  sameDraft,
  specOf,
  unrestricted,
  withEntry,
  withoutEntry,
} from './appEgress';
import type { AddAction, DryRunResult, EgressDraft, EgressLine, EgressResponse } from './appEgress';
import { useNewestSpec } from './newestSpec';
import { PageHead, Quiet, SwitchRow } from './settingsParts';
import { AppVerb, useCan } from './verbs';

const FROM_WORDS: Record<EgressLine['from'], string> = {
  host: 'Set by this host',
  added: 'Added for this app',
  own: 'Added for this app',
  excluded: 'Excluded for this app',
};

export function Egress({ appID }: Readonly<{ appID: string }>) {
  const queries = useQueryClient();
  // Either verb: tightening needs one, loosening the other, and holding
  // app.egress.loosen covers both (R-184). The server says which on save.
  const canTighten = useCan(AppVerb.EgressTighten);
  const canLoosen = useCan(AppVerb.EgressLoosen);
  const canEdit = canTighten || canLoosen;
  const [restricting, setRestricting] = useState(false);

  const egress = useQuery({
    queryKey: ['apps', appID, 'egress'],
    queryFn: () => api.get<EgressResponse>(`/apps/${appID}/egress`),
  });
  const newest = useNewestSpec(appID);
  const spec = newest.spec as (Record<string, unknown> & { egress?: EgressResponse['spec'] }) | undefined;

  const data = egress.data;
  const eff = data?.effective ?? null;
  // What the newest revision says, which is what an edit builds on and what
  // the next deployment runs; the merged rules are what runs now.
  const draft = draftOf(spec ? spec.egress : data?.spec);
  const unshipped = Boolean(spec && data && !newest.pinned && !sameDraft(draft, draftOf(data.spec)));

  // For a saved change not yet deployed, the server's reading of it: which of
  // its addresses loosen, by the same dry-run an addition is put to.
  const saved = useQuery({
    queryKey: ['apps', appID, 'egress-dry-run', specOf(draft)],
    queryFn: () => api.post<DryRunResult>(`/apps/${appID}/specs?dry_run=true`, { ...spec, egress: specOf(draft) }),
    enabled: unshipped && canEdit,
    retry: false,
  });
  const rules = unshipped && saved.data ? saved.data.egress : eff;

  const write = useMutation({
    mutationFn: (next: EgressDraft) => api.post(`/apps/${appID}/specs`, { ...spec, egress: specOf(next) }),
    onSuccess: () => {
      for (const part of ['specs', 'egress', 'notices']) void queries.invalidateQueries({ queryKey: ['apps', appID, part] });
    },
  });

  if (egress.isPending) {
    return (
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        <PageHead title="Egress" line="Where this app may connect out to" />
        <Card tone="paper" padding="md">
          <Loading>
            <LineSkeleton width="40ch" />
            <LineSkeleton width="30ch" />
          </Loading>
        </Card>
      </div>
    );
  }
  if (!data?.install) {
    return (
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        <PageHead title="Egress" line="Where this app may connect out to" />
        <Banner tone="failed">{messageOf(egress.error)}</Banner>
      </div>
    );
  }

  const mode = installMode(data.install.mode);
  const editable = canEdit && Boolean(spec);
  const open = unrestricted(data.install, draft) && !restricting;
  const lines = egressLines(data.install, draft, rules?.loosenings ?? []);
  const gate = rules?.gate ?? data.install.loosening;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      <PageHead title="Egress" line="Where this app may connect out to" />

      {open ? (
        <Card tone="paper" padding="md">
          <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: 'var(--space-3)' }}>
            <span style={{ display: 'flex', alignItems: 'flex-start', gap: 'var(--space-3)' }}>
              <Icon name="globe" size={16} />
              <span style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
                <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink)' }}>Unrestricted</span>
                <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>This app can connect anywhere.</span>
              </span>
            </span>
            {editable && (
              <Button variant="secondary" onClick={() => setRestricting(true)}>
                Restrict this app
              </Button>
            )}
          </div>
        </Card>
      ) : (
        <Card tone="paper" padding="none">
          <div style={{ display: 'flex', alignItems: 'flex-start', gap: 'var(--space-3)', padding: 'var(--space-3) var(--space-4)' }}>
            <Icon name={mode === 'allow_all' ? 'globe' : 'shield'} size={16} />
            <span style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
              <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink)' }}>{hostLine(mode)}</span>
              {ownLine(draft) && (
                <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{ownLine(draft)}</span>
              )}
            </span>
          </div>
          {lines.map((line) => (
            <div key={`${line.from}:${line.entry}`} className="pando-line">
              <span style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)', minWidth: 0 }}>
                <span style={{ font: 'var(--type-code)', color: 'var(--ink)', overflowWrap: 'anywhere' }}>{line.entry}</span>
                <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{FROM_WORDS[line.from]}</span>
              </span>
              <span style={{ font: 'var(--type-caption)', color: 'var(--contour-text)' }}>
                {line.loosens ? looseningNote(gate) : ''}
              </span>
              {line.from === 'host' ? (
                <Tooltip content="Set by this host. Only an administrator can change it.">
                  <span aria-label="Set by this host" style={{ display: 'inline-flex', padding: 'var(--space-2)' }}>
                    <Icon name="lock" size={16} />
                  </span>
                </Tooltip>
              ) : (
                <span>
                  {editable && (
                    <Button variant="ghost" disabled={write.isPending} onClick={() => write.mutate(withoutEntry(draft, line))}>
                      {line.from === 'excluded' ? 'Restore' : 'Remove'}
                    </Button>
                  )}
                </span>
              )}
            </div>
          ))}
          {editable && (
            <AddRow
              appID={appID}
              spec={spec!}
              actions={addActions(mode, draft)}
              gate={gate}
              draftFor={(entry, action) => withEntry(mode, draft, entry, action)}
              saving={write.isPending}
              onAdd={(next) => write.mutate(next)}
            />
          )}
        </Card>
      )}

      <Card tone="paper" padding="md">
        <PrivateSwitch
          install={data.install}
          draft={draft}
          disabled={!editable || write.isPending}
          onChange={(blockPrivate) => write.mutate({ ...draft, blockPrivate })}
        />
      </Card>

      {write.isError && <Banner tone="failed">{refusal(write.error)}</Banner>}
      {unshipped && <Quiet>A saved change takes effect at the next deployment.</Quiet>}
      {/* Never a blocker: something somebody wrote expecting an effect, that has none. */}
      {(rules?.unused ?? []).map((message) => (
        <InlineWarning key={message}>{message}</InlineWarning>
      ))}
      {/* R-187, wherever a restriction is in effect: somebody choosing a list
          would otherwise expect everything else to keep working. */}
      {rules?.restricted && (
        <Quiet>
          Only HTTP and HTTPS through Pando&rsquo;s egress gateway leave this app. A database protocol or raw TCP
          can&rsquo;t connect out, even to an allowed address.
        </Quiet>
      )}
    </div>
  );
}

/**
 * The address box and its buttons. What the address would do is asked of the
 * server as typing pauses — a refusal as it is written, or that it loosens
 * the host's rules and what that needs — before anything is saved.
 */
function AddRow({
  appID,
  spec,
  actions,
  gate,
  draftFor,
  saving,
  onAdd,
}: Readonly<{
  appID: string;
  spec: Record<string, unknown>;
  actions: AddAction[];
  gate: string | undefined;
  draftFor: (entry: string, action: AddAction) => EgressDraft;
  saving: boolean;
  onAdd: (next: EgressDraft) => void;
}>) {
  const [entry, setEntry] = useState('');
  const [settled, setSettled] = useState('');
  useEffect(() => {
    const t = setTimeout(() => setSettled(entry.trim()), 400);
    return () => clearTimeout(t);
  }, [entry]);
  // Asked of the first action: the one the host's rules call for, or, with
  // neither list, allowing — which narrows to that address and loosens nothing.
  const probe = settled ? specOf(draftFor(settled, actions[0]!)) : null;
  const decision = useQuery({
    queryKey: ['apps', appID, 'egress-dry-run', probe],
    queryFn: () => api.post<DryRunResult>(`/apps/${appID}/specs?dry_run=true`, { ...spec, egress: probe }),
    enabled: probe !== null,
    retry: false,
  });
  const current = settled === entry.trim() && settled !== '' ? decision : undefined;
  const loosens = (current?.data?.new_loosenings ?? []).length > 0;

  const add = (action: AddAction) => {
    onAdd(draftFor(entry.trim(), action));
    setEntry('');
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)', padding: 'var(--space-3) var(--space-4)', borderTop: 'var(--border-width) solid var(--rule)' }}>
      <div style={{ display: 'flex', alignItems: 'flex-start', flexWrap: 'wrap', gap: 'var(--space-2)' }}>
        <Input
          aria-label="Address"
          mono
          placeholder="api.example.com or 203.0.113.0/24"
          value={entry}
          onChange={(e) => setEntry(e.target.value)}
          style={{ flex: '1 1 18rem' }}
        />
        {actions.map((action) => (
          <Button
            key={action}
            variant="secondary"
            disabled={saving || entry.trim() === '' || Boolean(current?.isError)}
            onClick={() => add(action)}
          >
            {action === 'allow' ? 'Allow' : 'Block'}
          </Button>
        ))}
      </div>
      {current?.isError && <span style={{ font: 'var(--type-caption)', color: 'var(--marker-deep)' }}>{refusal(current.error)}</span>}
      {loosens && <span style={{ font: 'var(--type-caption)', color: 'var(--contour-text)' }}>{looseningNote(gate)}</span>}
    </div>
  );
}

/** Blocking private addresses: the host's, when it sets it, and otherwise the app's. */
function PrivateSwitch({
  install,
  draft,
  disabled,
  onChange,
}: Readonly<{
  install: EgressResponse['install'];
  draft: EgressDraft;
  disabled: boolean;
  onChange: (next: EgressDraft['blockPrivate']) => void;
}>) {
  const on = blocksPrivate(install, draft);
  const hostSets = Boolean(install.block_private) && draft.blockPrivate !== 'off';
  let line = on ? 'On' : 'Off';
  if (hostSets) line = 'On for every app on this host';
  else if (install.block_private) line = 'Off for this app. Loosens this host’s rules.';
  return (
    <SwitchRow
      label="Block private addresses"
      line={line}
      checked={on}
      disabled={disabled || hostSets}
      // Back to the host's when the host blocks them; the app's own otherwise.
      onChange={(checked) => onChange(checked ? (install.block_private ? 'inherit' : 'on') : 'inherit')}
    />
  );
}
