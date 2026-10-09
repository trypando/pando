// The working line on the onboarding page while detection runs.
//
// A present-tense phrase per step, rotating while the step lasts, saying what
// Pando is looking at. Plain words, no jokes and no exclamation marks — the
// voice rules hold for a line that changes every second and a half as much as
// for an error. The page animates the trailing dots, so no phrase carries its
// own.
//
// The first phrase of each step says literally what Pando is doing, because
// that is the one shown under prefers-reduced-motion, where the line does not
// rotate.

const PHRASES: Record<string, string[]> = {
  read: ['Fetching the repository', 'Checking out the commit', 'Listing the files'],
  stack: ['Reading the code', 'Looking for a Dockerfile or a compose file', 'Weighing how it could be built'],
  runs: ['Finding what it runs', 'Tracing its services'],
  vars: ['Collecting variables', 'Matching variables to services'],
  trial: ['Starting it to watch', 'Watching which ports it opens', 'Checking what it writes'],
  scan: ['Scanning for known vulnerabilities', 'Reading the lockfiles', 'Matching packages against known CVEs'],
  repair: ['Reading the trial run’s output', 'Looking for what went wrong', 'Checking the start command'],
  answer: ['Reading the questions', 'Looking for the answers in the repo', 'Leaving what it can’t settle for you'],
  // A person asked AI to change the plan.
  ask: [
    'Reading what you said',
    'Opening the files it needs',
    'Checking that against the repo',
    'Weighing the change',
    'Writing a reply',
  ],
};

/**
 * The phrases for a step: its id from discovery.ts, with the AI step split by
 * what it was asked to do. An id this console does not know reads as the first.
 */
export function phrasesFor(step: string | undefined): string[] {
  return PHRASES[step ?? ''] ?? PHRASES.read!;
}

/** How often the phrase changes. */
export const PHRASE_MS = 1_500;
