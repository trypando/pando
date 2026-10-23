// Where to report a bug or suggest an idea.
//
// Both open GitHub's own forms in a new tab, filed under the person's own
// GitHub account. Pando sends nothing itself: an installation holds no GitHub
// credential to file with, and a form on GitHub is the same form whether it is
// reached from here, from the README or from the CLI's output.
//
// A bug is an issue, from .github/ISSUE_TEMPLATE/bug.yml. An idea is a
// discussion in the Ideas category, from .github/DISCUSSION_TEMPLATE/ideas.yml,
// so that an open issue stays a piece of accepted work and an idea becomes one
// only once it is taken up.
//
// Sponsoring is a link to GitHub Sponsors, not GitHub's embedded button: that
// button is a frame from github.com, which every screen of every installation
// would then load, offline installations included.

import { runningVersion } from '@api/client';
import { Menu, MenuItem } from './Menu';

const REPOSITORY_URL = 'https://github.com/trypando/pando';

/** The bug form, with its Version field filled in when the version is known. */
export function bugReportURL(version: string | undefined): string {
  const query = new URLSearchParams({ template: 'bug.yml' });
  if (version) query.set('version', `pando ${version}`);
  return `${REPOSITORY_URL}/issues/new?${query.toString()}`;
}

/** A new discussion in the Ideas category. */
export const IDEA_URL = `${REPOSITORY_URL}/discussions/new?category=ideas`;

/** The project's GitHub Sponsors page, the one .github/FUNDING.yml names. */
export const SPONSOR_URL = 'https://github.com/sponsors/trypando';

/** Beside the inbox on every signed-in screen. */
export function FeedbackButton() {
  return (
    <Menu label="Report a bug or suggest an idea" icon="bug">
      {(close) => (
        <>
          {/* Read when the menu opens, by which time the screen behind it has
              made a signed-in request and the version is known. */}
          <MenuItem href={bugReportURL(runningVersion())} onSelect={close}>
            Report a bug
          </MenuItem>
          <MenuItem href={IDEA_URL} onSelect={close}>
            Suggest an idea
          </MenuItem>
          <MenuItem href={SPONSOR_URL} onSelect={close}>
            Sponsor Pando
          </MenuItem>
        </>
      )}
    </Menu>
  );
}
