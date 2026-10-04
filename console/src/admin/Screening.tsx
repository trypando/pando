// The screening section of detection review (R-331, R-334; design 10 §5).
//
// An AI adapter amended the plan the person is about to accept, so what it
// changed is shown beside everything else Pando worked out: each change with
// its reason and the files it rests on, which files left the host (R-337), and
// what it asked for that Pando refused. The refusals are shown rather than
// dropped — a change that disappears without a word is one nobody can check.
//
// Reasons and summaries are rendered as the server wrote them. Like question
// text (design 08 §1.3), they are held to R-105 and paraphrasing them here
// would undo that.

import { Card, Icon, InlineCode, Tag } from '@design';

import type { Amendment, Outcome } from '@api/types.gen';
import { Disclosure } from '../ui/Disclosure';
import { describeAmendment, filesRead, screeningVisible } from './screeningText';

export function Screening({ outcome }: { outcome: Outcome | undefined }) {
  if (!screeningVisible(outcome)) return null;

  if (!outcome.ran) {
    return (
      <p style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)', margin: 0 }}>
        This plan was not checked by an AI adapter. {outcome.skipped}
      </p>
    );
  }

  const applied = outcome.applied ?? [];
  const refused = outcome.refused ?? [];
  const notes = outcome.notes ?? [];
  const files = outcome.files_read ?? [];

  return (
    <Card padding="md">
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
          <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-3)' }}>
            <h4 style={{ font: 'var(--type-h4)', margin: 0 }}>Checked by an AI adapter</h4>
            {outcome.model && <Tag mono>{outcome.model}</Tag>}
          </div>
          <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>
            {applied.length === 0
              ? 'It read the repository and found nothing in this plan to change.'
              : applied.length === 1
                ? 'It read the repository and changed one thing in this plan. Review it before accepting.'
                : `It read the repository and changed ${applied.length} things in this plan. Review them before accepting.`}
          </p>
        </div>

        {applied.length > 0 && (
          <ul style={listStyle}>
            {applied.map((change, index) => (
              <Change
                key={`${change.amendment.kind}-${index}`}
                icon="circle-check"
                title={sentence(change.summary)}
                amendment={change.amendment}
              />
            ))}
          </ul>
        )}

        {notes.length > 0 && (
          <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
            {notes.map((note) => (
              <p key={note} style={{ font: 'var(--type-body-ui)', color: 'var(--ink)', margin: 0 }}>
                {note}
              </p>
            ))}
          </div>
        )}

        <Disclosure
          show={`${filesRead(files.length)} Show which`}
          hide={`${filesRead(files.length)} Hide the list`}
          hidden={files.length === 0}
        >
          <div style={{ display: 'flex', flexWrap: 'wrap', gap: 'var(--space-2)' }}>
            {files.map((file) => (
              <InlineCode key={file}>{file}</InlineCode>
            ))}
          </div>
        </Disclosure>

        <Disclosure
          show={`Show what Pando refused (${refused.length})`}
          hide={`Hide what Pando refused (${refused.length})`}
          hidden={refused.length === 0}
        >
          <ul style={listStyle}>
            {refused.map((refusal, index) => (
              <Change
                key={`${refusal.amendment.kind}-${index}`}
                icon="circle-slash"
                title={describeAmendment(refusal.amendment)}
                amendment={refusal.amendment}
                refusal={refusal.reason}
              />
            ))}
          </ul>
        </Disclosure>
      </div>
    </Card>
  );
}

function Change({
  icon,
  title,
  amendment,
  refusal,
}: {
  icon: string;
  title: string;
  amendment: Amendment;
  refusal?: string;
}) {
  const evidence = (amendment.evidence ?? []).filter(Boolean);
  return (
    <li style={{ display: 'flex', gap: 'var(--space-3)', alignItems: 'flex-start' }}>
      <Icon name={icon} size={16} />
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)', flex: 1 }}>
        <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink)' }}>{title}</span>
        {refusal && (
          <span style={{ font: 'var(--type-body-ui)', color: 'var(--marker-deep)' }}>
            Refused: {refusal}
          </span>
        )}
        {amendment.reason && (
          <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
            {amendment.reason}
          </span>
        )}
        {evidence.length > 0 && (
          <span
            style={{
              display: 'flex',
              flexWrap: 'wrap',
              alignItems: 'center',
              gap: 'var(--space-2)',
              font: 'var(--type-caption)',
              color: 'var(--ink-secondary)',
            }}
          >
            Based on
            {evidence.map((file) => (
              <InlineCode key={file}>{file}</InlineCode>
            ))}
          </span>
        )}
      </div>
    </li>
  );
}

// The server writes summaries in lower case ("port set to 3000") because they
// were written to be embedded. Standing alone in a list they start a sentence.
function sentence(text: string): string {
  return text ? text.charAt(0).toUpperCase() + text.slice(1) : text;
}

const listStyle: React.CSSProperties = {
  display: 'flex',
  flexDirection: 'column',
  gap: 'var(--space-3)',
  margin: 0,
  padding: 0,
  listStyle: 'none',
};
