// The contributor agreement check (CONTRIBUTOR_AGREEMENT.md, issue #49).
//
// Every person who authored or co-authored a commit in a pull request must have
// signed the agreement once, by posting PHRASE as a comment on a pull request.
// Co-authors come from Co-authored-by trailers; AI tools named there are exempt
// (agreement section 7). A signature
// is recorded as one JSON entry on the SIGNATURE_BRANCH, keyed on the GitHub
// user ID rather than the login, because a login can be renamed and an ID
// cannot. The entry links the comment and names the agreement's version and
// the git blob it had, so the record says which text was agreed to, not only
// that something was.
//
// Run by .github/workflows/contributor-agreement.yml under pull_request_target
// and issue_comment. It reads pull request metadata through the API and never
// checks out or runs anything from the pull request.

const PHRASE = 'I have read the Contributor License Agreement and I agree to it.';
const VERSION = '1.0';
const DOCUMENT = 'CONTRIBUTOR_AGREEMENT.md';
const SIGNATURE_BRANCH = 'contributor-agreement-signatures';
const SIGNATURE_FILE = `signatures/v${VERSION}.json`;
const STATUS_CONTEXT = 'Contributor agreement';
const MARKER = '<!-- contributor-agreement -->';

// The licensee does not license to themself. Bots are exempt by account
// type, not by name, below.
const EXEMPT_LOGINS = new Set(['ben-meeker']);

// Case, surrounding whitespace, backticks and the closing period do not make a
// signature different.
function normalize(text) {
  return (text || '')
    .replace(/`/g, '')
    .trim()
    .replace(/\s+/g, ' ')
    .replace(/\.$/, '')
    .toLowerCase();
}

function isSignature(body) {
  return normalize(body) === normalize(PHRASE);
}

// Co-authors that are AI tools. A tool holds no copyright, so it has nothing to
// license (agreement section 7). Tools that co-author from a GitHub bot
// account, such as Copilot, are exempt by account type instead.
const AI_COAUTHOR_EMAILS = new Set(['noreply@anthropic.com', 'cursoragent@cursor.com']);

const COAUTHOR_TRAILER = /^co-authored-by:\s*(.*?)\s*<([^>]+)>\s*$/gim;
const NOREPLY = /^(?:\d+\+)?([^@+]+)@users\.noreply\.github\.com$/i;

function coAuthorsOf(message) {
  return [...(message || '').matchAll(COAUTHOR_TRAILER)].map(([, name, email]) => ({
    name: name || email,
    email: email.toLowerCase(),
  }));
}

// The GitHub account behind a co-author's email, or null. A noreply address
// names its account; any other address is matched against public profile
// emails, and only an unambiguous match counts.
async function accountFor(github, email) {
  const noreply = email.match(NOREPLY);
  if (noreply) {
    try {
      return (await github.rest.users.getByUsername({ username: noreply[1] })).data;
    } catch (e) {
      if (e.status === 404) return null;
      throw e;
    }
  }
  const { data } = await github.rest.search.users({ q: `${email} in:email` });
  return data.total_count === 1 ? data.items[0] : null;
}

// The people who must have signed: the pull request's author, every commit
// author, and every human named in a Co-authored-by trailer. Anyone whose email
// is not linked to a GitHub account cannot be matched to a signature, so they
// are reported by name instead.
async function authorsOf(github, pr, commits) {
  const people = new Map();
  const unlinked = new Set();
  const add = (user) => {
    if (user.type === 'Bot' || EXEMPT_LOGINS.has(user.login)) return;
    people.set(user.id, user.login);
  };
  add(pr.user);
  const coAuthors = new Map();
  for (const c of commits) {
    if (c.author) add(c.author);
    else unlinked.add(c.commit.author.name);
    for (const co of coAuthorsOf(c.commit.message)) {
      if (!AI_COAUTHOR_EMAILS.has(co.email)) coAuthors.set(co.email, co.name);
    }
  }
  for (const [email, name] of coAuthors) {
    const account = await accountFor(github, email);
    if (account) add(account);
    else unlinked.add(name);
  }
  return { people, unlinked };
}

async function readSignatures(github, owner, repo) {
  try {
    const { data } = await github.rest.repos.getContent({
      owner, repo, path: SIGNATURE_FILE, ref: SIGNATURE_BRANCH,
    });
    return { sha: data.sha, entries: JSON.parse(Buffer.from(data.content, 'base64').toString('utf8')) };
  } catch (e) {
    if (e.status === 404) return { sha: undefined, entries: [] };
    throw e;
  }
}

// The signature branch holds nothing but signatures, so it starts as an orphan
// commit rather than a copy of main.
async function ensureBranch(github, owner, repo) {
  try {
    await github.rest.git.getRef({ owner, repo, ref: `heads/${SIGNATURE_BRANCH}` });
    return;
  } catch (e) {
    if (e.status !== 404) throw e;
  }
  const readme = [
    '# Contributor agreement signatures',
    '',
    `Written by the ${STATUS_CONTEXT} check. One file per version of ${DOCUMENT};`,
    'each entry links the pull request comment that is the signature.',
    '',
  ].join('\n');
  const { data: tree } = await github.rest.git.createTree({
    owner, repo, tree: [{ path: 'README.md', mode: '100644', type: 'blob', content: readme }],
  });
  const { data: commit } = await github.rest.git.createCommit({
    owner, repo, message: 'Start the contributor agreement signature record', tree: tree.sha, parents: [],
  });
  await github.rest.git.createRef({ owner, repo, ref: `refs/heads/${SIGNATURE_BRANCH}`, sha: commit.sha });
}

// Appends entries, re-reading on a conflict: two pull requests signed at once
// both write this file.
async function recordSignatures(github, owner, repo, fresh) {
  await ensureBranch(github, owner, repo);
  for (let attempt = 0; attempt < 5; attempt++) {
    const { sha, entries } = await readSignatures(github, owner, repo);
    const known = new Set(entries.map((e) => e.id));
    const added = fresh.filter((e) => !known.has(e.id));
    if (added.length === 0) return;
    const next = [...entries, ...added].sort((a, b) => a.id - b.id);
    try {
      await github.rest.repos.createOrUpdateFileContents({
        owner, repo, branch: SIGNATURE_BRANCH, path: SIGNATURE_FILE, sha,
        message: `Record contributor agreement v${VERSION}: ${added.map((e) => e.login).join(', ')}`,
        content: Buffer.from(`${JSON.stringify(next, null, 2)}\n`).toString('base64'),
      });
      return;
    } catch (e) {
      if (e.status !== 409 && e.status !== 422) throw e;
    }
  }
  throw new Error(`Could not write ${SIGNATURE_FILE} after five attempts; another run kept changing it.`);
}

function unsignedComment(docUrl, missing, unlinked) {
  const lines = [MARKER, ''];
  if (missing.length) {
    lines.push(
      `Before this can be merged, ${missing.map((l) => `@${l}`).join(', ')} need${missing.length === 1 ? 's' : ''} ` +
        `to sign the [Contributor License Agreement](${docUrl}). You keep the copyright in your ` +
        'contribution and give the maintainer a license to it under any terms, so that Pando can be offered ' +
        'under the AGPL and a commercial license. You sign once, and it covers every later contribution.',
      '',
      'To sign, post this comment exactly:',
      '',
      '```',
      PHRASE,
      '```',
      '',
    );
  }
  if (unlinked.size) {
    lines.push(
      `${[...unlinked].join(', ')} authored or co-authored commits here under an email address that is ` +
        'not linked to a GitHub account, so they cannot be matched to a signature. Add the address to ' +
        'the account under GitHub Settings → Emails, or amend the commits or `Co-authored-by` lines to ' +
        'use an address that is, such as the account\'s `users.noreply.github.com` address. Then comment `recheck`.',
      '',
    );
  }
  return lines.join('\n');
}

