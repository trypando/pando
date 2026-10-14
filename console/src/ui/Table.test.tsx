import { describe, expect, it } from 'vitest';

import { filterRows, valuesOf } from './Table';
import type { Column } from './Table';

const rows = [
  { id: 'a', name: 'Dana', roles: ['Administrator', 'Audit readers'] },
  { id: 'b', name: 'Eve', roles: ['Audit readers'] },
  { id: 'c', name: 'Sam', roles: ['None'] },
];
const name: Column = { key: 'name', header: 'Name', filter: 'text' };
const role: Column = { key: 'roles', header: 'Role', filter: 'values', filterValue: (r) => r.roles };

describe('valuesOf', () => {
  it('reads the column field, or each of the values filterValue gives', () => {
    expect(valuesOf(name, rows[0])).toEqual(['Dana']);
    expect(valuesOf(role, rows[0])).toEqual(['Administrator', 'Audit readers']);
    expect(valuesOf({ key: 'missing', header: 'X' }, rows[0])).toEqual(['']);
  });
});

// Issue #221: an account holding two roles is found by either one.
describe('filterRows', () => {
  it('keeps a row with several values when any of them is chosen', () => {
    const ids = (f: Parameters<typeof filterRows>[2]) => filterRows([name, role], rows, f).map((r) => r.id);
    expect(ids({ roles: { values: ['Audit readers'] } })).toEqual(['a', 'b']);
    expect(ids({ roles: { values: ['Administrator'] } })).toEqual(['a']);
    expect(ids({ roles: { values: ['None'] } })).toEqual(['c']);
  });

  it('matches text case-insensitively, and an empty filter keeps everything', () => {
    const ids = (f: Parameters<typeof filterRows>[2]) => filterRows([name, role], rows, f).map((r) => r.id);
    expect(ids({ name: { text: ' EV ' } })).toEqual(['b']);
    expect(ids({ name: { text: '  ' }, roles: { values: [] } })).toEqual(['a', 'b', 'c']);
    expect(ids({})).toEqual(['a', 'b', 'c']);
  });
});
