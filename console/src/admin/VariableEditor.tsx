// The variable editor, shared by an app's Settings and the plan screen.
//
// One card per workload, because the spec keeps variables per workload
// (Workload.Env): a variable added in a card belongs to that card's workload.
// Each row is the name, the value with a lock inside its right edge — closed
// is a secret, stored through the secrets adapter (R-190), open is a plain
// value in the spec — Generate, and remove. A value something else supplies (a
// dependency, R-131) is a read-only box saying where it comes from. An empty
// value is outlined in the warning color: a gap to fill, never an error.
//
// The editor holds no state of its own beyond the generated-value wash. What
// the rows are, what changing one does and which verbs allow it are the
// caller's, because Settings writes a revision and the plan screen sends its
// values with the accept.

import { useEffect, useState } from 'react';
import { Button, Card, Icon, Input, Tooltip } from '@design';

import { generateFor } from './onboarding';
import './onboarding.css';
import './settings.css';

/** One row as the editor draws it. */
export interface EditorRow {
  /** Stable across edits, for React. */
  id: string;
  key: string;
  value: string;
  secret: boolean;
  /** The name is typed here: a variable added in this editor. */
  newName?: boolean;
  /** Why the typed name can't be used. */
  nameError?: string;
  /** Beside the name: the Required tag on the plan screen. */
  nameExtra?: React.ReactNode;
  /** The line under the name: where the value came from. */
  source?: React.ReactNode;
  /** Supplied by something else; the value box is replaced by this. */
  from?: React.ReactNode;
  /** A secret already stored: shown as dots, and typing replaces it. */
  stored?: boolean;
  /** Shown, not changeable: a secret row without app.secrets.write. */
  readOnly?: boolean;
  /** Whether the lock can be turned: app.secrets.write. */
  lockable: boolean;
  removable: boolean;
  placeholder?: string;
  /** The shape Generate fills (spec.SecretFormat). */
  format?: string;
}

export interface EditorGroup {
  workload: string;
  rows: EditorRow[];
}

export interface RowPatch {
  key?: string;
  value?: string;
  secret?: boolean;
}

/** What the dots stand for when a stored secret's value is not shown. */
const STORED = '••••••••••••';

export function VariableEditor({
  groups,
  canEdit,
  onChange,
  onRemove,
  onAdd,
  onGenerated,
}: Readonly<{
  groups: EditorGroup[];
  /** Whether anything here may change at all. */
  canEdit: boolean;
  onChange: (id: string, patch: RowPatch) => void;
  onRemove: (id: string) => void;
  /** Adds a row to this workload. */
  onAdd: (workload: string) => void;
  /** Said once a row's value was generated, with that value. */
  onGenerated?: (id: string, value: string) => void;
}>) {
  // How many times each row has been generated, to replay its wash; and the
  // row whose button shows a check for a moment.
  const [flash, setFlash] = useState<Record<string, number>>({});
  const [justGenerated, setJustGenerated] = useState<string | null>(null);
  useEffect(() => {
    if (!justGenerated) return undefined;
    const timer = window.setTimeout(() => setJustGenerated(null), 1_600);
    return () => window.clearTimeout(timer);
  }, [justGenerated, flash]);

  const generate = (row: EditorRow) => {
    const value = generateFor(row.format);
    setFlash((all) => ({ ...all, [row.id]: (all[row.id] ?? 0) + 1 }));
    setJustGenerated(row.id);
    // A generated value is a secret by nature; kept one unless the person
    // opens the lock afterwards.
    onChange(row.id, { value, secret: row.lockable ? true : row.secret });
    onGenerated?.(row.id, value);
  };

  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
      {groups.map((group) => (
        <Card key={group.workload} tone="paper" padding="md">
          <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
            <h4 style={{ font: 'var(--type-code)', color: 'var(--ink)', margin: 0 }}>{group.workload}</h4>
            {group.rows.length === 0 && !canEdit && (
              <span style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)' }}>No variables.</span>
            )}
            {group.rows.map((row) => (
              <VariableRowLine
                key={row.id}
                row={row}
                canEdit={canEdit}
                flash={flash[row.id] ?? 0}
                justGenerated={justGenerated === row.id}
                onChange={(patch) => onChange(row.id, patch)}
                onRemove={() => onRemove(row.id)}
                onGenerate={() => generate(row)}
              />
            ))}
            {canEdit && (
              <div>
                <Button variant="ghost" icon={<Icon name="plus" size={16} />} onClick={() => onAdd(group.workload)}>
                  Add variable
                </Button>
              </div>
            )}
          </div>
        </Card>
      ))}
    </div>
  );
}

