// What a scan read, in words (R-311).
//
// The score is only as good as what was scanned. A scan of the source alone
// says nothing about the operating system packages in the image that runs, and
// one of the image alone says nothing about the secrets in the tree — so a scan
// that read less than both says which part it read, rather than letting the
// number stand for more than it covers.

import type { Scan } from '@api/types.gen';

export function scanScope(scan: Pick<Scan, 'spec_id' | 'commit' | 'image'> | undefined): string {
  if (!scan) return '';
  // A scan with no revision is the one detection took, before there was a
  // configuration to attach it to: of the source alone for an app Pando
  // builds, or of the published image it will run.
  if (!scan.spec_id) {
    if (!scan.image) return 'from the source, before this configuration';
    return imageOnly(scan) ? 'of the image it will run' : 'of the image it will run and its source';
  }
  if (!scan.image) return 'of the source only, not the image it runs';
  if (!scan.commit) return 'of the running image only, not its source';
  if (imageOnly(scan)) return 'of the image it runs';
  return 'of the image it runs and its source';
}

/**
 * What a plan says in place of a score when its scan read the source alone,
 * for an app Pando builds (the report's `awaiting_image`). That number is the
 * source's, and the app runs an image nobody has built yet (R-311).
 */
export const AWAITING_IMAGE = 'Scored once the image is built';

// An app added as an image has no source but the image: the source it names
// is the image's own digest.
function imageOnly(scan: Pick<Scan, 'commit' | 'image'>): boolean {
  if (!scan.image || !scan.commit) return Boolean(scan.image);
  return scan.image === scan.commit || scan.image.endsWith(`@${scan.commit}`);
}
