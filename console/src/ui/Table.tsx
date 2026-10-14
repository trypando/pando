// The design system's Table, with two things the console needs on top.
//
// A class, so that at phone width a table can scroll sideways inside itself
// rather than crushing its columns (see narrow.css).
//
// Column filters. A column that sets `filter` gets a button in its header that
// opens a filter for that column alone: "contains" for a column of free text,
// a checklist of the values present for a column of a few kinds of value.
// Filters on several columns combine. They filter the rows the page already
// has — the same reasoning as ui/search.ts: one installation's lists are small,
// and the API returns them whole (R-261).

import { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { Button, Checkbox, Icon, Input, Skeleton, Table as DesignTable } from '@design';
import type { TableColumn, TableProps as DesignTableProps } from '@design';

export interface Column extends TableColumn {
  /** `text` filters by "contains"; `values` offers the values present. */
  filter?: 'text' | 'values';
  /**
   * What the filter reads from a row. Defaults to the column's own field.
   * Several values for a row that has several — an account's roles — each
   * offered on its own, and the row shown when any of them is chosen.
   */
  filterValue?: (row: any) => string | string[];
}

export interface TableProps extends Omit<DesignTableProps, 'columns'> {
  columns?: Column[];
  /** The rows are still on their way: the headers, and `skeletonRows` rows of
   *  placeholders in the real columns, instead of an empty state that would
   *  say there is nothing. */
  loading?: boolean;
  skeletonRows?: number;
}

export interface ColumnFilter {
  text?: string;
  values?: string[];
}

/** What a column's filter reads from a row: one value, or each of several. */
export function valuesOf(c: Column, row: any): string[] {
  const v = c.filterValue ? c.filterValue(row) : String(row?.[c.key] ?? '');
  return Array.isArray(v) ? v : [v];
}

const active = (f?: ColumnFilter) => Boolean(f && ((f.text ?? '').trim() || (f.values?.length ?? 0) > 0));

/** The rows every active column filter keeps; a row with several values is kept when any matches. */
export function filterRows(columns: Column[], rows: any[], filters: Record<string, ColumnFilter>): any[] {
  return rows.filter((row) =>
    columns.every((c) => {
      const f = filters[c.key];
      if (!c.filter || !f || !active(f)) return true;
      const vs = valuesOf(c, row);
      if (c.filter === 'text') {
        const text = (f.text ?? '').trim().toLowerCase();
        return vs.some((v) => v.toLowerCase().includes(text));
      }
      return vs.some((v) => (f.values ?? []).includes(v));
    }),
  );
}

export function Table({ className, columns = [], rows = [], empty, loading = false, skeletonRows = 4, ...props }: TableProps) {
  const [filters, setFilters] = useState<Record<string, ColumnFilter>>({});

  const shown = filterRows(columns, rows, filters);

  const anyActive = columns.some((c) => c.filter && active(filters[c.key]));

  const headed = columns.map((c) =>
    c.filter
      ? {
          ...c,
          // The design Table renders a header as given; a node works as well
          // as a string, which is what lets the filter sit in the header.
          header: (
            <ColumnHeader
              label={c.header}
              kind={c.filter}
              active={active(filters[c.key])}
              filter={filters[c.key] ?? {}}
              values={c.filter === 'values' ? tally(rows.flatMap((r) => valuesOf(c, r))) : []}
              onChange={(next) => setFilters((all) => ({ ...all, [c.key]: next }))}
            />
          ) as unknown as string,
        }
      : c,
  );

  // Each cell a line of placeholder, a little shorter row to row so the
  // block reads as rows of text rather than a grey slab. Only while there is
  // nothing to show yet: a refetch keeps the rows it has.
  if (loading && rows.length === 0) {
    return (
      <DesignTable
        className={className ? `pando-table ${className}` : 'pando-table'}
        role="status"
        aria-label="Loading"
        columns={columns.map((c) => ({
          ...c,
          render: (row: { n: number }) => <Skeleton width={c.align === 'right' ? '40%' : `${80 - (row.n % 3) * 15}%`} />,
        }))}
        rows={Array.from({ length: skeletonRows }, (_, n) => ({ id: `skeleton-${n}`, n }))}
        {...props}
      />
    );
  }

  return (
    <DesignTable
      className={className ? `pando-table ${className}` : 'pando-table'}
      columns={headed}
      rows={shown}
      empty={
        anyActive && rows.length > 0 ? (
          <div
            style={{
              display: 'flex',
              alignItems: 'center',
              gap: 'var(--space-3)',
              padding: 'var(--space-3)',
              font: 'var(--type-body-ui)',
              color: 'var(--ink-secondary)',
            }}
          >
            No rows match the column filters.
            <Button variant="ghost" onClick={() => setFilters({})}>
              Clear column filters
            </Button>
          </div>
        ) : (
          empty
        )
      }
      {...props}
    />
  );
}

/** Each distinct value and how many rows have it, most common first. */
function tally(values: string[]): Array<{ value: string; count: number }> {
  const counts = new Map<string, number>();
  for (const v of values) counts.set(v, (counts.get(v) ?? 0) + 1);
  return [...counts.entries()]
    .map(([value, count]) => ({ value, count }))
    .sort((a, b) => b.count - a.count || a.value.localeCompare(b.value));
}

function ColumnHeader({
  label,
  kind,
  active,
  filter,
  values,
  onChange,
}: {
  label: string;
  kind: 'text' | 'values';
  active: boolean;
  filter: ColumnFilter;
  values: Array<{ value: string; count: number }>;
  onChange: (next: ColumnFilter) => void;
}) {
  const [open, setOpen] = useState(false);
  const button = useRef<HTMLButtonElement>(null);
  const panel = useRef<HTMLDivElement>(null);
  const [at, setAt] = useState<{ top: number; left: number } | null>(null);

  // Placed against the window rather than the table: at phone width the table
  // scrolls sideways inside itself, and a popover inside it would be clipped.
  useLayoutEffect(() => {
    if (!open || !button.current) return;
    const r = button.current.getBoundingClientRect();
    const width = panel.current?.offsetWidth ?? 0;
    setAt({ top: r.bottom + 4, left: Math.max(8, Math.min(r.left, window.innerWidth - width - 8)) });
  }, [open]);

  // Focus once placed: the panel is hidden while it is measured, and a hidden
  // field cannot take focus, so autoFocus alone lands nowhere.
  useEffect(() => {
    if (open && at) panel.current?.querySelector<HTMLElement>('input')?.focus();
  }, [open, at]);

  useEffect(() => {
    if (!open) return undefined;
    const outside = (e: MouseEvent) => {
      const t = e.target as Node;
      if (!panel.current?.contains(t) && !button.current?.contains(t)) setOpen(false);
    };
    const key = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        setOpen(false);
        button.current?.focus();
      }
    };
    document.addEventListener('mousedown', outside);
    document.addEventListener('keydown', key);
    return () => {
      document.removeEventListener('mousedown', outside);
      document.removeEventListener('keydown', key);
    };
  }, [open]);

  const chosen = new Set(filter.values ?? []);

  return (
    <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-1)' }}>
      {label}
      <button
        ref={button}
        type="button"
        aria-label={`Filter ${label}`}
        aria-expanded={open}
        aria-pressed={active}
        onClick={() => setOpen((o) => !o)}
        style={{
          display: 'inline-flex',
          border: 'none',
          padding: 'var(--space-1)',
          borderRadius: 'var(--radius-xs)',
          cursor: 'pointer',
          background: active ? 'var(--paper-raised)' : 'transparent',
        }}
      >
        <Icon name="list-filter" size={14} color={active ? 'var(--ink)' : 'var(--ink-muted)'} />
      </button>

      {open && (
        <div
          ref={panel}
          role="dialog"
          aria-label={`Filter ${label}`}
          style={{
            position: 'fixed',
            top: at?.top ?? 0,
            left: at?.left ?? 0,
            visibility: at ? 'visible' : 'hidden',
            zIndex: 30,
            width: '26ch',
            maxHeight: '50vh',
            overflowY: 'auto',
            padding: 'var(--space-3)',
            display: 'flex',
            flexDirection: 'column',
            gap: 'var(--space-2)',
            background: 'var(--paper-raised)',
            border: 'var(--border-width) solid var(--rule-strong)',
            borderRadius: 'var(--radius-md)',
            boxShadow: 'var(--shadow-popover)',
            font: 'var(--type-body-ui)',
            color: 'var(--ink)',
            textTransform: 'none',
          }}
        >
          {kind === 'text' ? (
            <Input
              aria-label={`${label} contains`}
              placeholder="Contains"
              value={filter.text ?? ''}
              onChange={(e) => onChange({ text: e.target.value })}
            />
          ) : (
            values.map(({ value, count }) => (
              <Checkbox
                key={value}
                label={`${value || '—'} (${count})`}
                checked={chosen.has(value)}
                onChange={(e) => {
                  const next = new Set(chosen);
                  if (e.target.checked) next.add(value);
                  else next.delete(value);
                  onChange({ values: [...next] });
                }}
              />
            ))
          )}
          {active && (
            <Button variant="ghost" onClick={() => onChange({})} style={{ alignSelf: 'flex-start' }}>
              Clear
            </Button>
          )}
        </div>
      )}
    </span>
  );
}
