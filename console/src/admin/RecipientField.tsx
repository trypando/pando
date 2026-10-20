// Who to share an app with: type to find an account or a group, then choose
// one from the list.
//
// Backed by GET /apps/{id}/principals, not by the install's account list. That
// list is install.view, and the person sharing an app is anyone holding
// app.grants.manage on it — most of whom cannot read every account, and should
// not have to in order to share one app. The server searches; the field only
// asks, a moment after the typing stops.
//
// A choice, not free text. The form used to take an email address and send it
// as-is, which the server never accepted: sharing needs a principal ID, and the
// only honest way to get one is to pick it from what exists.
//
// The same shape as the audit log's ActorField — an Input with a listbox under
// it, arrow keys and Enter — so the two pickers in the console behave alike.

import { useEffect, useId, useRef, useState } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import { Input } from '@design';

import { api } from '@api/client';
import type { Recipient } from './share-access';
import { pickKeys, useCloseOnOutside } from '../ui/combobox';
import { ComboList, ComboOption } from '../ui/ComboOption';

interface Principals {
  users: { id: string; username: string; name?: string }[] | null;
  groups: { id: string; name: string }[] | null;
}

interface Option {
  id: string;
  label: string;
  note?: string;
}

/** The chosen recipient, with the name the field shows for it. */
export type Chosen = Recipient & { label: string };

export function RecipientField({
  appID,
  kind,
  value,
  onChange,
}: {
  appID: string;
  kind: 'user' | 'group';
  value: Chosen | null;
  onChange: (chosen: Chosen | null) => void;
}) {
  const [text, setText] = useState(value?.label ?? '');
  const [open, setOpen] = useState(false);
  const [at, setAt] = useState(0);
  const listID = useId();
  const root = useRef<HTMLDivElement>(null);
  const q = useSettled(text.trim(), 200);

  // Cleared from outside — after sharing, or on switching between an account
  // and a group — clears the text too.
  useEffect(() => {
    if (!value) setText('');
  }, [value]);

  useCloseOnOutside(root, open, () => setOpen(false));

  const found = useQuery({
    queryKey: ['apps', appID, 'principals', q],
    queryFn: () => api.get<Principals>(`/apps/${appID}/principals?q=${encodeURIComponent(q)}`),
    enabled: open,
    // The last answer stays up while the next is asked for, so the list does
    // not blink empty on every keystroke.
    placeholderData: keepPreviousData,
  });

  const options: Option[] =
    kind === 'user'
      ? (found.data?.users ?? []).map((u) => ({ id: u.id, label: u.username, note: u.name }))
      : (found.data?.groups ?? []).map((g) => ({ id: g.id, label: g.name }));

  const choose = (o: Option) => {
    setText(o.label);
    setOpen(false);
    onChange({ kind, id: o.id, label: o.label });
  };

  const label = kind === 'user' ? 'Account' : 'Group';
  // Said only once there is an answer to say it about: "No accounts match"
  // under a list that has not loaded yet would be a guess.
  const none = open && !found.isPending && !found.isPlaceholderData && options.length === 0;
  const noMatch = kind === 'user' ? 'No accounts match.' : 'No groups match.';

  return (
    <div ref={root} style={{ position: 'relative' }}>
      <Input
        label={label}
        role="combobox"
        aria-expanded={open && options.length > 0}
        aria-autocomplete="list"
        aria-controls={listID}
        aria-activedescendant={open && options[at] !== undefined ? `${listID}-${at}` : undefined}
        placeholder={kind === 'user' ? 'Username, name or email' : 'Group name'}
        value={text}
        autoComplete="off"
        onChange={(e) => {
          setText(e.target.value);
          setAt(0);
          setOpen(true);
          // What is typed is no longer the choice, so there is none until
          // another is made.
          if (value) onChange(null);
        }}
        onFocus={() => setOpen(true)}
        onKeyDown={pickKeys({ open, at, n: options.length, setOpen, setAt, choose: (i) => choose(options[i]!) })}
      />
      {open && (options.length > 0 || none) && (
        <ComboList
          id={listID}
          label={kind === 'user' ? 'Matching accounts' : 'Matching groups'}
          none={none ? noMatch : undefined}
        >
          {options.map((o, i) => (
            <ComboOption
              key={o.id}
              id={`${listID}-${i}`}
              active={i === at}
              onChoose={() => choose(o)}
              onHover={() => setAt(i)}
              label={o.label}
              detail={o.note}
            />
          ))}
        </ComboList>
      )}
    </div>
  );
}

/** `value`, once it has stopped changing for `ms` — one search per pause in
 *  typing rather than one per key. */
function useSettled<T>(value: T, ms: number): T {
  const [settled, setSettled] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setSettled(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return settled;
}
