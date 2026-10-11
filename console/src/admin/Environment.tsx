// Environment variables the app needs and detection did not find.
//
// R-102 is "ask, never guess", and R-103 promises detection is reviewable
// rather than authoritative. Both imply the answer can be corrected — but the
// console offered no way to add a variable detection missed, so an app needing
// one setting Pando could not infer could not be configured at all without the
// API.
//
// Two kinds, and the difference is not cosmetic. A plain value goes into the
// spec, which is exportable and meant to be safe to hand to somebody (design 01
// §5). A secret goes through the secrets adapter and the spec carries only a
// reference (R-190, R-191) — so pasting an API key into the wrong box would put
// it in every export of this app forever. The form asks which, and defaults to
// the safe one.
//
// Two verbs, one for each kind. Every change here writes a spec revision, which
// is app.spec.edit; a secret is also a write to the secrets adapter, which is
// app.secrets.write. Without the first the list is read-only. Without the
// second a variable can still be set as an ordinary value, and a secret's row
// cannot be changed — a Save that stored half of it would be worse than none.

import { useEffect, useRef, useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Dialog, Input, Select } from '@design';

import { api } from '@api/client';
import type { AppSpec, EnvEntry, Workload } from '@api/types.gen';
import { Quiet, messageOf } from '../install/Accounts';
import { looksSensitive } from './sensitive';
import { Table } from '../ui/Table';
import { useNewestSpec } from './newestSpec';
import { AppVerb, useCan } from './verbs';

interface Row {
  key: string;
  workload: string;
  kind: 'value' | 'secret' | 'slot';
  shown: string;

  /** Declared by detection and never given a value. */
  unset: boolean;
}

