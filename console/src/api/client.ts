// The API client.
//
// R-261: the API is the product, and the console is one of its clients with no
// capability the API lacks. So this is deliberately thin — a fetch wrapper that
// understands the error envelope and nothing else. Business logic that appears
// here instead of behind an endpoint is a capability the CLI and MCP will not
// have.

import type { Error as ApiError } from './types.gen';

export type { ApiError };

/** Thrown for any non-2xx response, carrying the envelope when there is one. */
export class RequestFailed extends Error {
  readonly status: number;
  readonly envelope: ApiError | null;

  constructor(status: number, envelope: ApiError | null, fallback: string) {
    // The envelope's message is written to the R-105 standard — self-contained,
    // actionable, no apology — so it is shown as-is. Rewriting it here would
    // undo that in the UI layer, which is the same mistake as paraphrasing a
    // detection question.
    super(envelope?.message ?? fallback);
    this.name = 'RequestFailed';
    this.status = status;
    this.envelope = envelope;
  }

  /** The remedy, when the server supplied one. */
  get remedy(): string | undefined {
    return this.envelope?.remedy || undefined;
  }

  get code(): string | undefined {
    return this.envelope?.code || undefined;
  }
}

/**
 * Where the API is, relative to wherever this page is being served.
 *
 * Normally `/api/v1`. But the sign-in page is also served from Pando's one
 * reserved path, `/.pando/login`, so that somebody who lands on an app's own
 * hostname or its own port has somewhere to sign in (R-172) — and on those
 * listeners `/api/v1` belongs to the app, not to Pando. The whole router is
 * mounted under the reserved prefix for exactly this reason, so the page asks
 * for the API where the page itself came from.
 */
export const base = reservedPrefix() + '/api/v1';

function reservedPrefix(): string {
  const prefix = '/.pando';
  const path = window.location.pathname;
  return path === prefix || path.startsWith(prefix + '/') ? prefix : '';
}

/**
 * A plain-text GET.
 *
 * `GET /apps/{id}/logs` answers `text/plain` — it is a runtime's output copied
 * through, not a document — and running that through `response.json()` throws
 * on the first line of it.
 */
async function requestText(path: string): Promise<string> {
  const response = await fetch(base + path, { credentials: 'same-origin' });

  if (!response.ok) {
    let envelope: ApiError | null = null;
    try {
      envelope = (await response.json()) as ApiError;
    } catch {
      envelope = null;
    }
    throw new RequestFailed(response.status, envelope, `The request failed (${response.status}).`);
  }

  return await response.text();
}

/**
 * A PUT whose body is a file, sent as-is.
 *
 * An app's image (R-340) is the image bytes, not JSON. The server decides what
 * the bytes are, so the content type sent is only the file's own claim.
 */
async function requestBytes<T>(path: string, file: Blob, method = 'PUT'): Promise<T> {
  const response = await fetch(base + path, {
    method,
    headers: { 'Content-Type': file.type || 'application/octet-stream' },
    body: file,
    credentials: 'same-origin',
  });

  if (!response.ok) {
    let envelope: ApiError | null = null;
    try {
      envelope = (await response.json()) as ApiError;
    } catch {
      envelope = null;
    }
    throw new RequestFailed(response.status, envelope, `The request failed (${response.status}).`);
  }

  return (await response.json()) as T;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const response = await fetch(base + path, {
    method,
    headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
    body: body === undefined ? null : JSON.stringify(body),
    credentials: 'same-origin',
  });

  if (!response.ok) {
    let envelope: ApiError | null = null;
    try {
      envelope = (await response.json()) as ApiError;
    } catch {
      // A response with no envelope is a failure the API did not expect to
      // produce. Reported as-is rather than dressed up as one it did.
      envelope = null;
    }
    throw new RequestFailed(response.status, envelope, `The request failed (${response.status}).`);
  }

  if (response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

export const api = {
  get: <T>(path: string) => request<T>('GET', path),
  text: (path: string) => requestText(path),
  post: <T>(path: string, body?: unknown) => request<T>('POST', path, body),
  put: <T>(path: string, body?: unknown) => request<T>('PUT', path, body),
  putFile: <T>(path: string, file: Blob) => requestBytes<T>(path, file),
  postFile: <T>(path: string, file: Blob) => requestBytes<T>(path, file, 'POST'),
  patch: <T>(path: string, body?: unknown) => request<T>('PATCH', path, body),
  del: <T>(path: string) => request<T>('DELETE', path),
};
