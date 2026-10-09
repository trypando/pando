// Where this app may connect out to (R-182 – R-188).
//
// Two halves. The rules the app runs with, merged — the installation's mode
// and list with this app's additions and removals, the app's own list on top,
// private-address blocking, and every way the app loosens the installation's
// rules with what that needs — read from `GET /apps/{id}/egress`, which any
// holder of app.view may ask, so an owner who cannot read host policy still
// edits against rules they can see. And an editor, which writes the spec's
// `egress` the ordinary way: a new revision, taking effect at the next deploy.
//
// The editor is offered on app.egress.tighten or app.egress.loosen (R-184).
// Which change needs which is the server's decision. The editor asks for it
// as the draft changes, with a dry-run save that runs every check a save runs
// and writes nothing, and shows the answer — a refusal as it is written, or
// what the draft loosens and whether its deploy would need approval.

import { useEffect, useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Dialog, Radio, StatusIndicator } from '@design';

import { api } from '@api/client';
import { Quiet, messageOf, refusal } from '../install/Accounts';
import { ListField } from '../ui/ListField';
import { ENTRY_FORMS } from '../install/policyEgress';
import { InlineWarning } from '../ui/InlineWarning';
import { Table } from '../ui/Table';
import { MEASURE } from '../ui/layout';
import { LineSkeleton, Loading } from '../ui/Loading';
import {
  appModeWords,
  draftOf,
  dryRunWords,
  fromWords,
  gateWords,
  installMode,
  listEdits,
  modeWords,
  sameDraft,
  specOf,
} from './appEgress';
import type { DryRunResult, EgressDraft, EgressResponse } from './appEgress';
import { useNewestSpec } from './newestSpec';
import { AppVerb, useCan } from './verbs';