export function Environment({ appID, focus }: { appID: string; focus?: boolean }) {
  const queries = useQueryClient();
  const [adding, setAdding] = useState(false);
  const [editing, setEditing] = useState<Row | null>(null);
  const canEdit = useCan(AppVerb.SpecEdit);
  const canWriteSecrets = useCan(AppVerb.SecretsWrite);
  const heading = useRef<HTMLElement>(null);

  useEffect(() => {
    if (focus) heading.current?.scrollIntoView({ behavior: 'smooth', block: 'start' });
  }, [focus]);

  // The newest revision is what an edit builds on, not the pinned one: two
  // edits in a row should both survive, and building each on the pinned spec
  // would silently discard the first.
  const { spec, pending, error } = useNewestSpec(appID);

  const save = useMutation({
    mutationFn: async (entry: { key: string; value: string; secret: boolean; workload: string }) => {
      if (!spec) throw new Error('no spec');

      let env: EnvEntry;
      if (entry.secret) {
        // Stored through the secrets adapter; the spec gets a reference. This
        // is what keeps "an exported spec is safe to hand to someone" true
        // without a special case for this one field.
        await api.put(`/apps/${appID}/secrets/${encodeURIComponent(entry.key)}`, { value: entry.value });
        env = { key: entry.key, secret_ref: entry.key, source: 'user' };
      } else {
        env = { key: entry.key, value: entry.value, source: 'user' };
      }

      const body: AppSpec = {
        ...spec,
        workloads: (spec.workloads ?? []).map((w: Workload) =>
          w.name === entry.workload
            ? { ...w, env: [...(w.env ?? []).filter((e) => e.key !== entry.key), env] }
            : w,
        ),
      };
      return api.post(`/apps/${appID}/specs`, body);
    },
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['apps', appID] });
      void queries.invalidateQueries({ queryKey: ['apps', appID, 'specs'] });
      setAdding(false);
      setEditing(null);
    },
  });

  const remove = useMutation({
    mutationFn: (row: Row) => {
      if (!spec) throw new Error('no spec');
      const body: AppSpec = {
        ...spec,
        workloads: (spec.workloads ?? []).map((w: Workload) =>
          w.name === row.workload ? { ...w, env: (w.env ?? []).filter((e) => e.key !== row.key) } : w,
        ),
      };
      return api.post(`/apps/${appID}/specs`, body);
    },
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['apps', appID] });
      void queries.invalidateQueries({ queryKey: ['apps', appID, 'specs'] });
    },
  });

  const rows = envRows(spec);
  const workloads = (spec?.workloads ?? []).map((w: Workload) => w.name);
  const unset = rows.filter((r) => r.unset);

  return (
    <section ref={heading}>
      <div style={{ display: 'flex', alignItems: 'baseline', justifyContent: 'space-between' }}>
        <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>Environment</h4>
        {spec && canEdit && (
          <Button variant="secondary" onClick={() => setAdding(true)}>
            Add variable
          </Button>
        )}
      </div>

      <Quiet>
        What the app reads from its environment.
        {canEdit && ' Pando works out what it can; add anything it missed.'}
      </Quiet>

      {error && <Banner tone="failed">{messageOf(error)}</Banner>}
      {remove.isError && <Banner tone="failed">{messageOf(remove.error)}</Banner>}

      {/* Detection reads the names out of `.env.example` and cannot know the
          values — they are the app's own keys and passwords. Saying how many
          are still empty is the difference between a list somebody scans and
          a list somebody finishes. Never a blocker: which of them the app
          actually needs is what the trial run is for (R-133, O-4). */}
      {!error && unset.length > 0 && (
        <Banner tone="info">
          {unset.length === 1
            ? `${unset[0]?.key} has no value yet. The app starts without it.`
            : `${unset.length} variables have no value yet. The app starts without them.`}
        </Banner>
      )}

      <div style={{ marginTop: 'var(--space-4)' }}>
        <Table
          loading={pending}
          columns={[
            { key: 'key', header: 'Name', width: 'minmax(0,26ch)', mono: true },
            { key: 'shown', header: 'Value', width: 'minmax(0,28ch)', mono: true, muted: true },
            { key: 'workload', header: 'Part of the app', width: '18ch', muted: true },
            ...(canEdit
              ? [
                  {
                    key: 'actions',
                    header: '',
                    width: '24ch',
                    align: 'right' as const,
                    render: (row: Row) =>
                      // A slot is filled on the Dependencies list, and removing it
                      // here would take away the only thing pointing at a database.
                      row.kind === 'slot' ? null : (
                        <span style={{ display: 'inline-flex', gap: 'var(--space-3)' }}>
                          {/* Detection declared these and had no values to put in
                              them, so the only action on a variable was to delete
                              it: a list of everything the app reads, and no way to
                              say what it reads. */}
                          {(row.kind !== 'secret' || canWriteSecrets) && (
                            <Button variant="secondary" onClick={() => setEditing(row)}>
                              {row.unset ? 'Set value' : 'Change'}
                            </Button>
                          )}
                          <Button variant="secondary" onClick={() => remove.mutate(row)}>
                            Remove
                          </Button>
                        </span>
                      ),
                  },
                ]
              : []),
          ]}
          rows={rows}
          empty={<Quiet>This app reads nothing from its environment.</Quiet>}
        />
      </div>

      {adding && spec && canEdit && (
        <VariableDialog
          workloads={workloads}
          canWriteSecrets={canWriteSecrets}
          onClose={() => setAdding(false)}
          onSave={(entry) => save.mutate(entry)}
          saving={save.isPending}
          error={save.isError ? messageOf(save.error) : undefined}
        />
      )}

      {editing && spec && canEdit && (
        <VariableDialog
          workloads={[editing.workload]}
          canWriteSecrets={canWriteSecrets}
          existing={editing}
          onClose={() => setEditing(null)}
          onSave={(entry) => save.mutate(entry)}
          saving={save.isPending}
          error={save.isError ? messageOf(save.error) : undefined}
        />
      )}
    </section>
  );
}

