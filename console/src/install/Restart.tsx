/**
 * Restarting Pando from the console (POST /restart).
 *
 * Adapters and the configuration file are read at startup (R-253), so saving
 * one ends with "restart Pando". This is the button for that, rather than a
 * command for someone with a shell on the host.
 */
import { useEffect, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Dialog, Toast } from '@design';
import { api } from '@api/client';
import { messageOf } from './Accounts';
import { WAIT_SECONDS, waitForRestart } from './restartWait';

async function startedAt(): Promise<string | undefined> {
  const out = await api.get<{ started_at?: string }>('/adapters');
  return out.started_at;
}

export function RestartButton({ onRestarted }: { onRestarted?: () => void }) {
  const queries = useQueryClient();
  const [stage, setStage] = useState<'idle' | 'confirm' | 'restarting'>('idle');
  const [error, setError] = useState<string | null>(null);
  // "Pando restarted.", once it is back: the dialog closes on its own, and
  // without this nothing on screen would say the restart happened.
  const [done, setDone] = useState(false);
  useEffect(() => {
    if (!done) return undefined;
    const timer = setTimeout(() => setDone(false), 6000);
    return () => clearTimeout(timer);
  }, [done]);

  const restart = async () => {
    setError(null);
    setDone(false);
    setStage('restarting');
    try {
      const before = await startedAt();
      await api.post('/restart');
      if (await waitForRestart(before, startedAt)) {
        // Everything on screen came from the process that just stopped.
        await queries.invalidateQueries();
        setStage('idle');
        setDone(true);
        onRestarted?.();
        return;
      }
      setError(
        `Pando has not come back after ${WAIT_SECONDS} seconds. Check its logs where it runs, for example docker compose logs pando.`,
      );
    } catch (e) {
      setError(messageOf(e));
    }
    setStage('confirm');
  };

  const restarting = stage === 'restarting';
  return (
    <>
      {/* Destructive: it takes every app offline for a moment and cuts off a
          build under way. Outlined red rather than filled, since it asks
          before it does anything. */}
      <Button variant="destructive" onClick={() => setStage('confirm')}>
        Restart Pando
      </Button>
      <Dialog
        open={stage !== 'idle'}
        title="Restart Pando?"
        description="Apps are unreachable for a few seconds, and any build or deploy in progress is interrupted."
        onClose={restarting ? undefined : () => setStage('idle')}
        footer={
          <>
            <Button variant="ghost" disabled={restarting} onClick={() => setStage('idle')}>
              Cancel
            </Button>
            <Button variant="primary" disabled={restarting} onClick={restart}>
              {restarting ? 'Restarting…' : 'Restart'}
            </Button>
          </>
        }
      >
        {error && <Banner tone="failed">{error}</Banner>}
      </Dialog>
      {done && (
        <Toast
          status="running"
          onDismiss={() => setDone(false)}
          style={{ position: 'fixed', right: 'var(--space-5)', bottom: 'var(--space-5)', zIndex: 70 }}
        >
          Pando restarted.
        </Toast>
      )}
    </>
  );
}
