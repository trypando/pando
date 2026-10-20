// A repository from a source connection that can list them (R-091): type part
// of its name and choose it from the list under the field.
//
// One field, not a search box above a select. The server searches; the field
// asks a moment after the typing stops, and keeps the last answer up while the
// next is on its way so the list does not blink empty on every key.
//
// The same shape as RecipientField and ActorField — an Input with a listbox
// under it, arrow keys and Enter — so the pickers in the console behave alike.

import { useId, useRef, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { Input } from '@design';

import { api } from '@api/client';
import { refusal } from '../install/Accounts';
import { pickKeys, useCloseOnOutside } from '../ui/combobox';
import { ComboList, ComboOption } from '../ui/ComboOption';
import { useSettled } from '../ui/paged';

interface Repository {
  url: string;
  full_name: string;
}

const SHOWN = 8;

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
  const [text, setText] = useState('');
  const [chosen, setChosen] = useState(false);
  const [open, setOpen] = useState(false);
  const [at, setAt] = useState(0);
  const listID = useId();
  const root = useRef<HTMLDivElement>(null);
  // A chosen repository's full name is in the field; searching for it would
  // narrow the list to the one already picked.
  const q = useSettled(chosen ? '' : text.trim(), 200);

  useCloseOnOutside(root, open, () => setOpen(false));

  const found = useQuery({
    queryKey: ['source-repositories', source, q],
    queryFn: () => {
      const search = q ? '?q=' + encodeURIComponent(q) : '';
      return api.get<{ repositories: Repository[] | null }>(`/sources/${encodeURIComponent(source)}/repositories${search}`);
    },
    placeholderData: keepPreviousData,
  });

  const options = (found.data?.repositories ?? []).slice(0, SHOWN);

  const choose = (r: Repository) => {
    setText(r.full_name);
    setChosen(true);
    setOpen(false);
    onChange(r.url);
  };

  // Said only once there is an answer to say it about.
  // A failed search is said as the field's error, not as an empty list.
  const none = open && !found.isPending && !found.isError && !found.isPlaceholderData && options.length === 0;

  return (
    <div ref={root} style={{ position: 'relative' }}>
      <Input
        label="Repository"
        mono
        role="combobox"
        aria-expanded={open && options.length > 0}
        aria-autocomplete="list"
        aria-controls={listID}
        aria-activedescendant={open && options[at] !== undefined ? `${listID}-${at}` : undefined}
        placeholder={found.isPending ? 'Loading repositories' : 'Search by name'}
        value={text}
        autoComplete="off"
        error={found.isError ? refusal(found.error) : error}
        onChange={(e) => {
          setText(e.target.value);
          setAt(0);
          setOpen(true);
          // What is typed is no longer the choice.
          if (chosen) {
            setChosen(false);
            onChange('');
          }
        }}
        onFocus={() => setOpen(true)}
        onKeyDown={pickKeys({ open, at, n: options.length, setOpen, setAt, choose: (i) => choose(options[i]!) })}
      />
      {open && (options.length > 0 || none) && (
        <ComboList id={listID} label="Matching repositories" none={none ? 'No repositories match.' : undefined}>
          {options.map((r, i) => (
            <ComboOption
              key={r.url}
              id={`${listID}-${i}`}
              active={i === at}
              onChoose={() => choose(r)}
              onHover={() => setAt(i)}
              label={r.full_name}
              mono
            />
          ))}
        </ComboList>
      )}
    </div>
  );
}