function VariableRowLine({
  row,
  canEdit,
  flash,
  justGenerated,
  onChange,
  onRemove,
  onGenerate,
}: Readonly<{
  row: EditorRow;
  canEdit: boolean;
  flash: number;
  justGenerated: boolean;
  onChange: (patch: RowPatch) => void;
  onRemove: () => void;
  onGenerate: () => void;
}>) {
  const label = row.key || 'the new variable';
  const editable = canEdit && !row.readOnly;
  return (
    <div className="pando-vrow" data-row={row.id}>
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-1)', minWidth: 0 }}>
        {row.newName && canEdit ? (
          <Input
            aria-label="Name"
            placeholder="NAME"
            mono
            value={row.key}
            error={row.nameError}
            onChange={(e) => onChange({ key: e.target.value })}
          />
        ) : (
          <span className="pando-vname-key">
            {row.key}
            {row.nameExtra}
          </span>
        )}
        {row.newName && canEdit && row.nameExtra}
        {row.source && (
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
            {row.source}
          </span>
        )}
      </div>

      {row.from ? (
        <span className="pando-vfrom">
          <Icon name="link" size={16} />
          {row.from}
        </span>
      ) : (
        <>
          <div
            // Re-keyed on each generation, so the wash replays every time:
            // the value visibly changed, even when it is hidden as a secret.
            key={`${row.id}-${flash}`}
            className={flash ? 'pando-generated' : undefined}
            style={{ minWidth: 0, borderRadius: 'var(--radius-sm)' }}
          >
            <ValueField row={row} label={label} editable={editable} onChange={onChange} />
          </div>
          {editable ? (
            <GenerateButton justDone={justGenerated} onGenerate={onGenerate} />
          ) : (
            <span />
          )}
          {editable && row.removable ? (
            <Tooltip content={`Remove ${label}`}>
              <Button variant="ghost" aria-label={`Remove ${label}`} icon={<Icon name="x" size={16} />} onClick={onRemove} />
            </Tooltip>
          ) : (
            <span />
          )}
        </>
      )}
    </div>
  );
}

/** The value, with the lock inside its right edge. */
function ValueField({
  row,
  label,
  editable,
  onChange,
}: Readonly<{ row: EditorRow; label: string; editable: boolean; onChange: (patch: RowPatch) => void }>) {
  const empty = row.value === '' && !row.stored;
  const lockLabel = row.secret ? 'Secret. Click to store as a plain value' : 'Plain value. Click to store as a secret';
  const lockFixed = row.secret ? 'Secret' : 'Plain value';
  const canLock = editable && row.lockable;
  return (
    <div className={`pando-vfield${empty ? ' pando-vfield--empty' : ''}`}>
      <input
        aria-label={`Value of ${label}`}
        type={row.secret ? 'password' : 'text'}
        autoComplete="off"
        spellCheck={false}
        placeholder={row.stored && row.value === '' ? STORED : (row.placeholder ?? 'No value')}
        value={row.value}
        disabled={!editable}
        onChange={(e) => onChange({ value: e.target.value })}
      />
      <button
        type="button"
        className="pando-vlock"
        aria-pressed={row.secret}
        aria-label={`Secret: ${label}`}
        title={canLock ? lockLabel : lockFixed}
        disabled={!canLock}
        onClick={() => onChange({ secret: !row.secret })}
      >
        <Icon name={row.secret ? 'lock' : 'lock-open'} size={16} />
      </button>
    </div>
  );
}

/**
 * Fill a value with a random one: for a variable that just needs a secret
 * nobody will type — an encryption key, a session secret. The tooltip says
 * what it is, so nobody wonders whether Pando looked something up.
 */
function GenerateButton({ onGenerate, justDone }: Readonly<{ onGenerate: () => void; justDone: boolean }>) {
  return (
    <Tooltip side="top" content="Fills in a random, secure string.">
      {/* The label never changes, only the icon (the same size), so the
          button keeps its width and the value beside it does not jump. */}
      <Button
        variant="ghost"
        icon={<Icon name={justDone ? 'check' : 'dices'} size={16} />}
        onClick={onGenerate}
        aria-label={justDone ? 'Generated a random value. Generate another.' : 'Generate a random value'}
      >
        Generate
      </Button>
    </Tooltip>
  );
}
