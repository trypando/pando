// Where Pando's documentation is.
//
// The API, CLI, MCP and error references are generated from Pando's code at
// each release and published on trypando.ai, so the console links to them
// rather than drawing a second copy. The site describes the latest release; an
// installation running an older one may differ in the newest additions.
// GET /api/v1/reference still describes exactly the running binary (R-261).

export const DOCS_URL = 'https://trypando.ai/docs';

/** The events reference: every event, its payload, and how to verify a delivery. */
export const EVENTS_DOCS_URL = `${DOCS_URL}/events`;

/** The page for one error code: each code is an anchor on the errors page. */
export function errorDocsURL(code: string): string {
  return `${DOCS_URL}/errors#${encodeURIComponent(code)}`;
}