module.exports = async function run({ github, context, core }) {
  const { owner, repo } = context.repo;

  if (context.eventName === 'issue_comment') {
    if (!context.payload.issue.pull_request) return;
    const body = context.payload.comment.body;
    if (!isSignature(body) && normalize(body) !== 'recheck') return;
  }
  const number = context.payload.pull_request?.number ?? context.payload.issue.number;
  const { data: pr } = await github.rest.pulls.get({ owner, repo, pull_number: number });
  if (pr.state !== 'open') return;

  const docUrl = `https://github.com/${owner}/${repo}/blob/${pr.base.ref}/${DOCUMENT}`;
  const commits = await github.paginate(github.rest.pulls.listCommits, {
    owner, repo, pull_number: number, per_page: 100,
  });
  const { people, unlinked } = await authorsOf(github, pr, commits);

  const { entries } = await readSignatures(github, owner, repo);
  const signed = new Set(entries.map((e) => e.id));
  let missing = [...people].filter(([id]) => !signed.has(id));

  if (missing.length) {
    const comments = await github.paginate(github.rest.issues.listComments, {
      owner, repo, issue_number: number, per_page: 100,
    });
    const { data: doc } = await github.rest.repos.getContent({ owner, repo, path: DOCUMENT, ref: pr.base.sha });
    const fresh = [];
    for (const [id, login] of missing) {
      const c = comments.find((x) => x.user.id === id && isSignature(x.body));
      if (!c) continue;
      fresh.push({
        login,
        id,
        signed_at: c.created_at,
        comment: c.html_url,
        pull_request: number,
        agreement_version: VERSION,
        agreement_blob: doc.sha,
      });
    }
    if (fresh.length) {
      await recordSignatures(github, owner, repo, fresh);
      const now = new Set(fresh.map((e) => e.id));
      missing = missing.filter(([id]) => !now.has(id));
      core.info(`Recorded signatures for ${fresh.map((e) => e.login).join(', ')}.`);
    }
  }

  const missingLogins = missing.map(([, login]) => login);
  const ok = missingLogins.length === 0 && unlinked.size === 0;
  const waitingOn = [...missingLogins, ...unlinked].join(', ');
  await github.rest.repos.createCommitStatus({
    owner, repo, sha: pr.head.sha, context: STATUS_CONTEXT, target_url: docUrl,
    state: ok ? 'success' : 'failure',
    description: (ok ? 'Every author has signed.' : `Waiting on ${waitingOn}.`).slice(0, 140),
  });

  // One comment per pull request, edited in place, and none at all when
  // nobody ever needed to sign.
  const all = await github.paginate(github.rest.issues.listComments, {
    owner, repo, issue_number: number, per_page: 100,
  });
  const existing = all.find((c) => c.user.type === 'Bot' && c.body.startsWith(MARKER));
  if (ok) {
    if (existing) {
      await github.rest.issues.updateComment({
        owner, repo, comment_id: existing.id,
        body: `${MARKER}\n\nEvery author has signed the [Contributor License Agreement](${docUrl}). Thank you.\n`,
      });
    }
    return;
  }
  const body = unsignedComment(docUrl, missingLogins, unlinked);
  if (existing) await github.rest.issues.updateComment({ owner, repo, comment_id: existing.id, body });
  else await github.rest.issues.createComment({ owner, repo, issue_number: number, body });
};

module.exports.normalize = normalize;
module.exports.isSignature = isSignature;
module.exports.authorsOf = authorsOf;
module.exports.coAuthorsOf = coAuthorsOf;
module.exports.PHRASE = PHRASE;
