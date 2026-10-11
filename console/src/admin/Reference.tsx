// API and tools (R-261, R-262).
//
// Everything Pando does, it does through the API; the console, the CLI and MCP
// are clients of it. This screen is where to find the reference for each of
// them and where to mint a token to use them with.
//
// The reference itself is not drawn here. It is generated from Pando's code at
// each release and published at trypando.ai, so this screen links to it rather
// than keeping a second copy. `GET /api/v1/reference` still serves the running
// binary's own reference, built from its router, cobra tree and MCP tool list,
// for the CLI, for MCP and for AI's answers below (R-346).
//
// Open to anyone signed in. A developer with no administrative verb still has
// apps shared with them, still automates, and still needs a token; hiding the
// manual behind administration would make the API an administrative feature,
// which R-262 explicitly says it is not.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, CodeBlock, Dialog, InlineCode, Input } from '@design';

import { api } from '@api/client';
import type { Token } from '@api/types.gen';
import { InstallVerb, useInstallVerb } from '../app/principal';
import { Quiet, Screen } from '../install/Accounts';
import { MEASURE } from '../ui/layout';
import { relative } from '../ui/time';
import { Table } from '../ui/Table';
import { AIButton, AIDialog, AIPrompt, AnsweredBy } from '../ui/AskAI';
import { AI_PHRASES, AiThinking } from '../ui/AiThinking';
import { useAIFunctionState } from '../install/AIFunctions';
import { DOCS_URL, EVENTS_DOCS_URL } from '../ui/docs';
import { FailedBanner } from '../ui/ErrorCode';

/** What POST /ai/reference/answer answers (R-346). */
interface ReferenceAnswer {
  answer: string;
  cites: string[];
  covered: boolean;
  adapter_id?: string;
  model?: string;
}

/**
 * "How can I…", answered from the reference built into this installation
 * (R-346), in a dialog behind the AI button. The answer describes and cites;
 * the documentation is where to check it. Nothing to accept: it does nothing.
 */
function AskHow({ onClose }: { onClose: () => void }) {
  const [question, setQuestion] = useState('');
  const ask = useMutation({
    mutationFn: (q: string) => api.post<ReferenceAnswer>('/ai/reference/answer', { question: q }),
  });
  const a = ask.data;
  return (
    <AIDialog
      title="Ask AI how to do something"
      intro="Answered from the reference built into this installation: the API, the CLI and the MCP tools. AI explains how and cites what it used; it doesn't do anything."
      onClose={onClose}
      footer={
        <Button variant="ghost" onClick={onClose}>
          Close
        </Button>
      }
    >
      <AIPrompt
        label={a ? 'Ask another question' : 'What you want to do'}
        placeholder="How can I give a group access to one app?"
        pending={ask.isPending}
        error={ask.error}
        onAsk={(q) => {
          setQuestion(q);
          ask.mutate(q);
        }}
      />
      {ask.isPending && <AiThinking phrases={AI_PHRASES.reference} />}
      {a && (
        <>
          <Quiet>{question}</Quiet>
          <p style={{ font: 'var(--type-body-ui)', margin: 0, whiteSpace: 'pre-wrap' }}>{a.answer}</p>
          {a.cites.length > 0 && (
            <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-2)' }}>
              {a.cites.map((c) => (
                <InlineCode key={c}>{c}</InlineCode>
              ))}
            </div>
          )}
          <AnsweredBy adapter={a.adapter_id} model={a.model} />
        </>
      )}
    </AIDialog>
  );
}

/** The published references, one link each. */
const DOCS = [
  { label: 'API reference', href: `${DOCS_URL}/api`, about: 'Every endpoint, what it takes and the verb it asks for.' },
  { label: 'CLI reference', href: `${DOCS_URL}/cli`, about: 'Installing the CLI, signing it in, and every command.' },
  { label: 'MCP server', href: `${DOCS_URL}/mcp`, about: 'Connecting an agent, and every tool it gets.' },
  { label: 'Errors', href: `${DOCS_URL}/errors`, about: 'Every error code and what it means.' },
  { label: 'Events', href: EVENTS_DOCS_URL, about: 'Every event a subscription can receive, and how to verify a delivery.' },
];

