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
import { Button, Card, CodeBlock, Dialog, InlineCode, Input, Skeleton } from '@design';

import { api } from '@api/client';
import type { Token } from '@api/types.gen';
import { InstallVerb, useInstallVerb } from '../app/principal';
import { Quiet, Screen } from '../install/Accounts';
import { day, stamp } from '../ui/time';
import { useNarrow } from '../ui/narrow';
import { CopyField } from '../ui/CopyField';
import { CardHead } from './settingsParts';
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
      intro="Answered from the API, CLI and MCP reference built into this installation."
      onClose={onClose}
      footer={
        <Button variant="ghost" onClick={onClose}>
          Close
        </Button>
      }
    >
      <AIPrompt
        label={a ? 'Ask another question' : 'Question'}
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
  { label: 'API reference', href: `${DOCS_URL}/api` },
  { label: 'CLI', href: `${DOCS_URL}/cli` },
  { label: 'MCP', href: `${DOCS_URL}/mcp` },
  { label: 'Errors', href: `${DOCS_URL}/errors` },
  { label: 'Events', href: EVENTS_DOCS_URL },
];

export function Reference() {
  // Offered to everyone signed in unless reference help is known to be off:
  // most people cannot read which functions are on, and the answer says so.
  const aiState = useAIFunctionState('answer_reference');
  const [asking, setAsking] = useState(false);

  return (
    <Screen heading="API and tools" action={aiState !== 'off' && <AIButton onClick={() => setAsking(true)} />}>
      {asking && <AskHow onClose={() => setAsking(false)} />}

      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-5)' }}>
        <Connect />
        <Tokens />
      </div>
    </Screen>
  );
}

// --- connecting ------------------------------------------------------------

/** A link to a reference, drawn as a small button. */
const docLink: React.CSSProperties = {
  display: 'inline-flex',
  alignItems: 'center',
  height: 'var(--control-console)',
  padding: '0 var(--space-3)',
  border: 'var(--border-width) solid var(--field-border)',
  borderRadius: 'var(--radius-sm)',
  font: 'var(--type-label)',
  color: 'var(--ink)',
  textDecoration: 'none',
};

function Connect() {
  // The one thing the published docs cannot know: where this installation
  // is. The CLI signs in to it, and an MCP client is added by its MCP
  // endpoint's address (R-413).
  const origin = window.location.origin;

  return (
    <Card padding="md" as="section" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      <CardHead title="Connect" />
      <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(min(100%, 20rem), 1fr))', gap: 'var(--space-4)' }}>
        <CopyField label="CLI" value={`pando login ${origin}`} />
        <CopyField label="MCP server" value={`${origin}/api/v1/mcp`} />
        <CopyField label="API" value={`${origin}/api/v1`} />
      </div>
      <nav
        aria-label="Documentation"
        style={{
          display: 'flex',
          flexWrap: 'wrap',
          gap: 'var(--space-2)',
          paddingTop: 'var(--space-3)',
          borderTop: 'var(--border-width) solid var(--rule)',
        }}
      >
        {DOCS.map((d) => (
          <a key={d.href} href={d.href} target="_blank" rel="noopener noreferrer" style={docLink}>
            {d.label}
          </a>
        ))}
      </nav>
    </Card>
  );
}

// --- tokens ----------------------------------------------------------------

function Tokens() {
  // Service tokens have their own verb (R-080): issuing a credential for an
  // automation is not the same trust as managing people.
  const canManageTokens = useInstallVerb(InstallVerb.TokensManage);

  return (
    <div
      style={{
        display: 'grid',
        // Side by side on a wide window, one above the other below about
        // 1100px, where two would squeeze their columns.
        gridTemplateColumns: 'repeat(auto-fit, minmax(min(100%, 30rem), 1fr))',
        gap: 'var(--space-5)',
        alignItems: 'start',
      }}
    >
      <TokenList heading="Your tokens" path="/tokens" queryKey={['tokens']} />
      {canManageTokens && <TokenList heading="Service tokens" path="/tokens/service" queryKey={['tokens', 'service']} />}
    </div>
  );
}

/** When a token expires, and when it was last used, in a line under its name. */
export function tokenLine(t: Pick<Token, 'expires_at' | 'last_used_at'>): string {
  const expires = t.expires_at ? `Expires ${day(t.expires_at)}` : 'Never expires';
  const used = t.last_used_at ? `used ${stamp(t.last_used_at)}` : 'never used';
  return `${expires} · ${used}`;
}

function TokenList({ heading, path, queryKey }: Readonly<{ heading: string; path: string; queryKey: string[] }>) {
  const queries = useQueryClient();
  const narrow = useNarrow();
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
  const revokeButton = (row: Token) => (
    <Button variant="destructive" disabled={revoke.isPending} onClick={() => revoke.mutate(row.id)}>
      Revoke
    </Button>
  );

  return (
    <Card padding="md" as="section" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)', minWidth: 0 }}>
      <CardHead title={heading} action={<Button onClick={() => setCreating(true)}>New token</Button>} />

      {tokens.isError && <FailedBanner error={tokens.error} />}

      {narrow ? (
        <TokenRows loading={tokens.isPending} rows={rows} action={revokeButton} />
      ) : (
        <Table
          loading={tokens.isPending}
          skeletonRows={2}
          columns={[
            { key: 'name', header: 'Name', width: 'minmax(0,1fr)' },
            {
              key: 'expires_at',
              header: 'Expires',
              width: '10ch',
              muted: true,
              render: (row: Token) => (row.expires_at ? day(row.expires_at) : 'Never'),
            },
            {
              key: 'last_used_at',
              header: 'Last used',
              width: '14ch',
              muted: true,
              render: (row: Token) => (row.last_used_at ? stamp(row.last_used_at) : 'Never'),
            },
            { key: 'actions', header: '', width: '12ch', align: 'right', render: revokeButton },
          ]}
          rows={rows}
          empty={<Quiet>No tokens.</Quiet>}
        />
      )}

      {revoke.isError && <FailedBanner error={revoke.error} />}

      {creating && (
        <NewToken
          heading={heading}
          path={path}
          onClose={() => setCreating(false)}
          onCreated={() => void queries.invalidateQueries({ queryKey })}
        />
      )}
    </Card>
  );
}

/** Tokens at phone width: a row each, its dates under its name. */
function TokenRows({
  loading,
  rows,
  action,
}: Readonly<{ loading: boolean; rows: Token[]; action: (row: Token) => React.ReactNode }>) {
  if (loading) return <Skeleton width="60%" />;
  if (rows.length === 0) return <Quiet>No tokens.</Quiet>;
  return (
    <div>
      {rows.map((t) => (
        <div
          key={t.id}
          style={{
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
            gap: 'var(--space-3)',
            padding: 'var(--space-2) 0',
            borderTop: 'var(--border-width) solid var(--rule)',
          }}
        >
          <span style={{ display: 'flex', flexDirection: 'column', minWidth: 0 }}>
            <span style={{ font: 'var(--type-label)', overflowWrap: 'anywhere' }}>{t.name}</span>
            <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>{tokenLine(t)}</span>
          </span>
          {action(t)}
        </div>
      ))}
    </div>
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
      // R-060: its own principal, holding only what is shared with it.
      description={heading === 'Service tokens' ? 'Holds nothing until an app is shared with it.' : undefined}
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

