// Asking the audit log a question with AI (R-345).
//
// Nothing to accept here: a search creates nothing. AI turns the question into
// filters, Pando runs them, and AI summarizes what they found. Show these
// events puts those filters on the audit log, as ordinary filters a person can
// read and change, so the events shown are the log's own and not AI's account
// of them.

import { useState } from 'react';
import { useMutation } from '@tanstack/react-query';
import { Button } from '@design';

import { api } from '@api/client';
import { AIDialog, AIPrompt, AnsweredBy } from '../ui/AskAI';
import { AI_PHRASES, AiThinking } from '../ui/AiThinking';
import { Quiet } from './Accounts';
import type { SearchFilter } from './audit';

interface AuditSearch {
  filter: SearchFilter;
  summary: string;
  note?: string;
  matched: number;
  truncated?: boolean;
  adapter_id?: string;
  model?: string;
}

export function AuditAI({ onClose, onShow }: { onClose: () => void; onShow: (f: SearchFilter) => void }) {
  const [question, setQuestion] = useState('');
  const search = useMutation({
    mutationFn: (q: string) => api.post<AuditSearch>('/ai/audit/search', { question: q }),
  });
  const found = search.data;

  return (
    <AIDialog
      title="Ask AI about the audit log"
      intro="Ask a question about this installation. AI turns it into audit log filters and summarizes the results."
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Close
          </Button>
          <Button
            variant="primary"
            disabled={!found}
            onClick={() => {
              if (!found) return;
              onShow(found.filter);
              onClose();
            }}
          >
            Show these events
          </Button>
        </>
      }
    >
      <AIPrompt
        label={found ? 'Ask another question' : 'Question'}
        placeholder="Which apps did Dana create or delete last month?"
        pending={search.isPending}
        error={search.error}
        onAsk={(q) => {
          setQuestion(q);
          search.mutate(q);
        }}
      />
      {search.isPending && <AiThinking phrases={AI_PHRASES.audit} />}
      {found && (
        <>
          <Quiet>{question}</Quiet>
          <p style={{ font: 'var(--type-body-ui)', margin: 0 }}>{found.summary}</p>
          {found.note && <Quiet>{found.note}</Quiet>}
          <Quiet>
            {found.matched === 1 ? 'One event matched.' : `${found.matched}${found.truncated ? '+' : ''} events matched.`}
          </Quiet>
          <AnsweredBy adapter={found.adapter_id} model={found.model} />
        </>
      )}
    </AIDialog>
  );
}