export function Reference() {
  // Offered to everyone signed in unless reference help is known to be off:
  // most people cannot read which functions are on, and the answer says so.
  const aiState = useAIFunctionState('answer_reference');
  const [asking, setAsking] = useState(false);

  return (
    <Screen heading="API and tools" action={aiState !== 'off' && <AIButton onClick={() => setAsking(true)} />}>
      {asking && <AskHow onClose={() => setAsking(false)} />}

      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-7)', maxWidth: MEASURE }}>
        <Documentation />
        <Tokens />
      </div>
    </Screen>
  );
}

// --- documentation ---------------------------------------------------------

function Documentation() {
  // The one thing the published docs cannot know: where this installation is.
  // They write https://pando.example.com; this is what goes there.
  const origin = window.location.origin;

  return (
    <section>
      <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-3)' }}>Documentation</h4>
      <ul
        style={{
          listStyle: 'none',
          margin: 0,
          padding: 0,
          display: 'flex',
          flexDirection: 'column',
          gap: 'var(--space-3)',
        }}
      >
        {DOCS.map((d) => (
          <li key={d.href}>
            <a href={d.href} target="_blank" rel="noopener noreferrer" style={{ font: 'var(--type-body-ui)' }}>
              {d.label}
            </a>
            <div>
              <Quiet>{d.about}</Quiet>
            </div>
          </li>
        ))}
      </ul>
      <div style={{ marginTop: 'var(--space-4)' }}>
        <Quiet>
          This installation is at <InlineCode>{origin}</InlineCode>. Use it wherever the documentation
          shows <InlineCode>https://pando.example.com</InlineCode>.
        </Quiet>
      </div>
    </section>
  );
}

// --- tokens ----------------------------------------------------------------

function Tokens() {
  // Service tokens have their own verb (R-080): issuing a credential for an
  // automation is not the same trust as managing people.
  const canManageTokens = useInstallVerb(InstallVerb.TokensManage);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-7)', maxWidth: MEASURE }}>
      <TokenList
        heading="Your tokens"
        // R-058, R-059, said where somebody is about to make one.
        note="A token acts as you and holds exactly what you hold. If you lose access to an app, so does the token. If your account goes, the token goes with it."
        path="/tokens"
        queryKey={['tokens']}
      />

      {canManageTokens && (
        <TokenList
          heading="Service tokens"
          // R-060: the difference that matters, in the place where the choice
          // between the two is made.
          note="A service token is its own principal. It holds nothing until somebody shares an app with it, it appears in the audit log under its own name, and it outlives whoever created it."
          path="/tokens/service"
          queryKey={['tokens', 'service']}
        />
      )}
    </div>
  );
}

