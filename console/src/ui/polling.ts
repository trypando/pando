// How often the console asks the server again, in one place (issue #72).
//
// The target is five thousand consoles open at once. Every poll below is
// multiplied by that, so each is as slow as the screen can bear, asks only
// while somebody can see what it answers, and is shared between the
// components that read the same thing. The budget is written out in
// docs/design/notes-console-paths-issue-72.md; change it there as well.
//
// A poll stops by itself while the browser tab is hidden: React Query does not
// run `refetchInterval` in the background unless told to, and nothing here
// tells it to.

import { useEffect, useRef, useState } from 'react';

/** The app's status while a deploy runs: what notices it has finished. */
export const STATUS_DEPLOYING_MS = 3_000;
/** The app's status while a part is restarting, stopped or unhealthy. */
export const STATUS_UNSETTLED_MS = 5_000;
/** The app's status while every part is running as it should. */
export const STATUS_SETTLED_MS = 30_000;
/** A deploy waiting for somebody else's approval. */
export const AWAITING_MS = 30_000;
/** What the app's parts are using. Each reading costs the runtime a sample. */
export const USAGE_MS = 30_000;
/** The security report while a scan runs. */
export const SCANNING_MS = 5_000;
/** The security report while a deploy runs that may start a scan. */
export const SCAN_WATCH_MS = 10_000;

interface PartState {
  present: boolean;
  running: boolean;
  restarting: boolean;
  healthy: boolean | null;
}

/**
 * How often to ask for an app's status. Fast while something is moving —
 * a deploy, or a part that is restarting, missing or unhealthy, where a stale
 * count is what makes a crash loop look like one restart — and slow once
 * everything is running.
 */
export function statusInterval(
  appState: string | undefined,
  status: { state?: string; workloads?: PartState[] | null } | undefined,
): number {
  if (appState === 'deploying' || status?.state === 'deploying') return STATUS_DEPLOYING_MS;
  const state = status?.state ?? appState;
  if (state === 'degraded') return STATUS_UNSETTLED_MS;
  const unsettled = (status?.workloads ?? []).some(
    (p) => !p.present || !p.running || p.restarting || p.healthy === false,
  );
  return state === 'running' && unsettled ? STATUS_UNSETTLED_MS : STATUS_SETTLED_MS;
}

/**
 * How often to ask for an app's deployments. Not at all while a deploy runs:
 * the status poll notices it finish and asks once then. Slowly while a deploy
 * waits for approval, since somebody else's answer is what moves it.
 */
export function deploymentsInterval(deployments: Array<{ status: string }> | null | undefined): number | false {
  return (deployments ?? []).some((d) => d.status === 'awaiting_approval') ? AWAITING_MS : false;
}

/**
 * How often to ask for the security report: while a scan runs, and while a
 * deploy that may start one runs, and never when the section is off screen.
 */
export function securityInterval(scanning: boolean, watching: boolean, visible: boolean): number | false {
  if (!visible) return false;
  if (scanning) return SCANNING_MS;
  return watching ? SCAN_WATCH_MS : false;
}

/** A webhook delivery about to be tried for the first time. */
export const DELIVERY_FIRST_MS = 3_000;
/** The longest a subscription's open dialog waits to ask again. */
export const DELIVERY_MAX_MS = 60_000;

/**
 * How often an open subscription asks for its deliveries. Every three seconds
 * while one is about to be tried for the first time; for one waiting to retry,
 * when its next attempt is due, at most a minute apart — a delivery to an
 * endpoint that is down retries for hours, and the dialog can stay open that
 * long. Not at all once none is pending.
 */
export function deliveriesInterval(
  deliveries: Array<{ status: string; attempts: number; next_attempt_at?: string }> | undefined,
  now: number = Date.now(),
): number | false {
  const pending = (deliveries ?? []).filter((d) => d.status === 'pending');
  if (pending.length === 0) return false;
  if (pending.some((d) => d.attempts === 0)) return DELIVERY_FIRST_MS;
  const due = pending
    .map((d) => (d.next_attempt_at ? new Date(d.next_attempt_at).getTime() - now : DELIVERY_MAX_MS))
    .map((ms) => (Number.isNaN(ms) ? DELIVERY_MAX_MS : ms));
  return Math.min(DELIVERY_MAX_MS, Math.max(DELIVERY_FIRST_MS, Math.min(...due)));
}

/**
 * Whether the app record should be read again because its status says the
 * app has moved on: the deploy it shows has finished, or a crash has
 * degraded it. Only for a status read after the record, so a record that
 * disagrees with the status is read once per status poll at most.
 */
export function recordIsBehind(
  record: { state?: string; updatedAt: number } | undefined,
  status: { state?: string; updatedAt: number } | undefined,
): boolean {
  if (!record?.state || !status?.state) return false;
  return status.state !== record.state && status.updatedAt > record.updatedAt;
}

/**
 * Whether an element is on screen. A poll for a section below the fold — the
 * usage meters, the security findings — has nobody reading its answer.
 *
 * True until the observer says otherwise, and always where there is no
 * IntersectionObserver, so a section never waits on it to load.
 */
export function useOnScreen<T extends Element>(): [React.RefObject<T | null>, boolean] {
  const ref = useRef<T>(null);
  const [visible, setVisible] = useState(true);
  useEffect(() => {
    const el = ref.current;
    if (!el || typeof IntersectionObserver === 'undefined') return;
    const observer = new IntersectionObserver((entries) => {
      for (const entry of entries) setVisible(entry.isIntersecting);
    });
    observer.observe(el);
    return () => observer.disconnect();
  }, []);
  return [ref, visible];
}
