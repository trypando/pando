// The Dependencies page of an app's Settings: each slot the app declared
// (R-130), and which of R-131's three ways fills it — Pando runs one, an
// existing one, or a value.
//
// Reading is app.view. Choosing a way is app.spec.edit, as PUT
// /apps/{id}/slots/{key} checks; a value is stored as a secret, which is also
// app.secrets.write. An unfilled required dependency blocks the next
// deployment (R-132), and says so in the warning color, never red: it is a
// gap to fill, not a failure.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Card, Icon, Input, StatusIndicator } from '@design';

import { api } from '@api/client';
import { messageOf } from '../install/Accounts';
import { LineSkeleton, Loading } from '../ui/Loading';
import { servicePlaceholder, slotLabel } from './discovery';
import { PageHead, Quiet } from './settingsParts';
import { AppVerb, useCan } from './verbs';

interface Slot {
  key: string;
  type: string;
  required?: boolean;
  resolution?: { mode: string; target?: string; secret_ref?: string };
}

type Way = 'provisioned' | 'bound' | 'literal';

/** Pando can stand these up itself (R-010). The rest are bound or given a value. */
const PROVISIONABLE = new Set(['postgres', 'mysql', 'redis', 'mongodb', 'memcached']);

const WAYS: Array<{ way: Way; label: string }> = [
  { way: 'provisioned', label: 'Pando runs one' },
  { way: 'bound', label: 'Use an existing one' },
  { way: 'literal', label: 'Enter a value' },
];

/** What a slot is, in words: the service, or "Value" for a bare one. */
function serviceName(slot: Pick<Slot, 'type'>): string {
  return slot.type && slot.type !== 'unknown' ? slotLabel(slot.type) : 'Value';
}

/** Where a slot stands, said shortly, and in the warning color when it blocks a deployment. */
function slotStatus(slot: Slot): { status: 'running' | 'info' | 'stopped' | 'building'; label: string; warn?: boolean } {
  switch (slot.resolution?.mode) {
    case 'provisioned':
      return { status: 'running', label: 'Pando runs it' };
    case 'bound':
      return { status: 'running', label: 'Connected' };
    case 'literal':
      return { status: 'running', label: 'Value set' };
    default:
      return slot.required
        ? { status: 'building', label: 'Needed to deploy', warn: true }
        : { status: 'stopped', label: 'Optional, not set up' };
  }
}

export function SettingsDependencies({ appID }: Readonly<{ appID: string }>) {
  const slots = useQuery({
    queryKey: ['slots', appID],
    queryFn: () => api.get<{ slots: Slot[] | null }>(`/apps/${appID}/slots`),
  });
  const rows = slots.data?.slots ?? [];

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      <PageHead title="Dependencies" line={rows.length > 0 ? 'Takes effect at the next deployment.' : undefined} />
      {slots.isError && <Banner tone="failed">{messageOf(slots.error)}</Banner>}
      {slots.isPending && (
        <Card tone="paper" padding="md">
          <Loading>
            <LineSkeleton width="30ch" />
            <LineSkeleton width="44ch" />
          </Loading>
        </Card>
      )}
      {slots.isSuccess && rows.length === 0 && (
        <Card tone="paper" padding="md">
          <Quiet>This app doesn&rsquo;t use any databases or other services.</Quiet>
        </Card>
      )}
      {rows.map((slot) => (
        <DependencyCard key={`${slot.key}:${slot.resolution?.mode ?? ''}`} appID={appID} slot={slot} />
      ))}
    </div>
  );
}

