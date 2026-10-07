// Drafting a role and a group with AI (R-343).
//
// Describe who should be able to do what; AI drafts a role and a group from
// Pando's own permissions and shows it. Change it by hand — tick and untick
// permissions, add and remove people — or by saying what to change, which
// re-drafts from what is on screen, hand edits included. Accept creates it
// through the same endpoints Add group and Add role use, under your own
// authority; Reject discards it. Nothing exists until Accept.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, Checkbox, Input, Select } from '@design';

import { api } from '@api/client';
import { AIDialog, AIHeading, AIPrompt, AnsweredBy } from '../ui/AskAI';
import { PeopleField } from '../ui/PeopleField';
import type { Person } from '../ui/PeopleField';
import { AI_PHRASES, AiThinking } from '../ui/AiThinking';
import { Quiet, refusal } from './Accounts';

interface RoleDraft {
  name: string;
  scope: string;
  verbs: string[];
}

interface GroupDraft {
  name: string;
  members?: string[];
}

export interface AccessDraft {
  role?: RoleDraft;
  group?: GroupDraft;
  reply?: string;
  refused?: string[];
  adapter_id?: string;
  model?: string;
}

export function AccessAI({ onClose }: { onClose: () => void }) {
  const queries = useQueryClient();
  const [draft, setDraft] = useState<AccessDraft | null>(null);

  const verbs = useQuery({
    queryKey: ['verbs'],
    queryFn: () => api.get<{ verbs: { verb: string; scope: string }[] | null }>('/verbs'),
  });
  const users = useQuery({
    queryKey: ['users', 'people'],
    queryFn: () => api.get<{ users: Person[] }>('/users?limit=500'),
    retry: false,
  });
  const people = users.data?.users ?? [];

  const ask = useMutation({
    mutationFn: (description: string) =>
      api.post<AccessDraft>('/ai/access/draft', {
        description,
        current: draft ? { role: draft.role, group: draft.group } : undefined,
      }),
    onSuccess: (d) => setDraft(d),
  });

  const accept = useMutation({
    mutationFn: async (d: AccessDraft) => {
      let roleID = '';
      if (d.role) roleID = (await api.post<{ id: string }>('/roles', d.role)).id;
      if (d.group) {
        const group = await api.post<{ id: string }>('/groups', { name: d.group.name, members: d.group.members ?? [] });
        // An installation role is held by a group directly (R-080). An app
        // role is granted on each app, which is that app's decision.
        if (roleID && d.role?.scope === 'install') await api.put(`/groups/${group.id}/role`, { role_id: roleID });
      }
    },
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['groups'] });
      void queries.invalidateQueries({ queryKey: ['roles'] });
      onClose();
    },
  });

  const setRole = (role?: RoleDraft) => draft && setDraft({ ...draft, role });
  const setGroup = (group?: GroupDraft) => draft && setDraft({ ...draft, group });
  const inScope = (verbs.data?.verbs ?? []).filter((v) => v.scope === draft?.role?.scope);
  const members = draft?.group?.members ?? [];

  return (
    <AIDialog
      title="Ask AI to draft access"
      intro="Describe who should be able to do what. AI drafts a role and a group from Pando's permissions. Change the draft by hand or by asking, then accept it to create it. Nothing is created until you accept."
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {draft ? 'Reject' : 'Cancel'}
          </Button>
          <Button
            variant="primary"
            disabled={!draft || (!draft.role && !draft.group) || accept.isPending}
            onClick={() => draft && accept.mutate(draft)}
          >
            {accept.isPending ? 'Creating' : 'Accept'}
          </Button>
        </>
      }
    >
      <AIPrompt
        label={draft ? 'Change the draft' : 'Who should be able to do what'}
        placeholder={draft ? 'They should read logs too' : 'Release managers can deploy and restart any app'}
        pending={ask.isPending}
        error={ask.error}
        onAsk={(text) => ask.mutate(text)}
      />
      {ask.isPending && <AiThinking phrases={AI_PHRASES.access} />}

      {draft && (
        <>
          {draft.reply && <p style={{ font: 'var(--type-body-ui)', margin: 0 }}>{draft.reply}</p>}
          {(draft.refused ?? []).map((r) => (
            <Quiet key={r}>{r}</Quiet>
          ))}

          {draft.role ? (
            <section style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
              <AIHeading>Role</AIHeading>
              <div style={{ display: 'flex', gap: 'var(--space-3)', flexWrap: 'wrap' }}>
                <Input
                  label="Name"
                  value={draft.role.name}
                  onChange={(e) => setRole({ ...draft.role!, name: e.target.value })}
                  style={{ flex: '1 1 16rem' }}
                />
                <Select
                  label="Applies to"
                  value={draft.role.scope}
                  options={[
                    { value: 'app', label: 'One app at a time' },
                    { value: 'install', label: 'The whole installation' },
                  ]}
                  // A role holds permissions of one scope (R-080), so the
                  // other scope's are cleared rather than carried over.
                  onChange={(e) => setRole({ ...draft.role!, scope: e.target.value, verbs: [] })}
                />
              </div>
              <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fill, minmax(14rem, 1fr))', gap: 'var(--space-2)' }}>
                {inScope.map((v) => (
                  <Checkbox
                    key={v.verb}
                    label={v.verb}
                    checked={draft.role!.verbs.includes(v.verb)}
                    onChange={(e) =>
                      setRole({
                        ...draft.role!,
                        verbs: e.target.checked
                          ? [...draft.role!.verbs, v.verb].sort()
                          : draft.role!.verbs.filter((x) => x !== v.verb),
                      })
                    }
                  />
                ))}
              </div>
              <div>
                <Button variant="ghost" onClick={() => setRole(undefined)}>
                  Leave out the role
                </Button>
              </div>
            </section>
          ) : (
            <div>
              <Button variant="ghost" onClick={() => setRole({ name: '', scope: 'app', verbs: [] })}>
                Add a role
              </Button>
            </div>
          )}

          {draft.group ? (
            <section style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
              <AIHeading>Group</AIHeading>
              <Input label="Name" value={draft.group.name} onChange={(e) => setGroup({ ...draft.group!, name: e.target.value })} />
              <PeopleField
                label="People"
                people={people}
                value={members}
                onChange={(ids) => setGroup({ ...draft.group!, members: ids })}
              />
              <div>
                <Button variant="ghost" onClick={() => setGroup(undefined)}>
                  Leave out the group
                </Button>
              </div>
            </section>
          ) : (
            <div>
              <Button variant="ghost" onClick={() => setGroup({ name: '', members: [] })}>
                Add a group
              </Button>
            </div>
          )}

          <AnsweredBy adapter={draft.adapter_id} model={draft.model} />
          {draft.role?.scope === 'app' && draft.group && (
            <Quiet>An app role is granted to the group on each app, from that app&rsquo;s sharing settings.</Quiet>
          )}
          {accept.isError && <Quiet>{refusal(accept.error)}</Quiet>}
        </>
      )}
    </AIDialog>
  );
}
