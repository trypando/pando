// The Variables page of an app's Settings: what each workload reads from its
// environment, edited in place and saved as one revision.
//
// Two verbs, one for each kind of value. Every change writes a spec revision,
// which is app.spec.edit; a secret is also a write to the secrets adapter,
// which is app.secrets.write. Without the first the page is read-only.
// Without the second a variable can still be given a plain value, and a
// secret's row is shown and cannot be changed — a Save that stored half of it
// would be worse than none.

import { useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Card } from '@design';

import { api } from '@api/client';
import { messageOf } from '../install/Accounts';
import { LineSkeleton, Loading } from '../ui/Loading';
import { slotLabel } from './discovery';
import { useNewestSpec } from './newestSpec';
import {
  changeCount,
  draftVars,
  emptyDraft,
  looksLikeCredential,
  newVar,
  saveVars,
  specVars,
  varProblems,
  type SettingsVar,
  type VarDraft,
} from './variableDraft';
import { PageHead, Quiet } from './settingsParts';
import { VariableEditor, type EditorGroup, type RowPatch } from './VariableEditor';
import { AppVerb, useCan } from './verbs';

interface Slot {
  key: string;
  type: string;
}

export function SettingsVariables({ appID }: Readonly<{ appID: string }>) {
  const queries = useQueryClient();
  const canEdit = useCan(AppVerb.SpecEdit);
  const canSecrets = useCan(AppVerb.SecretsWrite);
  // The newest revision is what an edit builds on, not the pinned one: two
  // edits in a row should both survive.
  const { spec, pending, error } = useNewestSpec(appID);
  const slots = useQuery({
    queryKey: ['slots', appID],
    queryFn: () => api.get<{ slots: Slot[] | null }>(`/apps/${appID}/slots`),
  });

  const [draft, setDraft] = useState<VarDraft>(emptyDraft);
  const next = useRef(0);

  const base = specVars(spec);
  const rows = draftVars(base, draft);
  const problems = varProblems(rows);
  const changes = changeCount(base, draft);

  const save = useMutation({
    mutationFn: async () => {
      if (!spec) throw new Error('no spec');
      const { body, secrets } = saveVars(spec, base, draft);
      // Stored through the secrets adapter first; the spec gets a reference.
      for (const s of secrets) {
        await api.put(`/apps/${appID}/secrets/${encodeURIComponent(s.key)}`, { value: s.value });
      }
      return api.post(`/apps/${appID}/specs`, body);
    },
    onSuccess: () => {
      setDraft(emptyDraft);
      for (const part of ['specs', 'notices', 'status']) {
        void queries.invalidateQueries({ queryKey: ['apps', appID, part] });
      }
      void queries.invalidateQueries({ queryKey: ['apps', appID] });
    },
  });

  const change = (id: string, patch: RowPatch) =>
    setDraft((d) => {
      if (d.added.some((v) => v.id === id)) {
        return { ...d, added: d.added.map((v) => (v.id === id ? { ...v, ...patch } : v)) };
      }
      const { key: _ignored, ...edit } = patch;
      return { ...d, edits: { ...d.edits, [id]: { ...d.edits[id], ...edit } } };
    });
  const remove = (id: string) =>
    setDraft((d) =>
      d.added.some((v) => v.id === id)
        ? { ...d, added: d.added.filter((v) => v.id !== id) }
        : { ...d, removed: [...d.removed, id] },
    );
  const add = (workload: string) => {
    next.current += 1;
    const id = `added-${next.current}`;
    setDraft((d) => ({ ...d, added: [...d.added, newVar(id, workload)] }));
  };

  const typeOf = (key: string) => (slots.data?.slots ?? []).find((s) => s.key === key)?.type;
  const groups: EditorGroup[] = (spec?.workloads ?? []).map((w) => ({
    workload: w.name,
    rows: rows.filter((v) => v.workload === w.name).map((v) => editorRow(v, problems[v.id], canSecrets, typeOf)),
  }));

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      <PageHead title="Variables" line="Takes effect at the next deployment." />
      {error && <Banner tone="failed">{messageOf(error)}</Banner>}
      {pending && (
        <Card tone="paper" padding="md">
          <Loading>
            <LineSkeleton width="30ch" />
            <LineSkeleton width="40ch" />
          </Loading>
        </Card>
      )}
      {!pending && groups.length === 0 && !error && (
        <Card tone="paper" padding="md">
          <Quiet>No variables.</Quiet>
        </Card>
      )}
      {groups.length > 0 && (
        <VariableEditor groups={groups} canEdit={canEdit} onChange={change} onRemove={remove} onAdd={add} />
      )}
      {save.isError && <Banner tone="failed">{messageOf(save.error)}</Banner>}
      {canEdit && changes > 0 && (
        <SaveBar
          count={changes}
          saving={save.isPending}
          blocked={Object.keys(problems).length > 0}
          onDiscard={() => setDraft(emptyDraft)}
          onSave={() => save.mutate()}
        />
      )}
    </div>
  );
}

/** A variable as the editor draws it on this page. */
function editorRow(
  v: SettingsVar,
  problem: string | undefined,
  canSecrets: boolean,
  typeOf: (slot: string) => string | undefined,
) {
  if (v.slot) {
    // Filled from a dependency: where it comes from, not what it is. It is
    // changed on Dependencies, and removing it here would take away the only
    // thing pointing at a database.
    const type = typeOf(v.slot);
    const service = type && type !== 'unknown' ? slotLabel(type) : v.slot;
    return { id: v.id, key: v.key, value: '', secret: false, lockable: false, removable: false, from: <span>From {service}</span> };
  }
  // A secret already stored is shown and fixed without app.secrets.write.
  const readOnly = v.stored && !canSecrets;
  let source: React.ReactNode;
  if (problem && !v.added) source = <span style={{ color: 'var(--marker-deep)' }}>{problem}</span>;
  else if (looksLikeCredential(v) && v.added) source = <span>Looks like a credential. Lock it to keep it secret.</span>;
  return {
    id: v.id,
    key: v.key,
    value: v.value,
    secret: v.secret,
    stored: v.stored,
    newName: v.added,
    nameError: v.added ? problem : undefined,
    source,
    readOnly,
    lockable: canSecrets,
    removable: !readOnly,
  };
}

/** The one Save for the page: how many changes, Discard, Save changes. */
function SaveBar({
  count,
  saving,
  blocked,
  onDiscard,
  onSave,
}: Readonly<{ count: number; saving: boolean; blocked: boolean; onDiscard: () => void; onSave: () => void }>) {
  return (
    <Card tone="paper" padding="sm">
      <div
        role="region"
        aria-label="Unsaved changes"
        style={{ display: 'flex', justifyContent: 'space-between', alignItems: 'center', flexWrap: 'wrap', gap: 'var(--space-3)' }}
      >
        <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink)' }}>
          {count === 1 ? '1 unsaved change' : `${count} unsaved changes`}
        </span>
        <span style={{ display: 'flex', gap: 'var(--space-2)' }}>
          <Button variant="ghost" disabled={saving} onClick={onDiscard}>
            Discard
          </Button>
          <Button variant="primary" disabled={saving || blocked} onClick={onSave}>
            {saving ? 'Saving' : 'Save changes'}
          </Button>
        </span>
      </div>
    </Card>
  );
}
