// Renaming an app: PATCH /apps/{id}, app.spec.edit.
//
// The app's heading, with Rename beside it, edited in place. Only the display
// name changes — the ID and the address stay — so there is nothing to confirm
// and nothing to warn about, and a dialog would be ceremony.

import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { invalidateAppLists } from './appList';
import { Button, Icon, Input } from '@design';

import { api } from '@api/client';
import type { App } from '@api/types.gen';
import { AppVerb, keepVerbs, useCan } from './verbs';

export function AppName({ app }: { app: App }) {
  const queries = useQueryClient();
  const [draft, setDraft] = useState<string | null>(null);
  const canRename = useCan(AppVerb.SpecEdit);

  const rename = useMutation({
    mutationFn: (name: string) => api.patch<App>(`/apps/${app.id}`, { name }),
    onSuccess: (updated) => {
      queries.setQueryData(['apps', app.id], keepVerbs(updated));
      void invalidateAppLists(queries);
      void queries.invalidateQueries({ queryKey: ['me', 'apps'] });
      setDraft(null);
    },
  });

  // Without app.spec.edit the name is a heading to read, not a control that
  // is refused when used.
  if (draft === null || !canRename) {
    return (
      <>
        <h3 style={{ font: 'var(--type-h3)', margin: 0 }}>{app.name}</h3>
        {canRename && (
          <Button variant="ghost" icon={<Icon name="pencil" />} onClick={() => setDraft(app.name)}>
            Rename
          </Button>
        )}
      </>
    );
  }

  const name = draft.trim();
  const save = () => {
    if (name === '' || name === app.name) {
      setDraft(null);
      return;
    }
    rename.mutate(name);
  };

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        save();
      }}
      style={{ display: 'flex', alignItems: 'flex-start', gap: 'var(--space-2)', flexWrap: 'wrap' }}
    >
      <Input
        aria-label="App name"
        value={draft}
        autoFocus
        onChange={(e) => setDraft(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === 'Escape') setDraft(null);
        }}
        // The server's message, verbatim (R-105) — a taken name says so.
        error={rename.error?.message}
      />
      <Button variant="secondary" type="submit" disabled={rename.isPending || name === ''}>
        {rename.isPending ? 'Saving' : 'Save'}
      </Button>
      <Button variant="ghost" type="button" onClick={() => setDraft(null)}>
        Cancel
      </Button>
    </form>
  );
}
