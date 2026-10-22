// A repository from a source connection that can list them (R-091): type part
// of its name and choose it from the list under the field.

import { api } from '@api/client';
import { SearchPicker, type Search } from '../ui/SearchPicker';

interface Repository {
  url: string;
  full_name: string;
}

function repositories(source: string): Search {
  return {
    key: (q) => ['source-repositories', source, q],
    find: async (q) => {
      const search = q ? '?q=' + encodeURIComponent(q) : '';
      const found = await api.get<{ repositories: Repository[] | null }>(
        `/sources/${encodeURIComponent(source)}/repositories${search}`,
      );
      return (found.repositories ?? []).map((r) => ({ id: r.url, label: r.full_name }));
    },
  };
}

export function RepositoryField({
  source,
  onChange,
  error,
}: Readonly<{
  /** The source connection to list from. */
  source: string;
  /** The chosen repository's URL, or empty once the text no longer names one. */
  onChange: (url: string) => void;
  error?: string;
}>) {
  return (
    <SearchPicker
      label="Repository"
      noun="repositories"
      mono
      search={repositories(source)}
      error={error}
      onChange={(chosen) => onChange(chosen?.id ?? '')}
    />
  );
}
