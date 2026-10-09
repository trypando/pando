// When nobody uses an app (R-393 – R-397, issue #131): whether Pando stops it,
// and later deletes it, and when that would be if nothing happens first.
//
// App settings, not spec (R-397): saving writes no revision and starts no
// deploy, so it is a PUT of its own rather than a spec edit like the deploy
// settings above it. The dates come from the server, which counts the notice
// the owner is owed (R-395), so the console never works one out differently.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Input, Select } from '@design';

import { api } from '@api/client';
import type { App } from '@api/types.gen';
import { MEASURE } from '../ui/layout';
import { refusal } from '../install/Accounts';
import { invalidateApp } from './appList';
import { AppVerb, useCan } from './verbs';

/** GET /apps/{id}/idle. */
interface IdleReport {
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
type Choice = 'install' | 'off' | 'days';

const choiceOf = (own: number | null): Choice => (own === null ? 'install' : own === 0 ? 'off' : 'days');

const day = (iso: string) => new Date(iso).toLocaleDateString(undefined, { dateStyle: 'long' });

export function IdleSection({ app }: { app: App }) {
  const report = useQuery({
    queryKey: ['apps', app.id, 'idle'],
    queryFn: () => api.get<IdleReport>(`/apps/${app.id}/idle`),
  });
  if (!report.data) return null;
  // A new report starts the form again from what it says.
  return <IdleSettings key={JSON.stringify([report.data.stop_days, report.data.delete_days])} app={app} report={report.data} />;
}

function IdleSettings({ app, report }: { app: App; report: IdleReport }) {
  const queries = useQueryClient();
  const canEdit = useCan(AppVerb.SpecEdit);
  const [stop, setStop] = useState<Choice>(choiceOf(report.stop_days));
  const [stopDays, setStopDays] = useState(String(report.stop_days || report.install_stop_days || 30));
  const [del, setDel] = useState<Choice>(choiceOf(report.delete_days));
  const [deleteDays, setDeleteDays] = useState(String(report.delete_days || report.install_delete_days || 90));

  const value = (choice: Choice, days: string) =>
    choice === 'install' ? null : choice === 'off' ? 0 : Math.max(1, Math.round(Number(days) || 1));

  const save = useMutation({
    mutationFn: () =>
      api.put<IdleReport>(`/apps/${app.id}/idle`, {
        stop_days: value(stop, stopDays),
        delete_days: value(del, deleteDays),
      }),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['apps', app.id, 'idle'] });
      void invalidateApp(queries, app.id);
    },
  });

  const installPhrase = (days: number) => (days > 0 ? `The installation's, ${days} days` : "The installation's, never");

  return (
    <section style={{ maxWidth: MEASURE }}>
      <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>When nobody uses it</h4>
      <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: '0 0 var(--space-4)' }}>
        Counted from the last time somebody used, deployed or started this app: {day(report.last_activity_at)}. Its
        owner is told 7 days before Pando acts. Changing these settings doesn't deploy anything.
      </p>

      {/* What will happen, in dates, before any control. */}
      {report.stopped_for_idle ? (
        <Banner tone="info">Pando stopped this app because nobody had used it. Start it to use it again.</Banner>
      ) : (
        (report.stops_at || report.deletes_at) && (
          <Banner tone="info">
            {report.stops_at && `If nobody uses it, Pando stops it on ${day(report.stops_at)}.`}
            {report.stops_at && report.deletes_at && ' '}
            {report.deletes_at && `Pando deletes it on ${day(report.deletes_at)} if nobody uses it by then.`}
          </Banner>
        )
      )}

      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)', marginTop: 'var(--space-4)' }}>
        <Select
          label="Stop it"
          value={stop}
          disabled={!canEdit}
          onChange={(e) => setStop(e.target.value as Choice)}
          options={[
            { value: 'install', label: installPhrase(report.install_stop_days) },
            { value: 'off', label: 'Never' },
            { value: 'days', label: 'After a number of days' },
          ]}
        />
        {stop === 'days' && (
          <Input
            label="Days without use before Pando stops it"
            type="number"
            value={stopDays}
            disabled={!canEdit}
            onChange={(e) => setStopDays(e.target.value)}
          />
        )}
        <Select
          label="Delete it"
          value={del}
          disabled={!canEdit}
          onChange={(e) => setDel(e.target.value as Choice)}
          options={[
            { value: 'install', label: installPhrase(report.install_delete_days) },
            { value: 'off', label: 'Never' },
            { value: 'days', label: 'After a number of days' },
          ]}
        />
        {del === 'days' && (
          <Input
            label="Days without use before Pando deletes it"
            type="number"
            value={deleteDays}
            disabled={!canEdit}
            helper="Counted from the same last use as stopping, so it has to be longer."
            onChange={(e) => setDeleteDays(e.target.value)}
          />
        )}
        {save.isError && <Banner tone="failed">{refusal(save.error)}</Banner>}
        {canEdit && (
          <div>
            <Button variant="secondary" disabled={save.isPending} onClick={() => save.mutate()}>
              {save.isPending ? 'Saving' : 'Save'}
            </Button>
          </div>
        )}
      </div>
    </section>
  );
}
