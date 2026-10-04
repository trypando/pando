// Updates — whether a newer Pando is released (R-351), how to upgrade this
// installation to it (R-352), and upgrading it in place (R-355 – R-360).
//
// The check's settings and the in-place opt-in are host policy and live on the
// Policy screen with every other setting, so a value fixed in the startup
// config is shown read-only in one place rather than two.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Checkbox, CodeBlock, Dialog, InlineCode, Input, StatusIndicator, Tag } from '@design';

import { api } from '@api/client';
import type { Attempt, Plan, Release, Status } from '@api/types.gen';
import { InstallVerb, useInstallVerb } from '../app/principal';
import { Quiet, Screen, messageOf } from './Accounts';
import { relative } from '../ui/time';

/** The same rule as any backup's passphrase (backup.MinPassphraseLength). */
const MIN_PASSPHRASE = 16;

export function useUpdates(enabled: boolean) {
  return useQuery({
    queryKey: ['updates'],
    queryFn: () => api.get<Status>('/updates'),
    enabled,
    // The server checks every six hours; asking it hourly is plenty to show
    // what it found.
    refetchInterval: 60 * 60 * 1000,
  });
}

export function Updates({ onPolicy }: { onPolicy?: () => void }) {
  const updates = useUpdates(true);
  const st = updates.data;
  const policyLink = onPolicy && (
    <Button variant="ghost" onClick={onPolicy}>
      Open policy
    </Button>
  );

  return (
    <Screen heading="Updates">
      {updates.isError && <Quiet>{messageOf(updates.error)}</Quiet>}
      {st && (
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-6)', maxWidth: '72ch' }}>
          <Summary st={st} />

          {!st.enabled && (
            <Banner tone="info" action={policyLink}>
              The update check is off in host policy, so Pando has not looked for newer releases.
            </Banner>
          )}
          {st.error && <Banner tone="failed">{st.error}</Banner>}
          {st.enabled && !st.checked_at && !st.error && (
            <Quiet>Pando has not checked for releases yet. It checks at startup and every six hours.</Quiet>
          )}

          <LastUpgrade />

          {st.available && st.upgrade && (
            <section style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
              <h2 style={{ font: 'var(--type-h3)', color: 'var(--ink)', margin: 0 }}>Upgrade to {st.upgrade.version}</h2>
              <InPlace version={st.upgrade.version} onPolicy={onPolicy} />
              {st.breaking && (
                <Banner tone="info">
                  A version between this one and {st.upgrade.version} may break something that works now. Read
                  its Upgrade notes below before you upgrade.
                </Banner>
              )}
              <CodeBlock prompt copyable lines={st.upgrade.command} />
              <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
                {st.upgrade.instructions}
              </p>
            </section>
          )}

          {(st.releases ?? []).map((r) => (
            <ReleaseNotes key={r.version} release={r} />
          ))}
        </div>
      )}
    </Screen>
  );
}

function Summary({ st }: { st: Status }) {
  const row = { display: 'flex', gap: 'var(--space-4)', alignItems: 'baseline' } as const;
  const key = { font: 'var(--type-label)', color: 'var(--ink-secondary)', minWidth: '12ch' } as const;
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
      <div style={row}>
        <span style={key}>Running</span>
        <span style={{ font: 'var(--type-code)' }}>{st.development ? 'a development build' : st.current}</span>
      </div>
      {st.enabled && st.checked_at && (
        <>
          <div style={row}>
            <span style={key}>Latest</span>
            <span style={{ font: 'var(--type-code)' }}>{st.latest || 'none published'}</span>
            <Tag>{st.channel === 'prerelease' ? 'Release candidates included' : 'Stable'}</Tag>
          </div>
          <div style={row}>
            <span style={key}>Status</span>
            {st.available ? (
              <StatusIndicator
                status={st.security ? 'failed' : 'info'}
                label={st.security ? 'Update available, with a security fix' : 'Update available'}
              />
            ) : (
              <StatusIndicator status="running" label={st.development ? 'Not compared' : 'Up to date'} />
            )}
          </div>
          <div style={row}>
            <span style={key}>Checked</span>
            <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>{relative(st.checked_at)}</span>
          </div>
        </>
      )}
    </div>
  );
}