function TokenList({
  heading,
  note,
  path,
  queryKey,
}: {
  heading: string;
  note: string;
  path: string;
  queryKey: string[];
}) {
  const queries = useQueryClient();
  const [creating, setCreating] = useState(false);

  const tokens = useQuery({
    queryKey,
    queryFn: () => api.get<{ tokens: Token[] | null }>(path),
  });

  const revoke = useMutation({
    mutationFn: (id: string) => api.del(`/tokens/${id}`),
    onSuccess: () => queries.invalidateQueries({ queryKey }),
  });

  const rows = (tokens.data?.tokens ?? []).filter((t) => !t.revoked_at);

  return (
    <section>
      <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-4)' }}>
        <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>{heading}</h4>
        <Button onClick={() => setCreating(true)}>New token</Button>
      </div>
      <div style={{ marginTop: 'var(--space-2)' }}>
        <Quiet>{note}</Quiet>
      </div>

      {tokens.isError && <FailedBanner error={tokens.error} />}

      <div style={{ marginTop: 'var(--space-4)' }}>
        <Table
          loading={tokens.isPending}
          skeletonRows={2}
          columns={[
            { key: 'name', header: 'Name', width: 'minmax(0,30ch)' },
            {
              key: 'expires_at',
              header: 'Expires',
              width: '18ch',
              muted: true,
              render: (row: Token) =>
                row.expires_at ? new Date(row.expires_at).toLocaleDateString() : 'Never',
            },
            {
              key: 'last_used_at',
              header: 'Last used',
              width: '18ch',
              muted: true,
              render: (row: Token) => (row.last_used_at ? relative(row.last_used_at) : 'Never'),
            },
            {
              key: 'actions',
              header: '',
              width: '18ch',
              align: 'right',
              render: (row: Token) => (
                <Button
                  variant="destructive"
                  disabled={revoke.isPending}
                  onClick={() => revoke.mutate(row.id)}
                >
                  Revoke
                </Button>
              ),
            },
          ]}
          rows={rows}
          empty={<Quiet>No tokens.</Quiet>}
        />
      </div>

      {revoke.isError && <FailedBanner error={revoke.error} />}

      {creating && (
        <NewToken
          heading={heading}
          path={path}
          onClose={() => setCreating(false)}
          onCreated={() => void queries.invalidateQueries({ queryKey })}
        />
      )}
    </section>
  );
}

/** Expiries offered. A number of days, because that is what the API takes. */
const LIFETIMES = [
  { value: 30, label: '30 days' },
  { value: 90, label: '90 days' },
  { value: 365, label: 'A year' },
  { value: 0, label: 'Never' },
];

function NewToken({
  heading,
  path,
  onClose,
  onCreated,
}: {
  heading: string;
  path: string;
  onClose: () => void;
  onCreated: () => void;
}) {
  const [name, setName] = useState('');
  const [days, setDays] = useState(90);
  const [secret, setSecret] = useState<string | null>(null);

  const create = useMutation({
    mutationFn: () => api.post<{ secret: string }>(path, { name, expires_days: days }),
    onSuccess: (issued) => {
      setSecret(issued.secret);
      onCreated();
    },
  });

  // R-063: shown once. The dialog does not close on success — closing it would
  // take the only copy of the secret with it.
  if (secret) {
    return (
      <Dialog
        open
        onClose={onClose}
        title="Copy this now"
        description="This is the only time Pando will show it."
        footer={
          <Button variant="primary" onClick={onClose}>
            Done
          </Button>
        }
      >
        <CodeBlock lines={[secret]} />
      </Dialog>
    );
  }

  return (
    <Dialog
      open
      onClose={onClose}
      title={heading === 'Service tokens' ? 'New service token' : 'New token'}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={create.isPending || !name} onClick={() => create.mutate()}>
            {create.isPending ? 'Creating' : 'Create'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-5)' }}>
        <Input
          label="Name"
          value={name}
          helper="Where it will be used. This is what appears in the audit log."
          onChange={(e) => setName(e.target.value)}
        />

        <div>
          <p style={{ font: 'var(--type-label)', margin: '0 0 var(--space-2)' }}>Expires</p>
          <div style={{ display: 'flex', gap: 'var(--space-2)', flexWrap: 'wrap' }}>
            {LIFETIMES.map((option) => (
              <Button
                key={option.value}
                variant={days === option.value ? 'primary' : 'secondary'}
                onClick={() => setDays(option.value)}
              >
                {option.label}
              </Button>
            ))}
          </div>
          {days === 0 && (
            <div style={{ marginTop: 'var(--space-3)' }}>
              {/* R-061: host policy may cap this, and the server clamps rather
                  than refusing. Saying so beats a token that quietly expires
                  in ninety days. */}
              <Quiet>Host policy may still put an expiry on it.</Quiet>
            </div>
          )}
        </div>

        {create.isError && <FailedBanner error={create.error} />}
      </div>
    </Dialog>
  );
}

