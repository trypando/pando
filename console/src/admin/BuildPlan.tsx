// The build plan, before it runs and afterwards.
//
// An app with no Dockerfile is built from a plan Pando generates. It used to be
// generated at build time, used, and thrown away — so the answer to "how is
// this being built" was "read the adapter's source". R-102 is ask-never-guess,
// and showing the decision is the same principle applied one step later.
//
// Editable because R-020 says the state store is the sole record of how an app
// runs, and "commit a Dockerfile to your repo to take control" is the opposite
// of that: it puts deployment files in the source tree, which is the thing
// Pando exists to avoid. An edit here is a spec revision like any other
// (R-152), so it can be diffed and rolled back. Which is app.spec.edit:
// without it the plan is shown and there is no Edit.

import { useState } from 'react';
import { useMutation, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Input, Select } from '@design';

import { api } from '@api/client';
import { Quiet, messageOf } from '../install/Accounts';
import { useNewestSpec } from './newestSpec';
import { AppVerb, useCan } from './verbs';

export function BuildPlan({ appID }: { appID: string }) {
  const queries = useQueryClient();
  const [draft, setDraft] = useState<string | null>(null);
  const canEdit = useCan(AppVerb.SpecEdit);

  const { spec } = useNewestSpec(appID);
  const files = spec?.build?.generated_files ?? {};
  const dockerfile = spec?.build?.dockerfile || '.nixpacks/Dockerfile';

  // Every file of the plan, not only the Dockerfile. A generated Dockerfile
  // copies the files beside it — a package list, a web server's
  // configuration — and one of those failing the build is not something the
  // Dockerfile alone can fix. Editing was offered for the one file and the
  // rest were only stored, which left "editable" true of a plan and false of
  // the line that was wrong.
  const paths = Object.keys(files).sort((a, b) => {
    if (a === dockerfile) return -1;
    if (b === dockerfile) return 1;
    return a.localeCompare(b);
  });
  const [path, setPath] = useState<string | null>(null);
  const showing = path && files[path] !== undefined ? path : dockerfile;
  const current = files[showing];

  const save = useMutation({
    mutationFn: (content: string) =>
      api.post(`/apps/${appID}/specs`, {
        ...spec,
        build: { ...spec?.build, generated_files: { ...files, [showing]: content } },
      }),
    onSuccess: () => {
      setDraft(null);
      void queries.invalidateQueries({ queryKey: ['apps', appID] });
      void queries.invalidateQueries({ queryKey: ['apps', appID, 'specs'] });
    },
  });

  // Nothing to show for an app built from its own Dockerfile, or one that has
  // not been through detection yet. Saying so beats an empty panel.
  if (!spec || current === undefined) return null;

  return (
    <section>
      <div style={{ display: 'flex', alignItems: 'baseline', justifyContent: 'space-between' }}>
        <h4 style={{ font: 'var(--type-h4)', margin: '0 0 var(--space-2)' }}>How this app is built</h4>
        {draft === null ? (
          canEdit && (
            <Button variant="secondary" onClick={() => setDraft(current)}>
              Edit
            </Button>
          )
        ) : (
          <div style={{ display: 'flex', gap: 'var(--space-3)' }}>
            <Button variant="ghost" onClick={() => setDraft(null)}>
              Discard
            </Button>
            <Button variant="primary" disabled={save.isPending} onClick={() => save.mutate(draft)}>
              {save.isPending ? 'Saving' : 'Save'}
            </Button>
          </div>
        )}
      </div>

      <Quiet>
        Pando worked this out because the repository has no Dockerfile. It is kept here rather than
        in your repository, and it takes effect at the next deploy.
      </Quiet>

      {paths.length > 1 && (
        <div style={{ marginTop: 'var(--space-3)', maxWidth: '52ch' }}>
          <Select
            label="File"
            mono
            value={showing}
            options={paths}
            disabled={draft !== null}
            helper={draft !== null ? 'Save or discard this edit before opening another file.' : undefined}
            onChange={(e) => setPath(e.target.value)}
          />
        </div>
      )}

      {save.isError && <Banner tone="failed">{messageOf(save.error)}</Banner>}

      <div style={{ marginTop: 'var(--space-4)' }}>
        {draft === null || !canEdit ? (
          <pre
            style={{
              font: 'var(--type-code-sm)',
              background: 'var(--paper-sunken)',
              border: 'var(--border-width) solid var(--rule)',
              borderRadius: 'var(--radius-sm)',
              padding: 'var(--space-4)',
              overflowX: 'auto',
              margin: 0,
            }}
          >
            {current}
          </pre>
        ) : (
          <Input
            as="textarea"
            rows={22}
            mono
            value={draft}
            label={showing}
            helper="Saved as a new revision. The previous one stays in the app's history."
            onChange={(e) => setDraft(e.target.value)}
          />
        )}
      </div>
    </section>
  );
}