function ReleaseNotes({ release }: { release: Release }) {
  return (
    <article style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
      <header style={{ display: 'flex', gap: 'var(--space-3)', alignItems: 'baseline', flexWrap: 'wrap' }}>
        <h2 style={{ font: 'var(--type-h3)', color: 'var(--ink)', margin: 0 }}>
          {release.url ? <a href={release.url}>{release.version}</a> : release.version}
        </h2>
        {release.published_at && (
          <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
            {release.published_at.slice(0, 10)}
          </span>
        )}
        {release.security && <StatusIndicator status="failed" label="Security fix" />}
        {release.breaking && <Tag tone="contour">May break what worked before</Tag>}
        {release.prerelease && <Tag>Release candidate</Tag>}
      </header>
      <Changelog notes={release.notes} />
    </article>
  );
}

/**
 * A CHANGELOG.md section, rendered. The release workflow writes exactly the
 * shapes docs/releasing.md asks for — `###` headings, `-` bullets,
 * paragraphs, inline code — so those are all this reads; anything else is
 * shown as the text it is. Never HTML: release notes are text, and this
 * renders them as text.
 */
export function Changelog({ notes }: { notes: string }) {
  const blocks: React.ReactNode[] = [];
  let bullets: string[] = [];
  let para: string[] = [];
  const flush = () => {
    if (para.length) {
      blocks.push(
        <p key={blocks.length} style={{ font: 'var(--type-body-ui)', color: 'var(--ink)', margin: 0 }}>
          {inline(para.join(' '))}
        </p>,
      );
      para = [];
    }
    if (bullets.length) {
      blocks.push(
        <ul key={blocks.length} style={{ margin: 0, paddingLeft: 'var(--space-5)', display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
          {bullets.map((b, i) => (
            <li key={i} style={{ font: 'var(--type-body-ui)', color: 'var(--ink)' }}>
              {inline(b)}
            </li>
          ))}
        </ul>,
      );
      bullets = [];
    }
  };

  for (const raw of notes.split('\n')) {
    const line = raw.trimEnd();
    const heading = /^#{1,6}\s+(.*)$/.exec(line.trim());
    if (heading) {
      flush();
      blocks.push(
        <h3 key={blocks.length} style={{ font: 'var(--type-label)', color: 'var(--ink-secondary)', margin: 'var(--space-2) 0 0' }}>
          {heading[1]}
        </h3>,
      );
    } else if (/^\s*[-*]\s+/.test(line)) {
      if (para.length) flush();
      bullets.push(line.replace(/^\s*[-*]\s+/, ''));
    } else if (line.trim() === '') {
      flush();
    } else if (bullets.length && /^\s+/.test(line)) {
      bullets[bullets.length - 1] += ' ' + line.trim();
    } else {
      if (bullets.length) flush();
      para.push(line.trim());
    }
  }
  flush();
  return <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>{blocks}</div>;
}

/** Inline code between backticks; everything else as text. */
function inline(text: string): React.ReactNode[] {
  return text.split(/(`[^`]+`)/).map((part, i) =>
    part.startsWith('`') && part.endsWith('`') && part.length > 1 ? (
      <InlineCode key={i}>{part.slice(1, -1)}</InlineCode>
    ) : (
      part.replace(/\*\*([^*]+)\*\*/g, '$1')
    ),
  );
}

/**
 * The upgrade from here (R-355): a button when Pando can replace itself, and
 * otherwise every reason it cannot, each saying what to change. The manual
 * command below stays either way.
 */
function InPlace({ version, onPolicy }: { version: string; onPolicy?: () => void }) {
  const canUpgrade = useInstallVerb(InstallVerb.Upgrade);
  const [open, setOpen] = useState(false);
  const plan = useQuery({
    queryKey: ['upgrade-plan', version],
    queryFn: () => api.get<Plan>(`/upgrade?version=${encodeURIComponent(version)}`),
  });
  const p = plan.data;
  if (!p) return null;

  if (!p.possible) {
    const off = (p.reasons ?? []).some((r) => r.startsWith('In-place upgrades are off'));
    return (
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
        <p style={{ font: 'var(--type-label)', color: 'var(--ink)', margin: 0 }}>Pando can't upgrade itself from here</p>
        <ul style={{ margin: 0, paddingLeft: 'var(--space-5)', display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
          {(p.reasons ?? []).map((r) => (
            <li key={r} style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>
              {r}
            </li>
          ))}
        </ul>
        {off && onPolicy && (
          <div>
            <Button variant="secondary" onClick={onPolicy}>
              Open policy
            </Button>
          </div>
        )}
      </div>
    );
  }

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)', alignItems: 'flex-start' }}>
      <Button variant="primary" disabled={!canUpgrade} onClick={() => setOpen(true)}>
        Upgrade to {p.target}
      </Button>
      {!canUpgrade && <Quiet>Upgrading needs install.upgrade, which the Administrator role holds.</Quiet>}
      <Quiet>Or upgrade it yourself:</Quiet>
      {open && <UpgradeDialog plan={p} onClose={() => setOpen(false)} />}
    </div>
  );
}

/**
 * The confirmation (R-358, R-360): the outage, each breaking version's notes
 * with the version typed to go ahead, and the backup's passphrase or an
 * explicit choice to go without one. The same questions `pando upgrade` asks.
 */
function UpgradeDialog({ plan, onClose }: { plan: Plan; onClose: () => void }) {
  const queries = useQueryClient();
  const [typed, setTyped] = useState('');
  const [passphrase, setPassphrase] = useState('');
  const [skip, setSkip] = useState(false);
  const breaking = plan.breaking ?? [];
  const confirmed = breaking.length === 0 || typed.trim().replace(/^v/, '') === plan.target;
  const backupReady = skip || passphrase.length >= MIN_PASSPHRASE;

  const start = useMutation({
    mutationFn: () =>
      api.post<Attempt>('/upgrade', {
        version: plan.target,
        ...(breaking.length > 0 ? { confirm_breaking: plan.target } : {}),
        ...(skip ? { skip_backup: true } : { passphrase }),
      }),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['upgrade-last'] });
      onClose();
    },
  });

  return (
    <Dialog
      open
      title={`Upgrade Pando to ${plan.target}`}
      description={plan.note}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" disabled={!confirmed || !backupReady || start.isPending} onClick={() => start.mutate()}>
            {start.isPending ? 'Upgrading' : `Upgrade to ${plan.target}`}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        {breaking.map((r) => (
          <div key={r.version} style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
            <p style={{ font: 'var(--type-label)', color: 'var(--ink)', margin: 0 }}>
              {r.version} may break something that works now
            </p>
            <Changelog notes={r.notes} />
          </div>
        ))}
        {breaking.length > 0 && (
          <Input
            label={`Type ${plan.target} to upgrade anyway`}
            mono
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
          />
        )}

        {/* R-214: said before the passphrase is chosen, not after. */}
        <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink)', margin: 0 }}>
          Pando takes a full backup first. It doesn't keep this passphrase: if you lose it, nothing in the backup can be
          read again.
        </p>
        <Input
          label="Backup passphrase"
          type="password"
          autoComplete="new-password"
          disabled={skip}
          value={passphrase}
          helper={`At least ${MIN_PASSPHRASE} characters.`}
          onChange={(e) => setPassphrase(e.target.value)}
        />
        <Checkbox
          label="Upgrade without a backup"
          description="Pando still keeps a copy of its database to roll back to if the new version doesn't start. Skipping is recorded in the audit log."
          checked={skip}
          onChange={(e) => setSkip(e.target.checked)}
        />
        {start.isError && <Banner tone="failed">{messageOf(start.error)}</Banner>}
      </div>
    </Dialog>
  );
}

const STATES: Record<string, string> = {
  running: 'Under way',
  succeeded: 'Upgraded',
  rolled_back: 'Rolled back',
  failed: 'Failed',
};

/**
 * The last upgrade (R-356, R-359). While one is under way Pando restarts, so
 * this keeps asking until the answer is the outcome; a request that fails in
 * between is Pando being down, not an error to show.
 */
function LastUpgrade() {
  const last = useQuery({
    queryKey: ['upgrade-last'],
    queryFn: () => api.get<{ upgrade: Attempt | null }>('/upgrade/last'),
    retry: false,
    refetchInterval: (q) => (q.state.data?.upgrade?.state === 'running' || q.state.error ? 5_000 : false),
  });
  const a = last.data?.upgrade;
  if (!a) return null;
  if (a.state === 'succeeded' && a.finished_at && Date.now() - Date.parse(a.finished_at) > 24 * 60 * 60 * 1000) {
    return null;
  }

  if (a.state === 'running') {
    return (
      <Banner tone="running">
        Upgrading from {a.from} to {a.to}. Every app is unreachable while Pando restarts; this page updates when it is
        back.
      </Banner>
    );
  }
  if (a.state === 'succeeded') {
    return (
      <Banner tone="info">
        {STATES[a.state]} from {a.from} to {a.to} {a.finished_at ? relative(a.finished_at) : ''}.
        {a.reason ? ` ${a.reason}` : ''}
      </Banner>
    );
  }
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
      <Banner tone="failed">
        {STATES[a.state]}: {a.reason}
      </Banner>
      {a.logs && <CodeBlock title={`The last lines ${a.to} wrote`} lines={a.logs.trimEnd()} dense />}
    </div>
  );
}
