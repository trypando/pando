// Updates — whether a newer Pando is released (R-351), and how to upgrade
// this installation to it (R-352).
//
// Read-only. The check's settings are host policy and live on the Policy
// screen with every other setting, so a value fixed in the startup config is
// shown read-only in one place rather than two.

import { useQuery } from '@tanstack/react-query';
import { Banner, Button, CodeBlock, InlineCode, StatusIndicator, Tag } from '@design';

import { api } from '@api/client';
import type { Release, Status } from '@api/types.gen';
import { Quiet, Screen, messageOf } from './Accounts';
import { relative } from '../ui/time';

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

          {st.available && st.upgrade && (
            <section style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
              <h2 style={{ font: 'var(--type-h3)', color: 'var(--ink)', margin: 0 }}>Upgrade to {st.upgrade.version}</h2>
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
