// Several people, chosen by typing: a TagField over a list the page already
// has, filtered here as the person types.

import { useState } from 'react';

import { matches } from './search';
import { TagField } from './TagField';

export interface Person {
  id: string;
  display_name?: string;
  email?: string;
  external_id?: string;
}

/** What a person is called: their name, else their email, else their ID. */
export function personName(p: Person | undefined, id: string): string {
  return p?.display_name || p?.email || p?.external_id || id;
}

export function PeopleField({
  label,
  people,
  value,
  onChange,
}: {
  label: string;
  /** Everyone who could be chosen. */
  people: Person[];
  /** The chosen people's IDs, in the order they were added. */
  value: string[];
  onChange: (ids: string[]) => void;
}) {
  const [text, setText] = useState('');
  const options = people
    .filter((p) => !value.includes(p.id))
    .filter((p) => matches(text, p.display_name, p.email, p.external_id, p.id))
    .map((p) => ({ id: p.id, name: p.display_name || p.external_id || p.id, detail: p.email }));

  return (
    <TagField
      label={label}
      value={value}
      onChange={onChange}
      nameOf={(id) =>
        personName(
          people.find((p) => p.id === id),
          id,
        )
      }
      text={text}
      onText={setText}
      options={options}
      placeholder="Type a name or email"
    />
  );
}