function DependencyCard({ appID, slot }: Readonly<{ appID: string; slot: Slot }>) {
  const queries = useQueryClient();
  const canEdit = useCan(AppVerb.SpecEdit);
  const canSecrets = useCan(AppVerb.SecretsWrite);
  const current = slot.resolution?.mode as Way | undefined;
  const [way, setWay] = useState<Way | undefined>(current);
  const [target, setTarget] = useState(slot.resolution?.target ?? '');
  const [value, setValue] = useState('');
  const status = slotStatus(slot);
  const name = serviceName(slot);

  const save = useMutation({
    mutationFn: (body: { mode: Way; target?: string; value?: string }) =>
      api.put(`/apps/${appID}/slots/${encodeURIComponent(slot.key)}`, body),
    onSuccess: () => {
      setValue('');
      void queries.invalidateQueries({ queryKey: ['slots', appID] });
      for (const part of ['specs', 'notices']) void queries.invalidateQueries({ queryKey: ['apps', appID, part] });
    },
  });

  const allowed = (w: Way) => {
    if (!canEdit) return false;
    if (w === 'provisioned') return PROVISIONABLE.has(slot.type);
    if (w === 'literal') return canSecrets;
    return true;
  };

  const choose = (w: Way) => {
    setWay(w);
    // Pando running one needs nothing more; the other two need a value first.
    if (w === 'provisioned' && current !== 'provisioned') save.mutate({ mode: 'provisioned' });
  };

  return (
    <Card tone="paper" padding="md">
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        <div style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'baseline', flexWrap: 'wrap', gap: 'var(--space-3)' }}>
          <span style={{ display: 'flex', alignItems: 'baseline', flexWrap: 'wrap', gap: 'var(--space-2)' }}>
            <span style={{ font: 'var(--type-label)', color: 'var(--ink)' }}>{name}</span>
            <span style={{ font: 'var(--type-code-sm)', color: 'var(--ink-secondary)' }}>{slot.key}</span>
          </span>
          <StatusIndicator
            status={status.status}
            label={status.label}
            style={status.warn ? { color: 'var(--contour-text)' } : undefined}
          />
        </div>

        <div role="radiogroup" aria-label={`How ${slot.key} is filled`} className="pando-segmented">
          {WAYS.map(({ way: w, label }) => (
            <button
              key={w}
              type="button"
              role="radio"
              aria-checked={way === w}
              disabled={!allowed(w) || save.isPending}
              title={w === 'provisioned' && !PROVISIONABLE.has(slot.type) ? `Pando doesn’t run ${name === 'Value' ? 'this' : name}.` : undefined}
              onClick={() => choose(w)}
            >
              {label}
            </button>
          ))}
        </div>

        {way === 'provisioned' && (
          <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
            Runs beside this app, reachable only by it, and backed up with it.
          </span>
        )}

        {way === 'bound' && (
          <div style={{ display: 'flex', alignItems: 'flex-end', flexWrap: 'wrap', gap: 'var(--space-2)' }}>
            <Input
              aria-label={`Address of the ${name} for ${slot.key}`}
              mono
              placeholder={servicePlaceholder(slot.type, undefined)}
              value={target}
              disabled={!canEdit}
              onChange={(e) => setTarget(e.target.value)}
              style={{ flex: '1 1 20rem' }}
            />
            {canEdit && (
              <Button
                variant="secondary"
                disabled={save.isPending || target.trim() === '' || (current === 'bound' && target === slot.resolution?.target)}
                onClick={() => save.mutate({ mode: 'bound', target: target.trim() })}
              >
                {save.isPending ? 'Saving' : 'Save'}
              </Button>
            )}
          </div>
        )}

        {way === 'literal' && (
          <div style={{ display: 'flex', alignItems: 'center', flexWrap: 'wrap', gap: 'var(--space-2)' }}>
            {/* Always a secret: the value is stored through the secrets adapter
                and never shown again (R-190, R-194). */}
            <div className="pando-vfield" style={{ flex: '1 1 20rem' }}>
              <input
                aria-label={`Value of ${slot.key}`}
                type="password"
                autoComplete="off"
                placeholder={current === 'literal' ? '••••••••••••' : 'No value'}
                value={value}
                disabled={!canSecrets}
                onChange={(e) => setValue(e.target.value)}
              />
              <span className="pando-vlock" title="Stored as a secret">
                <Icon name="lock" size={16} />
              </span>
            </div>
            {canSecrets && (
              <Button
                variant="secondary"
                disabled={save.isPending || value === ''}
                onClick={() => save.mutate({ mode: 'literal', value })}
              >
                {save.isPending ? 'Saving' : 'Save'}
              </Button>
            )}
          </div>
        )}

        {save.isError && <Banner tone="failed">{messageOf(save.error)}</Banner>}
      </div>
    </Card>
  );
}