/** Every environment entry across the app's workloads, flattened for a table. */
function envRows(body?: AppSpec): Row[] {
  const out: Row[] = [];
  for (const w of body?.workloads ?? []) {
    for (const e of w.env ?? []) {
      const unset = !e.secret_ref && !e.slot_ref && (e.value ?? '') === '';
      out.push({
        key: e.key,
        workload: w.name,
        kind: e.secret_ref ? 'secret' : e.slot_ref ? 'slot' : 'value',
        unset,
        // A stored secret is never shown, here or anywhere (R-194). A slot
        // says where its value comes from rather than what it is, because at
        // this point it does not have one yet. An empty cell said nothing at
        // all — it reads as a value Pando lost rather than one nobody has
        // given yet.
        shown: e.secret_ref
          ? 'Set, and not shown'
          : e.slot_ref
            ? `From the ${e.slot_ref} dependency`
            : unset
              ? 'Not set'
              : (e.value ?? ''),
      });
    }
  }
  return out;
}

/**
 * One dialog for adding a variable and for giving one a value.
 *
 * The same two decisions either way — what it is called, and whether it is a
 * value or a secret — so the same form. With `existing`, the name is fixed:
 * it came from the app's own `.env.example` and renaming it here would leave
 * the app reading a variable nothing sets.
 */
function VariableDialog({
  workloads,
  canWriteSecrets,
  existing,
  onClose,
  onSave,
  saving,
  error,
}: {
  workloads: string[];
  /** Without app.secrets.write the value can only be an ordinary setting. */
  canWriteSecrets: boolean;
  existing?: { key: string; kind: 'value' | 'secret' | 'slot' };
  onClose: () => void;
  onSave: (entry: { key: string; value: string; secret: boolean; workload: string }) => void;
  saving: boolean;
  error?: string;
}) {
  const [key, setKey] = useState(existing?.key ?? '');
  const [value, setValue] = useState('');
  const [secret, setSecret] = useState(
    canWriteSecrets && (existing ? existing.kind === 'secret' || looksSensitive(existing.key) : false),
  );
  const [workload, setWorkload] = useState(workloads[0] ?? '');

  return (
    <Dialog
      open
      onClose={onClose}
      title={existing ? `Set ${existing.key}` : 'Add an environment variable'}
      description="This takes effect at the next deploy."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={saving || key.trim() === '' || value === '' || workload === ''}
            onClick={() => onSave({ key: key.trim(), value, secret, workload })}
          >
            {saving ? 'Saving' : existing ? 'Save' : 'Add variable'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-5)' }}>
        {!existing && (
          <Input
            label="Name"
            mono
            autoFocus
            value={key}
            placeholder="STRIPE_API_KEY"
            helper="Exactly as the app reads it."
            onChange={(e) => setKey(e.target.value)}
          />
        )}

        <Select
          label="Kind"
          value={secret ? 'secret' : 'value'}
          options={[
            { value: 'value', label: 'An ordinary setting' },
            { value: 'secret', label: 'A secret — a key, token or password' },
          ]}
          onChange={(e) => setSecret(e.target.value === 'secret')}
          // Shown and fixed rather than hidden: the choice exists, and the
          // helper says why it is not this person's to make.
          disabled={!canWriteSecrets}
          helper={canWriteSecrets ? undefined : 'You can’t store secrets for this app, so this is kept as an ordinary setting.'}
        />

        {/* Warned, not prevented. Which variables are credentials is the
            person's knowledge, not Pando's — but a key stored as a value is
            in every export of this app from then on, and that is worth one
            sentence at the moment it would happen (R-190, R-191). */}
        {!secret && looksSensitive(key) && (
          <Banner tone="info">
            {key} reads like a credential. An ordinary setting is kept in the app&rsquo;s
            configuration, which is exportable; a secret is stored separately and never shown.
          </Banner>
        )}

        <Input
          label="Value"
          mono
          type={secret ? 'password' : 'text'}
          value={value}
          helper={
            secret
              ? 'Stored separately and never shown again. It will not appear in this app’s exported configuration.'
              : 'Kept in the app’s configuration, which is exportable — don’t put a key or password here.'
          }
          onChange={(e) => setValue(e.target.value)}
        />

        {workloads.length > 1 && (
          <Select
            label="Part of the app"
            value={workload}
            options={workloads.map((w) => ({ value: w, label: w }))}
            onChange={(e) => setWorkload(e.target.value)}
          />
        )}

        {error && <Banner tone="failed">{error}</Banner>}
      </div>
    </Dialog>
  );
}