export function Egress({ appID, focus }: { appID: string; focus?: boolean }) {
  const heading = useRef<HTMLElement>(null);
  useEffect(() => {
    if (focus) heading.current?.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }, [focus]);

  // Either verb: tightening needs one, loosening the other, and holding
  // app.egress.loosen covers both (R-184). The server says which on save.
  const canTighten = useCan(AppVerb.EgressTighten);
  const canLoosen = useCan(AppVerb.EgressLoosen);
  const canEdit = canTighten || canLoosen;
  const [editing, setEditing] = useState(false);

  const egress = useQuery({
    queryKey: ['apps', appID, 'egress'],
    queryFn: () => api.get<EgressResponse>(`/apps/${appID}/egress`),
  });
  const newest = useNewestSpec(appID);

  const eff = egress.data?.effective ?? null;
  // A saved change not yet deployed: the rules below are what runs, and the
  // change waits for the deploy, as every rule here does (O-10).
  const unshipped =
    newest.spec && egress.data && !newest.pinned
      ? !sameDraft(draftOf(newest.spec.egress), draftOf(egress.data.spec))
      : false;

  return (
    <section ref={heading} style={{ maxWidth: MEASURE }}>
      <div style={{ display: 'flex', alignItems: 'baseline', justifyContent: 'space-between' }}>
        <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>Where this app can connect out to</h4>
        {canEdit && newest.spec && egress.data && (
          <Button variant="secondary" onClick={() => setEditing(true)}>
            Change
          </Button>
        )}
      </div>
      <Quiet>
        The installation’s rules, with this app’s changes to them. Changes take effect at the next
        deploy.
      </Quiet>

      {egress.isPending && (
        <Loading>
          <LineSkeleton width="40ch" />
          <LineSkeleton width="30ch" />
        </Loading>
      )}
      {egress.isError && <Banner tone="failed">{messageOf(egress.error)}</Banner>}

      {egress.isSuccess && !eff && (
        <Quiet>This app has no configuration yet, so it has no rules to show.</Quiet>
      )}

      {eff && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)', marginTop: 'var(--space-4)' }}>
          {unshipped && (
            <Banner tone="info">A saved change to these rules takes effect at the next deploy.</Banner>
          )}

          <dl style={{ margin: 0, display: 'grid', gridTemplateColumns: 'max-content 1fr', gap: 'var(--space-2) var(--space-5)', font: 'var(--type-body-ui)' }}>
            <dt style={{ color: 'var(--ink-secondary)' }}>Destinations</dt>
            <dd style={{ margin: 0 }}>{modeWords(eff.mode)}</dd>
            <dt style={{ color: 'var(--ink-secondary)' }}>Private addresses</dt>
            <dd style={{ margin: 0 }}>
              {eff.block_private ? 'Blocked' : 'Not blocked'}
              {eff.block_private_from ? ` · set by ${eff.block_private_from === 'app' ? 'this app' : 'the installation'}` : ''}
            </dd>
          </dl>

          {installMode(eff.mode) !== 'allow_all' && (
            <Table
              dense
              columns={[
                { key: 'entry', header: eff.mode === 'allowlist' ? 'May reach' : 'May not reach', width: 'minmax(0,40ch)', mono: true },
                { key: 'from', header: 'From', width: '16ch', muted: true, render: (row: { from: string }) => fromWords(row.from) },
              ]}
              rows={(eff.list ?? []).map((e) => ({ id: `${e.from}:${e.entry}`, ...e }))}
              empty={
                <Quiet>
                  {eff.mode === 'allowlist'
                    ? 'The list is empty, so this app can’t connect anywhere.'
                    : 'The list is empty, so nothing is refused by it.'}
                </Quiet>
              }
            />
          )}

          {appModeWords(eff.app_mode) && (
            <div>
              <p style={{ font: 'var(--type-body-ui)', margin: '0 0 var(--space-2)' }}>
                {appModeWords(eff.app_mode)}, from this app’s own list. A destination has to pass both.
              </p>
              <Table
                dense
                columns={[{ key: 'entry', header: 'This app’s own list', width: 'minmax(0,40ch)', mono: true }]}
                rows={(eff.app_list ?? []).map((entry) => ({ id: entry, entry }))}
                empty={
                  <Quiet>
                    {eff.app_mode === 'allowlist'
                      ? 'The list is empty, so this app can’t connect anywhere.'
                      : 'The list is empty, so it refuses nothing.'}
                  </Quiet>
                }
              />
            </div>
          )}

          {(eff.loosenings ?? []).length > 0 && (
            <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
              <p style={{ font: 'var(--type-body-ui)', margin: 0 }}>
                This app loosens the installation’s rules. {gateWords(eff.gate).detail}
              </p>
              {(eff.loosenings ?? []).map((l) => (
                <div key={l.kind + (l.entry ?? '')} style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-3)', alignItems: 'baseline' }}>
                  <StatusIndicator status={gateWords(eff.gate).status} label={gateWords(eff.gate).label} />
                  <span style={{ font: 'var(--type-body-ui)' }}>{l.message}</span>
                </div>
              ))}
            </div>
          )}

          {/* Never a blocker: something somebody wrote expecting an effect,
              that has none. */}
          {(eff.unused ?? []).map((message) => (
            <InlineWarning key={message}>{message}</InlineWarning>
          ))}

          {/* R-187, in body text wherever a restriction is in effect: an owner
              choosing a denylist would otherwise expect everything else to
              keep working. */}
          {eff.restricted ? (
            <Banner tone="info">
              Only HTTP and HTTPS sent through Pando’s egress gateway leave this app. Pando gives it
              HTTP_PROXY and HTTPS_PROXY naming the gateway. Anything else — a database protocol, raw
              TCP, or a client that ignores those variables — can’t connect out, even to a destination
              these rules allow.
            </Banner>
          ) : (
            <Quiet>Nothing is restricted, so Pando puts nothing in this app’s path.</Quiet>
          )}
        </div>
      )}

      {editing && canEdit && newest.spec && egress.data && (
        <EditEgress
          appID={appID}
          data={egress.data}
          spec={newest.spec as unknown as Record<string, unknown> & { egress?: EgressResponse['spec'] }}
          onClose={() => setEditing(false)}
        />
      )}
    </section>
  );
}

