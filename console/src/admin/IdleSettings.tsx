// When nobody uses an app (R-393 – R-397, issue #131): whether Pando stops it,
// and later deletes it, and when that would be if nothing happens first.
//
// App settings, not spec (R-397): saving writes no revision and starts no
// deploy, so it is a PUT of its own rather than a spec edit like the deploy
// settings above it. The dates come from the server, which counts the notice
// the owner is owed (R-395), so the console never works one out differently.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Input } from '@design';

import { api } from '@api/client';
import type { App } from '@api/types.gen';
import { refusal } from '../install/Accounts';
import { invalidateApp } from './appList';
import { SwitchRow } from './settingsParts';
import { AppVerb, useCan } from './verbs';
import './onboarding.css';

/** GET /apps/{id}/idle. */
export interface IdleReport {
  stop_days: number | null;
  delete_days: number | null;
  install_stop_days: number;
  install_delete_days: number;
  effective_stop_days: number;
  effective_delete_days: number;
  last_activity_at: string;
  stops_at?: string;
  deletes_at?: string;
  stopped_for_idle: boolean;
}

/** One setting: the installation's, off, or a number of days of the app's own. */
export type Choice = 'install' | 'off' | 'days';

/** The choice an app's own value stands for: null is the installation's, 0 is never. */
export function choiceOf(own: number | null): Choice {
  if (own === null) return 'install';
  if (own === 0) return 'off';
  return 'days';
}

/** What a choice sends: null for the installation's, 0 for never, or whole days, at least one. */
export function idleValue(choice: Choice, days: string): number | null {
  if (choice === 'install') return null;
  if (choice === 'off') return 0;
  return Math.max(1, Math.round(Number(days) || 1));
}

const day = (iso: string) => new Date(iso).toLocaleDateString(undefined, { dateStyle: 'long' });

/**
 * What will happen to the app, in dates, or null when nothing will. An app
 * Pando already stopped still says when it would be deleted: that is the date
 * its owner most needs.
 */
export function outlook(report: IdleReport): string | null {
  const deletes = report.deletes_at ? `Pando deletes it on ${day(report.deletes_at)} if nobody uses it by then.` : '';
  if (report.stopped_for_idle) {
    return ['Pando stopped this app for being idle. Start it to use it again.', deletes]
      .filter(Boolean)
      .join(' ');
  }
  const stops = report.stops_at ? `If nobody uses it, Pando stops it on ${day(report.stops_at)}.` : '';
  return [stops, deletes].filter(Boolean).join(' ') || null;
}

export function IdleSection({ app }: Readonly<{ app: App }>) {
  const report = useQuery({
    queryKey: ['apps', app.id, 'idle'],
    queryFn: () => api.get<IdleReport>(`/apps/${app.id}/idle`),
  });
  if (!report.data) return null;
  return <IdleSettings app={app} report={report.data} />;
}

/** What the switch for one setting turns on: the installation's days when it has some, else the app's own. */
function turnedOn(install: number): Choice {
  return install > 0 ? 'install' : 'days';
}

/**
 * The two settings as switches, each saved as it flips. On, each says after
 * how long, and the number can be changed to the app's own.
 */
function IdleSettings({ app, report }: Readonly<{ app: App; report: IdleReport }>) {
  const queries = useQueryClient();
  const canEdit = useCan(AppVerb.SpecEdit);

  const save = useMutation({
    mutationFn: (body: { stop_days: number | null; delete_days: number | null }) =>
      api.put<IdleReport>(`/apps/${app.id}/idle`, body),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['apps', app.id, 'idle'] });
      void invalidateApp(queries, app.id);
    },
  });

  const stop = { stop_days: report.stop_days, delete_days: report.delete_days };
  const ahead = outlook(report);

  return (
    <div style={{ display: 'flex', flexDirection: 'column' }}>
      <IdleSwitch
        label="Stop when unused"
        verb="stopping"
        days={report.effective_stop_days}
        fallback={report.install_stop_days || 30}
        disabled={!canEdit || save.isPending}
        onSet={(choice, days) =>
          save.mutate({ ...stop, stop_days: idleValue(choice === 'on' ? turnedOn(report.install_stop_days) : choice, days) })
        }
      />
      <IdleSwitch
        label="Delete when unused"
        verb="deleting"
        days={report.effective_delete_days}
        fallback={report.install_delete_days || 90}
        disabled={!canEdit || save.isPending}
        onSet={(choice, days) =>
          save.mutate({ ...stop, delete_days: idleValue(choice === 'on' ? turnedOn(report.install_delete_days) : choice, days) })
        }
      />
      {/* What will happen, in dates (R-395, R-396). */}
      {ahead && <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', paddingTop: 'var(--space-2)' }}>{ahead}</span>}
      {save.isError && <Banner tone="failed">{refusal(save.error)}</Banner>}
    </div>
  );
}

function IdleSwitch({
  label,
  verb,
  days,
  fallback,
  disabled,
  onSet,
}: Readonly<{
  label: string;
  /** What Pando does, for the days field's name. */
  verb: string;
  /** In effect now; 0 is off. */
  days: number;
  /** The days a switch turned on with no installation default starts at. */
  fallback: number;
  disabled: boolean;
  /** `on` turns it on at the installation's days, or the fallback; `days` sets the app's own. */
  onSet: (choice: 'on' | 'off' | 'days', days: string) => void;
}>) {
  const [changing, setChanging] = useState(false);
  const [draft, setDraft] = useState(String(days || fallback));
  const on = days > 0;
  return (
    <>
      <SwitchRow
        label={label}
        line={on ? `After ${days} ${days === 1 ? 'day' : 'days'} without a visit` : 'Off'}
        checked={on}
        disabled={disabled}
        onChange={(checked) => onSet(checked ? 'on' : 'off', String(fallback))}
      >
        {on && !disabled && !changing && (
          <button type="button" className="pando-link" onClick={() => setChanging(true)}>
            Change
          </button>
        )}
      </SwitchRow>
      {changing && (
        <div style={{ display: 'flex', alignItems: 'flex-end', gap: 'var(--space-2)', paddingBottom: 'var(--space-2)' }}>
          <Input
            label={`Days before ${verb}`}
            type="number"
            min={1}
            value={draft}
            onChange={(e) => setDraft(e.target.value)}
            style={{ width: '10rem' }}
          />
          <Button
            variant="secondary"
            disabled={disabled}
            onClick={() => {
              onSet('days', draft);
              setChanging(false);
            }}
          >
            Save
          </Button>
          <Button variant="ghost" onClick={() => setChanging(false)}>
            Cancel
          </Button>
        </div>
      )}
    </>
  );
}
