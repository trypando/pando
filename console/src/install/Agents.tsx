// The instructions every agent is given when it connects over MCP (R-424):
// the company's part, which administrators with install.policy.manage edit
// here, and Pando's own guidance, shown for reference and never edited.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, CodeBlock, Input } from '@design';

import { api } from '@api/client';
import { LineSkeleton } from '../ui/Loading';
import { Quiet, messageOf, refusal } from './Accounts';
import { SystemPage } from './SystemPage';

/** GET /instructions. */
export interface Instructions {
  text: string;
  builtin: string;
  company: string;
  updated_at: string | null;
  updated_by: string;
}

const KEY = ['instructions'];

/**
 * What the field holds once the server sends `incoming`: the server's value
 * while the field is as the server last left it (`last`), and otherwise the
 * person's own edits, which a save in flight or a later refresh must not
 * throw away.
 */
export function nextDraft(draft: string, last: string, incoming: string): string {
  return draft === last ? incoming : draft;
}

/** System's Agents page: the company's instructions, and Pando's guidance. */
export function Agents({ canEdit }: Readonly<{ canEdit: boolean }>) {
  const doc = useQuery({ queryKey: KEY, queryFn: () => api.get<Instructions>('/instructions') });
  return (
    <SystemPage title="Agents">
      {doc.isPending && <LineSkeleton width="40ch" />}
      {doc.isError && <Quiet>{messageOf(doc.error)}</Quiet>}
      {doc.data && <AgentsForm doc={doc.data} canEdit={canEdit} />}
    </SystemPage>
  );
}

/** The instructions once loaded: the field, its Save, and Pando's guidance. */
function AgentsForm({ doc, canEdit }: Readonly<{ doc: Instructions; canEdit: boolean }>) {
  const queries = useQueryClient();
  const [company, setCompany] = useState(doc.company);
  // The server's value the field was last given, to tell an untouched field
  // from an edited one when a new value arrives.
  const [last, setLast] = useState(doc.company);
  const [showBuiltin, setShowBuiltin] = useState(false);
  if (doc.company !== last) {
    setLast(doc.company);
    setCompany(nextDraft(company, last, doc.company));
  }

  const save = useMutation({
    mutationFn: () => api.put<Instructions>('/instructions', { company }),
    onSuccess: (saved) => queries.setQueryData(KEY, saved),
  });
  const changed = company.trim() !== doc.company;

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-5)' }}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        <Input
          as="textarea"
          rows={14}
          label="Company instructions"
          helper="Markdown, up to 32 KB. Followed over Pando's guidance."
          disabled={!canEdit}
          value={company}
          onChange={(e) => {
            if (save.isError) save.reset();
            setCompany(e.target.value);
          }}
        />
        {save.isError && <Banner tone="failed">{refusal(save.error)}</Banner>}
        {canEdit && (
          <div>
            <Button variant="primary" disabled={!changed || save.isPending} onClick={() => save.mutate()}>
              {save.isPending ? 'Saving' : 'Save'}
            </Button>
          </div>
        )}
      </div>

      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
        <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)' }}>
          <h3 style={{ font: 'var(--type-h3)', color: 'var(--ink)', margin: 0 }}>Pando's guidance</h3>
          <Button variant="ghost" aria-expanded={showBuiltin} onClick={() => setShowBuiltin((s) => !s)}>
            {showBuiltin ? 'Hide' : 'Show'}
          </Button>
        </div>
        {showBuiltin && <CodeBlock dense copyable lines={doc.builtin} />}
      </div>
    </div>
  );
}