function EditEgress({
  appID,
  data,
  spec,
  onClose,
}: {
  appID: string;
  data: EgressResponse;
  spec: Record<string, unknown> & { egress?: EgressResponse['spec'] };
  onClose: () => void;
}) {
  const queries = useQueryClient();
  const start = draftOf(spec.egress);
  const [draft, setDraft] = useState<EgressDraft>(start);
  const set = (patch: Partial<EgressDraft>) => setDraft((d) => ({ ...d, ...patch }));

  const mode = installMode(data.install.mode);
  const installList = data.install.list ?? [];
  const edits = listEdits(mode);
  const gate = gateWords(data.install.loosening);
  const loosens = `This loosens the installation’s rules. ${gate.label}: ${gate.detail}`;

  // The server's decision on the draft, asked once typing pauses.
  const [settled, setSettled] = useState<EgressDraft>(start);
  useEffect(() => {
    const t = setTimeout(() => setSettled(draft), 400);
    return () => clearTimeout(t);
  }, [draft]);
  const changed = !sameDraft(settled, start);
  const decision = useQuery({
    queryKey: ['apps', appID, 'egress-dry-run', specOf(settled)],
    queryFn: () => api.post<DryRunResult>(`/apps/${appID}/specs?dry_run=true`, { ...spec, egress: specOf(settled) }),
    enabled: changed,
    retry: false,
  });
  const verdict = changed && settled === draft ? decision : undefined;

  const save = useMutation({
    mutationFn: () => api.post(`/apps/${appID}/specs`, { ...spec, egress: specOf(draft) }),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['apps', appID] });
      onClose();
    },
  });

  return (
    <Dialog
      open
      onClose={onClose}
      title="Change where this app can connect"
      description="Saved as a new revision. It takes effect at the next deploy."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={save.isPending || sameDraft(draft, start) || Boolean(verdict?.isError)}
            onClick={() => save.mutate()}
          >
            {save.isPending ? 'Saving' : 'Save'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-5)' }}>
        <div>
          <p style={{ font: 'var(--type-label)', margin: '0 0 var(--space-1)' }}>The installation’s rules</p>
          <Quiet>
            {modeWords(mode)}
            {mode !== 'allow_all' && installList.length > 0 ? `: ${installList.join(', ')}` : ''}.{' '}
            Private addresses {data.install.block_private ? 'blocked' : 'not blocked'}.
          </Quiet>
        </div>

        {edits ? (
          <>
            <ListField
              label={edits.add.label}
              rows={3}
              value={draft.add}
              helper={edits.add.loosens ? `${loosens} ${ENTRY_FORMS}` : ENTRY_FORMS}
              onChange={(add) => set({ add })}
            />
            <ListField
              label={edits.remove.label}
              rows={3}
              value={draft.remove}
              helper={
                edits.remove.loosens
                  ? `${loosens} Each entry must match one in the installation’s list.`
                  : 'Each entry must match one in the installation’s list.'
              }
              onChange={(remove) => set({ remove })}
            />
          </>
        ) : (
          (draft.add.length > 0 || draft.remove.length > 0) && (
            // Kept visible so they can be cleared: the installation has no
            // list, so additions and removals change nothing.
            <>
              <ListField label="Added to the installation’s list" rows={2} value={draft.add} helper="The installation has no list, so these change nothing." onChange={(add) => set({ add })} />
              <ListField label="Removed from the installation’s list" rows={2} value={draft.remove} helper="The installation has no list, so these change nothing." onChange={(remove) => set({ remove })} />
            </>
          )
        )}

        <fieldset style={FIELDSET}>
          <legend style={LEGEND}>A list of this app’s own</legend>
          {(
            [
              ['inherit', 'None', 'Only the installation’s rules apply.'],
              ['allowlist', 'Only these', 'This app may reach only the destinations below, and only where the installation allows them too.'],
              ['denylist', 'Never these', 'This app may not reach the destinations below, whatever the installation allows.'],
            ] as const
          ).map(([value, label, description]) => (
            <Radio
              key={value}
              name="egress_own_mode"
              value={value}
              label={label}
              description={description}
              checked={draft.ownMode === value}
              onChange={() => set({ ownMode: value })}
            />
          ))}
        </fieldset>
        {draft.ownMode !== 'inherit' && (
          <ListField
            label={draft.ownMode === 'allowlist' ? 'Destinations this app may reach' : 'Destinations this app may not reach'}
            value={draft.ownList}
            helper={`Narrows the rules, so it never needs the installation’s permission. ${ENTRY_FORMS}`}
            onChange={(ownList) => set({ ownList })}
          />
        )}

        <fieldset style={FIELDSET}>
          <legend style={LEGEND}>Private addresses</legend>
          {(
            [
              ['inherit', `As the installation: ${data.install.block_private ? 'blocked' : 'not blocked'}`, ''],
              ['on', 'Block', 'This app can’t reach the local network, loopback, or a cloud metadata address.'],
              [
                'off',
                'Don’t block',
                data.install.block_private ? loosens : 'The installation doesn’t block them either.',
              ],
            ] as const
          ).map(([value, label, description]) => (
            <Radio
              key={value}
              name="egress_block_private"
              value={value}
              label={label}
              description={description || undefined}
              checked={draft.blockPrivate === value}
              onChange={() => set({ blockPrivate: value })}
            />
          ))}
        </fieldset>

        {verdict?.isError && <Banner tone="failed">{refusal(verdict.error)}</Banner>}
        {verdict?.data && dryRunWords(verdict.data).length > 0 && (
          <Banner tone="info">
            {dryRunWords(verdict.data).join(' ')}
          </Banner>
        )}
        {save.isError && <Banner tone="failed">{refusal(save.error)}</Banner>}
      </div>
    </Dialog>
  );
}

const FIELDSET: React.CSSProperties = {
  border: 0,
  margin: 0,
  padding: 0,
  display: 'flex',
  flexDirection: 'column',
  gap: 'var(--space-3)',
};

const LEGEND: React.CSSProperties = { font: 'var(--type-label)', color: 'var(--ink)', marginBottom: 'var(--space-2)' };
