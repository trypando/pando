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
  // A scan with no revision is the one detection took of the source, before
  // there was a configuration to attach it to.
  if (!scan.spec_id) return 'from the source, before this configuration';
  if (!scan.image) return 'of the source only, not the image it runs';
  if (!scan.commit) return 'of the running image only, not its source';
  // An app added as an image has no source but the image: the source it
  // names is the image's own digest.
  if (scan.image === scan.commit || scan.image.endsWith(`@${scan.commit}`)) return 'of the image it runs';
  return 'of the image it runs and its source';
}
