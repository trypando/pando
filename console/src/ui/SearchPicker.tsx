// One thing chosen from a list the server searches: type part of its name and
// choose it from the list under the field.
//
// One field, not a search box above a select (#286). The field asks a moment
// after the typing stops, and keeps the last answer up while the next is on its
// way so the list does not blink empty on every key. Nothing is ever listed in
// full: an install may hold twenty thousand apps (issue #72).
//
// The same shape as RecipientField and ActorField — an Input with a listbox
// under it, arrow keys and Enter — so the pickers in the console behave alike.

import { useId, useRef, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { Input } from '@design';

import { refusal } from '../install/Accounts';
import { pickKeys, useCloseOnOutside } from './combobox';
import { ComboList, ComboOption } from './ComboOption';
import { useSettled } from './paged';

export interface Picked {
  id: string;
  label: string;
}

/** What the server finds for q: its query key, and how to ask. */
export interface Search {
  key: (q: string) => readonly unknown[];
  find: (q: string) => Promise<Picked[]>;
}

/** How many options the list holds at once. */
export const SHOWN = 8;

export function SearchPicker({
  label,
  noun,
  search,
  onChange,
  mono = false,
  error,
}: Readonly<{
  label: string;
  /** What is listed, plural and lowercase: "repositories". */
  noun: string;
  search: Search;
  /** The chosen option, or null once the text no longer names one. */
  onChange: (chosen: Picked | null) => void;
  mono?: boolean;
  error?: string;
}>) {
  const [text, setText] = useState('');
  const [chosen, setChosen] = useState(false);
  const [open, setOpen] = useState(false);
  const [at, setAt] = useState(0);
  const listID = useId();
  const root = useRef<HTMLDivElement>(null);
  // A choice's name is in the field; searching for it would narrow the list to
  // the one already picked.
  const q = useSettled(chosen ? '' : text.trim(), 200);

  useCloseOnOutside(root, open, () => setOpen(false));

  const found = useQuery({
    queryKey: search.key(q),
    queryFn: () => search.find(q),
    placeholderData: keepPreviousData,
  });

  const options = (found.data ?? []).slice(0, SHOWN);

  const choose = (o: Picked) => {
    setText(o.label);
    setChosen(true);
    setOpen(false);
    onChange(o);
  };

  // Said only once there is an answer to say it about. A failed search is said
  // as the field's error, not as an empty list.
  const none = open && !found.isPending && !found.isError && !found.isPlaceholderData && options.length === 0;

  return (
    <div ref={root} style={{ position: 'relative' }}>
      <Input
        label={label}
        mono={mono}
        role="combobox"
        aria-expanded={open && options.length > 0}
        aria-autocomplete="list"
        aria-controls={listID}
        aria-activedescendant={open && options[at] !== undefined ? `${listID}-${at}` : undefined}
        placeholder={found.isPending ? `Loading ${noun}` : 'Search by name'}
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
            onChange(null);
          }
        }}
        onFocus={() => setOpen(true)}
        onKeyDown={pickKeys({ open, at, n: options.length, setOpen, setAt, choose: (i) => choose(options[i]!) })}
      />
      {open && (options.length > 0 || none) && (
        <ComboList id={listID} label={`Matching ${noun}`} none={none ? `No ${noun} match.` : undefined}>
          {options.map((o, i) => (
            <ComboOption
              key={o.id}
              id={`${listID}-${i}`}
              active={i === at}
              onChoose={() => choose(o)}
              onHover={() => setAt(i)}
              label={o.label}
              mono={mono}
            />
          ))}
        </ComboList>
      )}
    </div>
  );
}
