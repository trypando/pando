// Where an app is reached, and changing it: GET and PUT /apps/{id}/routing.
//
// In the Overview's Address row, beside the address itself. A dialog rather
// than an edit in place, unlike the name: a changed address moves the app, the
// old one stops working once it is deployed, and that is said before anything
// is saved (R-165).
//
// What the console shows is the server's answer, not its own: which adapters
// an app can move to, which modes each serves, and the address that results
// (R-261). A mode the adapter does not default to is shown to everyone and
// chosen only by whoever holds app.routing.override (R-163) — the server
// refuses it otherwise, and a control that is refused when used teaches
// nothing.

import { useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Banner, Button, Dialog, Input, Radio, Select } from '@design';

import { api } from '@api/client';
import type { App, Routing } from '@api/types.gen';
import { messageOf } from '../install/Accounts';
import { AppVerb, useCan } from './verbs';

interface RoutingOption {
  adapter_ref: string;
  name: string;
  modes: string[];
  default_mode: string;
  base_domain?: string;
  is_default: boolean;
}

interface RoutingView {
  routing: Routing;
  address?: string;
  next_routing?: Routing;
  next_address?: string;
  options: RoutingOption[] | null;
}

/** An address as it reads, without the scheme. */
function shown(address: string): string {
  return address.replace(/^(https?:)?\/\//, '');
}

export function AppAddress({ app }: { app: App }) {
  const canEdit = useCan(AppVerb.SpecEdit);
  const [changing, setChanging] = useState(false);

  const routing = useQuery({
    queryKey: ['apps', app.id, 'routing'],
    queryFn: () => api.get<RoutingView>(`/apps/${app.id}/routing`),
    enabled: Boolean(app.pinned_spec_id),
  });
  const next = routing.data?.next_address;

  return (
    <span style={{ display: 'inline-flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-3)', flexWrap: 'wrap' }}>
        {/* From the server, not assembled here: where an app is reached
            follows its routing mode, and the console does not decide routing
            (R-261). */}
        {app.address ? (
          // Its own tab: an app is a different place from the console.
          <a href={app.address} target="_blank" rel="noopener noreferrer">
            {shown(app.address)}
          </a>
        ) : (
          <span style={{ color: 'var(--ink-secondary)' }}>This app gets an address when it is first deployed.</span>
        )}
        {canEdit && app.pinned_spec_id && routing.data && (
          <Button variant="secondary" onClick={() => setChanging(true)}>
            Change
          </Button>
        )}
      </span>
      {next && (
        <span style={{ font: 'var(--type-caption)', color: 'var(--ink-secondary)' }}>
          Moves to {shown(next)} when this app is next deployed.
        </span>
      )}
      {changing && routing.data && (
        <ChangeAddress app={app} view={routing.data} onClose={() => setChanging(false)} />
      )}
    </span>
  );
}

const MODE_LABEL: Record<string, string> = {
  subdomain: 'Its own hostname',
  path: 'A path on Pando’s address',
  port: 'Its own port',
};

function ChangeAddress({ app, view, onClose }: { app: App; view: RoutingView; onClose: () => void }) {
  const queries = useQueryClient();
  const canOverride = useCan(AppVerb.RoutingOverride);
  const options = view.options ?? [];
  // Start from where the app is going, if a change is already saved.
  const current = view.next_routing ?? view.routing;

  const [adapterRef, setAdapterRef] = useState(current.adapter_ref);
  const option = options.find((o) => o.adapter_ref === adapterRef);
  const sameAdapter = adapterRef === current.adapter_ref;
  const [mode, setMode] = useState(current.mode);
  const [hostname, setHostname] = useState(current.hostname ?? '');
  const [path, setPath] = useState(current.path_prefix ?? `/${app.slug}`);

  const pickAdapter = (ref: string) => {
    setAdapterRef(ref);
    const o = options.find((x) => x.adapter_ref === ref);
    if (!o) return;
    // Back on the app's own adapter, its own settings; on another, that
    // adapter's defaults — the old mode may be one it does not serve.
    if (ref === current.adapter_ref) {
      setMode(current.mode);
      setHostname(current.hostname ?? '');
    } else {
      setMode(o.default_mode);
      setHostname(o.base_domain ? `${app.slug}.${o.base_domain}` : '');
    }
  };

  const change = useMutation({
    mutationFn: () =>
      api.put<{ changed: boolean }>(`/apps/${app.id}/routing`, {
        adapter_ref: adapterRef,
        mode,
        hostname: mode === 'subdomain' ? hostname.trim() : undefined,
        path_prefix: mode === 'path' ? path.trim() : undefined,
        // The dialog is the confirmation: it says what stops working before
        // the button is pressed.
        confirm: true,
      }),
    onSuccess: () => {
      void queries.invalidateQueries({ queryKey: ['apps', app.id, 'routing'] });
      void queries.invalidateQueries({ queryKey: ['apps', app.id, 'specs'] });
      onClose();
    },
  });

  const modes = option?.modes ?? [];
  const blocked = option !== undefined && mode !== option.default_mode && !canOverride && !(sameAdapter && mode === current.mode);

  return (
    <Dialog
      open
      onClose={onClose}
      title="Change address"
      description="Saved now, and used from the next deploy."
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            disabled={
              change.isPending ||
              !option ||
              blocked ||
              (mode === 'subdomain' && hostname.trim() === '') ||
              (mode === 'path' && path.trim() === '')
            }
            onClick={() => change.mutate()}
          >
            {change.isPending ? 'Changing' : 'Change address'}
          </Button>
        </>
      }
    >
      <div style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-4)' }}>
        {options.length > 1 &&
          (options.length <= 3 ? (
            <fieldset style={{ border: 0, margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
              <legend style={{ font: 'var(--type-label)', color: 'var(--ink)', marginBottom: 'var(--space-2)' }}>
                Routing adapter
              </legend>
              {options.map((o) => (
                <Radio
                  key={o.adapter_ref}
                  name="adapter"
                  value={o.adapter_ref}
                  label={o.is_default ? `${o.name} (the installation's default)` : o.name}
                  checked={adapterRef === o.adapter_ref}
                  onChange={() => pickAdapter(o.adapter_ref)}
                />
              ))}
            </fieldset>
          ) : (
            <Select
              label="Routing adapter"
              value={adapterRef}
              options={options.map((o) => ({
                value: o.adapter_ref,
                label: o.is_default ? `${o.name} (the installation's default)` : o.name,
              }))}
              onChange={(e) => pickAdapter(e.target.value)}
            />
          ))}

        {modes.length > 1 && (
          <fieldset style={{ border: 0, margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
            <legend style={{ font: 'var(--type-label)', color: 'var(--ink)', marginBottom: 'var(--space-2)' }}>
              Reached at
            </legend>
            {modes.map((m) => {
              const isDefault = m === option?.default_mode;
              const kept = sameAdapter && m === current.mode;
              const locked = !isDefault && !kept && !canOverride;
              return (
                <Radio
                  key={m}
                  name="mode"
                  value={m}
                  label={MODE_LABEL[m] ?? m}
                  description={
                    locked
                      ? 'Choosing something other than this adapter’s default needs permission to override routing.'
                      : m === 'path'
                        ? 'A path on the address Pando is served at, such as /notes.'
                        : m === 'port'
                          ? 'Pando assigns a port from the installation’s range.'
                          : undefined
                  }
                  checked={mode === m}
                  disabled={locked}
                  onChange={() => setMode(m)}
                />
              );
            })}
          </fieldset>
        )}

        {mode === 'subdomain' && (
          <Input
            label="Hostname"
            mono
            autoComplete="off"
            spellCheck={false}
            value={hostname}
            placeholder={option?.base_domain ? `${app.slug}.${option.base_domain}` : 'notes.example.com'}
            helper={option?.base_domain ? `A name ending in ${option.base_domain}.` : undefined}
            onChange={(e) => setHostname(e.target.value)}
          />
        )}

        {mode === 'path' && (
          <Input
            label="Path"
            mono
            autoComplete="off"
            spellCheck={false}
            value={path}
            placeholder={`/${app.slug}`}
            helper="Lowercase letters, digits and hyphens, up to four parts, such as /team/notes. The app receives requests with this part removed, and is told it in X-Forwarded-Prefix."
            onChange={(e) => setPath(e.target.value)}
          />
        )}

        {/* R-166: path routing puts the app on Pando's own origin, where its
            script can use Pando as whoever opens it. Accepted for path
            routing, and said wherever it is chosen. */}
        {mode === 'path' && (
          <Banner tone="info">
            At a path, this app shares Pando’s address, so any script it serves can act in Pando as whoever opens it. Use
            a hostname for an app whose code you don’t fully trust.
          </Banner>
        )}

        {view.address && (
          <Banner tone="info">
            Once this is deployed, {shown(view.address)} stops working, and links and bookmarks to it break.
          </Banner>
        )}
        {change.isError && <Banner tone="failed">{messageOf(change.error)}</Banner>}
      </div>
    </Dialog>
  );
}
