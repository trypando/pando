// One row of a <dl> laid out as a two-column grid: a label and what it says.
// For facts stated rather than asked for.

export function Term({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <>
      <dt style={{ font: 'var(--type-label)', color: 'var(--ink)' }}>{label}</dt>
      <dd style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>{children}</dd>
    </>
  );
}
