// Drafting a policy change with AI (R-344).
//
// Describe the rule; AI proposes changes to host policy, each shown with what
// it was and what it would be. Untick a change to leave it out, or say what to
// change and AI re-drafts from what is kept. Before anything is saved the
// dialog shows which apps the kept changes would refuse at their next deploy
// (design 05 §3), the same check the Policy screen makes. Accept saves the
// policy; Reject discards the proposal. A field the startup configuration
// fixes is never changed, and says where it is set (R-271).

import { useEffect, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Checkbox } from '@design';

import { api } from '@api/client';
import { AIDialog, AIHeading, AIPrompt, AnsweredBy } from '../ui/AskAI';
import { AI_PHRASES, AiThinking } from '../ui/AiThinking';
import { Quiet, refusal } from './Accounts';
import { describeChange } from './policyChange';

type Doc = Record<string, unknown>;

/** How long the kept changes must stay put before their impact is asked for. */
const PREVIEW_SETTLE_MS = 400;

interface PolicyProposal {
  proposed: Doc;
  changes: { key: string; from: unknown; to: unknown }[];
  declined?: { key: string; reason: string }[];
  refused?: string[];
  reply: string;
  adapter_id?: string;
  model?: string;
}

interface Violation {
  app_id: string;
  app_name: string;
  message: string;
}



/** The proposal with the changes left out put back as they were. */
function kept(p: PolicyProposal, left: Set<string>): Doc {
  const doc = { ...p.proposed };
  for (const c of p.changes) {
    if (!left.has(c.key)) continue;
    if (c.from === null || c.from === undefined) delete doc[c.key];
    else doc[c.key] = c.from;
  }
  return doc;
}

export function PolicyAI({ onClose }: { onClose: () => void }) {
  const queries = useQueryClient();
  const [proposal, setProposal] = useState<PolicyProposal | null>(null);
  // Changes left out, by field. Everything proposed is kept until unticked.
  const [left, setLeft] = useState<Set<string>>(new Set());
  const doc = proposal ? kept(proposal, left) : null;
  const keeping = proposal ? proposal.changes.filter((c) => !left.has(c.key)) : [];

  const ask = useMutation({
    mutationFn: (description: string) =>
      api.post<PolicyProposal>('/ai/policy/draft', { description, proposed: doc ?? undefined }),
    onSuccess: (p) => {
      setProposal(p);
      setLeft(new Set());
    },
  });

  // Asked as soon as there is something to ask about, so the answer is on
  // screen before Accept rather than after it — once the changes kept have
  // stopped changing for a moment. Each preview reads apps on the server, and
  // unticking three changes in a row is one question, not three (issue #72).
  const docKey = JSON.stringify(doc);
  const [settledKey, setSettledKey] = useState(docKey);
  useEffect(() => {
    const timer = setTimeout(() => setSettledKey(docKey), PREVIEW_SETTLE_MS);
    return () => clearTimeout(timer);
  }, [docKey]);
  const settled = settledKey === docKey;
  const impact = useQuery({
    queryKey: ['policy-preview', settledKey],
    queryFn: () => api.post<{ violations: Violation[] | null }>('/policy/preview', doc),
    enabled: keeping.length > 0 && settled,
  });
  // Never an answer about a proposal that is no longer the one on screen.
  const violations = settled ? (impact.data?.violations ?? []) : [];

  const accept = useMutation({
    mutationFn: () => api.put('/policy', doc),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['policy'] });
      onClose();
    },
  });

  return (
    <AIDialog
      title="Ask AI to draft a policy change"
      intro="Describe the rule you want. AI proposes changes to host policy. Leave out any you don't want, or ask for something different, then accept to save the policy. Nothing is saved until you accept."
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            {proposal ? 'Reject' : 'Cancel'}
          </Button>
          <Button variant="primary" disabled={keeping.length === 0 || accept.isPending} onClick={() => accept.mutate()}>
            {accept.isPending ? 'Saving' : 'Accept'}
          </Button>
        </>
      }
    >
      <AIPrompt
        label={proposal ? 'Change the proposal' : 'The rule you want'}
        placeholder={proposal ? 'Allow it for administrators' : 'Nobody may open a shell in an app'}
        pending={ask.isPending}
        error={ask.error}
        onAsk={(text) => ask.mutate(text)}
      />
      {ask.isPending && <AiThinking phrases={AI_PHRASES.policy} />}

      {proposal && (
        <>
          {proposal.reply && <p style={{ font: 'var(--type-body-ui)', margin: 0 }}>{proposal.reply}</p>}

          <section style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
            <AIHeading>Changes</AIHeading>
            {proposal.changes.length === 0 ? (
              <Quiet>Nothing to change. The policy is as it was.</Quiet>
            ) : (
              proposal.changes.map((c) => (
                <Checkbox
                  key={c.key}
                  label={describeChange(c)
                    .map((d) => d.title)
                    .filter((t, i, all) => all.indexOf(t) === i)
                    .join('; ')}
                  description={describeChange(c)
                    .map((d) => d.detail)
                    .join(' ')}
                  checked={!left.has(c.key)}
                  onChange={(e) => {
                    const next = new Set(left);
                    if (e.target.checked) next.delete(c.key);
                    else next.add(c.key);
                    setLeft(next);
                  }}
                />
              ))
            )}
          </section>

          {[...(proposal.declined ?? []).map((d) => d.reason), ...(proposal.refused ?? [])].map((reason) => (
            <Quiet key={reason}>{reason}</Quiet>
          ))}

          {keeping.length > 0 &&
            (impact.isPending ? (
              <Quiet>Checking which apps this affects.</Quiet>
            ) : impact.isError ? (
              <Quiet>{refusal(impact.error)}</Quiet>
            ) : violations.length === 0 ? (
              <Banner tone="running">Nothing on this installation breaks under these rules.</Banner>
            ) : (
              <Banner tone="building">
                {violations.length === 1 && violations[0]
                  ? `${violations[0].app_name} keeps running and is refused at its next deploy: ${violations[0].message}`
                  : `${violations.length} apps keep running and are refused at their next deploy: ${violations
                      .map((v) => v.app_name)
                      .join(', ')}.`}
              </Banner>
            ))}

          <AnsweredBy adapter={proposal.adapter_id} model={proposal.model} />
          {accept.isError && <Quiet>{refusal(accept.error)}</Quiet>}
        </>
      )}
    </AIDialog>
  );
}
