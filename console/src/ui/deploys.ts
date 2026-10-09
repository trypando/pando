// How a deploy reads in a list.
//
// Its own file because the answer is not obvious and is asserted in a test: a
// deploy's status says whether Pando did the work, and the app it started may
// never have reported healthy. A history of three rows reading "Deployed", for
// an app that had not once served a request, is a true answer to a question
// nobody asked.

/**
 * A deploy's label with its place in the queue, while it waits for a build
 * slot (issue #93): how long a wait is depends on how many are ahead.
 */
export function deployLabelFor(d: { status: string; result_state?: string; queue_position?: number }): string {
  if (d.status === 'pending' && d.queue_position !== undefined) {
    return d.queue_position === 0 ? 'Queued, next' : `Queued, ${d.queue_position} ahead`;
  }
  return deployLabel(d.status, d.result_state);
}

export function deployLabel(status: string, result?: string): string {
  switch (status) {
    case 'succeeded':
      // A deploy that built, applied and routed is a deploy that worked, and
      // the app it started may never have reported healthy. Three rows reading
      // "Deployed" for an app that had not once served a request is a true
      // answer to the question nobody asked.
      return result === 'degraded' ? 'Deployed, not healthy' : 'Deployed';
    case 'failed':
      return 'Failed';
    case 'rolled_back':
      return 'Rolled back';
    case 'running':
      return 'Deploying';
    // Deploy approval (R-154 – R-156). A deploy waiting for somebody to say
    // yes has not started, and must never read as "Deploying": the person who
    // asked would watch a screen for something that is not happening.
    case 'awaiting_approval':
      return 'Waiting for approval';
    case 'rejected':
      return 'Rejected';
    case 'expired':
      return 'Expired, not approved';
    case 'superseded':
      return 'Replaced by a newer deploy';
    case 'pending':
      return 'Queued';
    default:
      return status.charAt(0).toUpperCase() + status.slice(1).replace(/_/g, ' ');
  }
}

/** Deploy statuses onto the design system's symbols. */
export function deployStatus(
  status: string,
  result?: string,
): 'running' | 'building' | 'failed' | 'stopped' | 'info' {
  switch (status) {
    // Nothing is happening to the app while a person decides, so not the
    // hollow ring, which means work under way.
    case 'awaiting_approval':
      return 'info';
    // A decision, not a failure: marker red is for failed apps and destructive
    // actions, and nothing broke. The dash for "did not go ahead".
    case 'rejected':
    case 'expired':
    case 'superseded':
      return 'stopped';
    case 'succeeded':
      // `building`, the same symbol a degraded app carries everywhere else:
      // not the failure symbol, because nothing failed, and not the running
      // one, because it is not running properly either.
      return result === 'degraded' ? 'building' : 'running';
    case 'failed':
      return 'failed';
    case 'rolled_back':
      return 'stopped';
    default:
      return 'building';
  }
}
