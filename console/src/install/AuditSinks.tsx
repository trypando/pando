// Where copies of the audit log go (R-383, R-385).
//
// Shown above the log itself, on the screen people read it on: an enabled
// audit sink sends every event off the installation as it is written, and
// nobody reading the audit log should be able to miss that a copy is leaving
// — as R-337 does for what AI is sent. The sentence is the server's
// (auditstream.Describe), so the console and the API say the same thing.

import { useQuery } from '@tanstack/react-query';
import { Banner, Button, StatusIndicator, Tooltip } from '@design';

import { api } from '@api/client';
import { relative } from '../ui/time';
import { messageOf } from './Accounts';

/** One audit sink as GET /audit/sinks returns it. */
export interface AuditSink {
  id: string;
  kind: string;
  name: string;
  enabled: boolean;
  declared?: boolean;
  transport?: string;
  endpoint?: string;
  format?: string;
  actions?: string[];
  exclude?: string[];
  /** Events past the cursor, at most 100,000. */
  backlog: number;
  /** There are at least `backlog` events past the cursor. */
  backlog_capped?: boolean;
  /** Why the sink cannot be built from its settings. */
  unusable?: string;
  adapter_id: string;
  delivered_at?: string;
  /** When the last event delivered was written. */
  cursor_at?: string;
  delivered_count: number;
  last_error?: string;
  last_error_at?: string;
  failing_since?: string;
  attempts: number;
  next_attempt_at?: string;
  /** Set when Pando turned the sink off after a day of failures. */
  disabled_at?: string;
  disabled_reason?: string;
  /** Events this sink never received: removed from the live log while it was off. */
  gap_from?: string;
  gap_to?: string;
  /** "Every audit event is sent to splunk.example.com:8088 over HTTPS." */
  disclosure: string;
}

/** The id of the Archived months section, which a gap notice points to. */
export const ARCHIVES_ANCHOR = 'archived-months';

export function useAuditSinks() {
  return useQuery({
    queryKey: ['audit-sinks'],
    queryFn: () => api.get<{ audit_sinks: AuditSink[] | null }>('/audit/sinks'),
  });
}

/** Every audit sink, with what it sends where and how far it has got. */
export function AuditSinks({ onAdapters }: { onAdapters?: () => void }) {
  const sinks = useAuditSinks();
  if (sinks.isError) {
    return (
      <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: '0 0 var(--space-5)' }}>
        {messageOf(sinks.error)}
      </p>
    );
  }
  return <AuditSinkList sinks={sinks.data?.audit_sinks ?? []} onAdapters={onAdapters} />;
}

export function AuditSinkList({ sinks, onAdapters }: { sinks: AuditSink[]; onAdapters?: () => void }) {
  // No sink is the usual case, and says nothing that needs saying here.
  if (sinks.length === 0) return null;
  const on = sinks.filter((s) => s.enabled);
  // Off because Pando turned it off: that is news, and its gap is a hole in
  // somebody's SIEM, so it is shown in full. Off because an administrator
  // turned it off is one line.
  const stopped = sinks.filter((s) => !s.enabled && s.disabled_at);
  const off = sinks.filter((s) => !s.enabled && !s.disabled_at);

  return (
    <section
      aria-label="Audit sinks"
      style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)', marginBottom: 'var(--space-5)' }}
    >
      {[...on, ...stopped].map((s) => (
        <Sink key={s.id} sink={s} />
      ))}
      {off.map((s) => (
        <p key={s.id} style={LINE}>
          <StatusIndicator status="stopped" label="Off" /> {s.name}
          {s.endpoint ? ` (${s.endpoint})` : ''} is turned off and sends nothing.
          {s.gap_from && s.gap_to && <> {gapNotice(s.gap_from, s.gap_to)}</>}
        </p>
      ))}
      {onAdapters && (
        <div>
          <Button variant="ghost" onClick={onAdapters}>
            Open adapters
          </Button>
        </div>
      )}
    </section>
  );
}

const LINE: React.CSSProperties = { font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 };

function Sink({ sink: s }: { sink: AuditSink }) {
  const failing = Boolean(s.enabled && s.failing_since);
  const turnedOff = !s.enabled && Boolean(s.disabled_at);

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-2)' }}>
      {s.enabled ? (
        <Banner tone={failing || s.unusable ? 'failed' : 'info'}>{s.disclosure}</Banner>
      ) : (
        <Banner tone="failed">
          Turned off by Pando{s.disabled_at && <> <When iso={s.disabled_at} /></>}: {s.name}
          {s.endpoint ? ` (${s.endpoint})` : ''} receives no audit events until it is turned back on in Adapters.
          {s.disabled_reason && <> {sentence(s.disabled_reason)}</>}
        </Banner>
      )}

      <p style={LINE}>
        <StatusIndicator
          status={failing || s.unusable ? 'failed' : turnedOff ? 'stopped' : 'running'}
          label={s.unusable ? 'Unusable' : failing ? 'Failing' : turnedOff ? 'Turned off by Pando' : 'Delivering'}
        />{' '}
        {s.name}, <span style={{ font: 'var(--type-code-sm)' }}>{s.adapter_id}</span>.{' '}
        {s.delivered_at ? (
          <>
            Last delivered <When iso={s.delivered_at} />.
          </>
        ) : (
          'Nothing delivered yet.'
        )}{' '}
        {s.cursor_at && (
          <>
            Has every event up to <When iso={s.cursor_at} />.{' '}
          </>
        )}
        {behind(s)}
      </p>

      {s.unusable && <p style={LINE}>Pando cannot send to it as configured. {sentence(s.unusable)}</p>}

      {failing && (
        <p style={LINE}>
          Failing since {new Date(s.failing_since!).toLocaleString()}, {s.attempts === 1 ? '1 attempt' : `${s.attempts} attempts`}.
          {s.last_error && (
            <>
              {' '}
              Last error
              {s.last_error_at && (
                <>
                  {' '}
                  <When iso={s.last_error_at} />
                </>
              )}
              : <span style={{ font: 'var(--type-code-sm)', color: 'var(--ink)' }}>{s.last_error}</span>
            </>
          )}
          {s.next_attempt_at && <> Next attempt at {new Date(s.next_attempt_at).toLocaleString()}.</>}
        </p>
      )}

      {s.gap_from && s.gap_to && <p style={LINE}>{gapNotice(s.gap_from, s.gap_to)}</p>}
    </div>
  );
}

/** How far behind a sink is, in events. */
function behind(s: AuditSink): string {
  if (s.backlog_capped) return `At least ${s.backlog.toLocaleString()} events behind.`;
  if (s.backlog === 0) return 'Up to date.';
  return s.backlog === 1 ? '1 event behind.' : `${s.backlog.toLocaleString()} events behind.`;
}

/** The range a sink missed, and where those events still are (R-386). */
function gapNotice(from: string, to: string) {
  return (
    <>
      Missed events from {new Date(from).toLocaleString()} to {new Date(to).toLocaleString()}, removed from the live
      log while it was off. The archive for those months is the backfill: download it from{' '}
      <a href={`#${ARCHIVES_ANCHOR}`}>Archived months</a> below.
    </>
  );
}

/** A relative time, with the exact one on hover. */
function When({ iso }: { iso: string }) {
  const r = relative(iso);
  return <Tooltip content={new Date(iso).toLocaleString()}>{r === 'Just now' || r === 'Yesterday' ? r.toLowerCase() : r}</Tooltip>;
}

/** A server's text as a sentence: its own final stop, or one added. */
function sentence(text: string): string {
  const t = text.trim();
  return /[.?!]$/.test(t) ? t : `${t}.`;
}
