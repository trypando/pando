// An API error's code, linked to what it means (R-105).
//
// The message says what happened and what to do; the code is what to search
// for and branch on. Each one is an anchor on the published errors reference,
// so the code is the link to its own explanation.

import type { ReactNode } from 'react';
import { Banner } from '@design';

import { RequestFailed } from '@api/client';
import { messageOf, refusal } from '../install/Accounts';
import { errorDocsURL } from './docs';

function ErrorCode({ code }: Readonly<{ code: string }>) {
  return (
    <a
      href={errorDocsURL(code)}
      target="_blank"
      rel="noopener noreferrer"
      title="View in docs"
      style={{ font: 'var(--type-code-sm)', color: 'inherit', overflowWrap: 'anywhere' }}
    >
      {code}
    </a>
  );
}

/** The code of a failed request, when the server gave one. */
export function codeOf(error: unknown): string | undefined {
  return error instanceof RequestFailed ? error.code : undefined;
}

/**
 * The server's words followed by the code, linked. Nothing is added when the
 * request never reached the server and so has no code.
 */
export function WithCode({ code, children }: Readonly<{ code?: string; children: ReactNode }>) {
  return (
    <>
      {children}
      {code && (
        <>
          {' '}
          <ErrorCode code={code} />
        </>
      )}
    </>
  );
}

/**
 * The failed-request banner: the message as the server wrote it, its remedy
 * when `remedy` is set and the server gave one, and the code.
 */
export function FailedBanner({ error, remedy = false }: Readonly<{ error: unknown; remedy?: boolean }>) {
  return (
    <Banner tone="failed">
      <WithCode code={codeOf(error)}>{remedy ? refusal(error) : messageOf(error)}</WithCode>
    </Banner>
  );
}
