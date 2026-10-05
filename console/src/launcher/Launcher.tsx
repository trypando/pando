// The launcher (R-264).
//
// Root is this, not a dashboard. A non-technical user's first experience is a
// page of tiles for the apps they can reach — and R-005 is the reason: someone
// who may not know what a port is should not be met by a management console.
//
// Tiles come from GET /me/apps, which is scoped to **data-plane** grants. That
// is a different list from GET /apps, which is control-plane scoped: two
// planes, two endpoints (R-070, R-071). Using the wrong one here would show
// someone an app they can administer but not open, or hide one they use daily.
//
// R-266: sharing sends no message. An app appearing here is the notification,
// so the list is the whole mechanism and has to be right.
//
// Favorites (R-341) and sections (R-342) are the same list, split up. One
// request carries the apps, where each is filed and the sections themselves,
// so no two parts of this page can disagree about what you can open.
//
// Arranging the page happens on the page: each tile's menu, each section's
// heading, one quiet "New section" at the foot, and dragging a tile from one
// group to another. The menu does everything dragging does, for a keyboard or
// a touch screen. There is no settings screen for it. Someone who never opens
// a menu sees "Your apps" and nothing else.

import { useEffect, useRef, useState } from 'react';
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query';
import { Button, EmptyState, Icon, IconButton, Input, Logo, Skeleton } from '@design';

import { api, base } from '@api/client';
import type { App, Section } from '@api/types.gen';
import { statusLabel } from '../ui/status';
import { Sheet } from '../ui/Sheet';
import { InboxButton } from '../ui/Inbox';
import { Menu, MenuDivider, MenuItem } from '../ui/Menu';
import { NoMatches, SearchField } from '../ui/SearchField';
import { matches } from '../ui/search';
import { useNarrow } from '../ui/narrow';
import { TopoBackground, TopoTile } from '../ui/TopoBackground';

type MyApps = { apps: App[] | null; sections: Section[] | null };

const KEY = ['me', 'apps'];

/** What a dragged tile carries: its app's ID, under a type only tiles use. */
const DRAG_TYPE = 'application/x-pando-app';

const SEARCH_ID = 'launcher-search';

export function Launcher({
  onAdmin,
  onSettings,
  onManage,
}: {
  onAdmin?: () => void;
  /** Theme and signing out, which everyone can reach. */
  onSettings: () => void;
  /** Opens an app in the admin console. Given only to someone who administers
   *  something; each tile then offers it only for an app they administer. */
  onManage?: (appID: string) => void;
}) {
  const apps = useQuery({ queryKey: KEY, queryFn: () => api.get<MyApps>('/me/apps') });
  const narrow = useNarrow();

  // Which of these apps the person can also administer: GET /apps, the
  // control-plane list — the same query the Admin entry is decided by, so it
  // is already in the cache. Two planes, two lists (R-070, R-071); an app is
  // offered in admin only when it is on the second one, never because it is
  // on the first.
  const managed = useQuery({
    queryKey: ['apps'],
    queryFn: () => api.get<{ apps: App[] | null }>('/apps'),
    enabled: Boolean(onManage),
    retry: false,
  });
  const manageable = new Set((managed.data?.apps ?? []).map((a) => a.id));
  const arrange = useArrange();
  const [collapsed, toggleCollapsed] = useCollapsed();

  const all = apps.data?.apps ?? [];
  const sections = apps.data?.sections ?? [];
  const known = new Set(sections.map((s) => s.id));

  // A favorite shows once, in Favorites, whatever section it is filed under.
  const pinned = all.filter((a) => a.favorite);
  const inSection = (id: string) => all.filter((a) => !a.favorite && a.section_id === id);
  const rest = all.filter((a) => !a.favorite && !(a.section_id && known.has(a.section_id)));

  // Search narrows every group at once, by name. A group it finds nothing in
  // is hidden, and a collapsed one is opened — a match folded out of sight is
  // a match not found.
  const [query, setQuery] = useState('');
  const searching = query.trim() !== '';
  const shown = (app: App) => matches(query, app.name, app.slug);
  const shownPinned = pinned.filter(shown);
  const shownRest = rest.filter(shown);

  // "/" jumps to the search, as it does on most sites with one, unless focus is
  // already in something that takes typing.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== '/' || e.metaKey || e.ctrlKey || e.altKey) return;
      const el = document.activeElement;
      if (el instanceof HTMLInputElement || el instanceof HTMLTextAreaElement || (el as HTMLElement | null)?.isContentEditable) return;
      e.preventDefault();
      document.getElementById(SEARCH_ID)?.focus();
    };
    document.addEventListener('keydown', onKey);
    return () => document.removeEventListener('keydown', onKey);
  }, []);

  // The app being dragged, if any. While there is one, Your apps shows even
  // when empty, so there is somewhere to drag an app out of every section —
  // it sits at the foot of the page, so appearing moves nothing. An empty
  // Favorites does not: at the top, it pushed the whole page down under the
  // pointer the moment a drag began. The first favorite comes from the menu.
  const [dragging, setDragging] = useState<App | null>(null);

  // What a drop means depends only on where it lands. Favorites favorites it.
  // Anywhere else files it there — and un-favorites it, because a favorite is
  // only shown in Favorites, and a tile dragged out of Favorites that stayed
  // there would look like a drop that did nothing.
  const dropInto = (target: 'favorites' | 'unsorted' | string) => (appID: string) => {
    const app = all.find((a) => a.id === appID);
    if (!app) return;
    if (target === 'favorites') {
      if (!app.favorite) arrange.favorite.mutate({ app, on: true });
      return;
    }
    if (app.favorite) arrange.favorite.mutate({ app, on: false });
    const to = target === 'unsorted' ? null : target;
    if ((app.section_id ?? null) !== to) arrange.move.mutate({ app, to });
  };

  const tile = (app: App) => (
    <Tile
      key={app.id}
      app={app}
      sections={sections}
      arrange={arrange}
      onManage={onManage && manageable.has(app.id) ? () => onManage(app.id) : undefined}
      onDragChange={(on) => setDragging(on ? app : null)}
    />
  );

  return (
    // The contour map in the header alone, fading out before it reaches the
    // tiles. Every tile carries its own map, and a map behind the whole grid
    // competed with them for the eye; in the header it marks the page without
    // sitting under anything.
    <div style={{ minHeight: '100vh', background: 'var(--paper)', position: 'relative', isolation: 'isolate' }}>
      <header
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          // On a phone the search takes a line of its own under the logo and
          // the controls, rather than being squeezed between them.
          flexWrap: narrow ? 'wrap' : undefined,
          gap: narrow ? 'var(--space-3)' : undefined,
          padding: `${narrow ? 'var(--space-4)' : 'var(--space-5)'} var(--console-padding)`,
          borderBottom: 'var(--border-width) solid var(--rule)',
          position: 'relative',
          isolation: 'isolate',
          overflow: 'hidden',
        }}
      >
        <TopoBackground seed="launcher" fade="linear-gradient(to bottom, black 30%, transparent)" />
        <Logo size={20} />
        <div style={narrow ? { order: 3, flexBasis: '100%' } : undefined}>
          <SearchField
            id={SEARCH_ID}
            value={query}
            onChange={setQuery}
            placeholder="Search apps"
            width={narrow ? '100%' : '40ch'}
          />
        </div>
        <div style={{ display: 'flex', alignItems: 'center', gap: 'var(--space-4)' }}>
          {onAdmin && (
            <button
              onClick={onAdmin}
              style={{
                border: 'none',
                background: 'transparent',
                padding: 0,
                cursor: 'pointer',
                font: 'var(--type-body-ui)',
                color: 'var(--ink-secondary)',
              }}
            >
              Admin
            </button>
          )}
          <InboxButton />
          <IconButton label="Settings" onClick={onSettings}>
            <Icon name="settings" size={16} />
          </IconButton>
        </div>
      </header>

      <main style={{ maxWidth: 'var(--console-max)', margin: '0 auto' }}>
        {/* Only when there is something in it: an empty "Favorites" heading on
            every launcher would be a section explaining a feature rather than
            showing anything. */}
        {shownPinned.length > 0 && (
          <Group
            id="favorites"
            title="Favorites"
            collapsed={collapsed}
            onToggle={toggleCollapsed}
            open={searching}
            onDropApp={dropInto('favorites')}
          >
            <Grid>{shownPinned.map(tile)}</Grid>
          </Group>
        )}

        {sections.map((section) => {
          const filed = inSection(section.id).filter(shown);
          // A search hides every section it finds nothing in, empty ones too.
          if (searching && filed.length === 0) return null;
          return (
            <Group
              key={section.id}
              id={section.id}
              title={section.name}
              collapsed={collapsed}
              onToggle={toggleCollapsed}
              open={searching}
              section={section}
              arrange={arrange}
              onDropApp={dropInto(section.id)}
            >
              {filed.length > 0 ? (
                <Grid>{filed.map(tile)}</Grid>
              ) : (
                <Quiet>Nothing here yet. Drag an app here, or use its menu to move it.</Quiet>
              )}
            </Group>
          );
        })}

        {/* Hidden once every app has somewhere else to be, rather than saying
            there is nothing shared with you directly under the apps that were. */}
        {(shownRest.length > 0 || (!searching && (all.length === 0 || dragging))) && (
          <Group
            id="unsorted"
            title="Your apps"
            collapsed={collapsed}
            onToggle={toggleCollapsed}
            open={searching}
            onDropApp={dropInto('unsorted')}
          >
            {/* Tiles' shapes, where the tiles will be: the page does not jump
                when they arrive, and nothing says "loading" in words. */}
            {apps.isPending && (
              <div role="status" aria-label="Loading your apps">
                <Grid>
                  {Array.from({ length: 4 }, (_, i) => (
                    <Skeleton key={i} height="auto" radius="md" style={{ aspectRatio: '1 / 1' }} />
                  ))}
                </Grid>
              </div>
            )}

            {apps.isError && <Quiet>Pando couldn&rsquo;t load your apps. Reload the page to try again.</Quiet>}

            {/* Not "you have no apps" — nothing has gone wrong, and an empty
                launcher is the normal state for someone who has just been given
                an account. */}
            {apps.data && all.length === 0 && (
              <EmptyState heading="Nothing shared with you yet">
                When someone shares an app with you, it shows up here.
              </EmptyState>
            )}

            {shownRest.length > 0 && <Grid>{shownRest.map(tile)}</Grid>}

            {/* Only while dragging: every app is elsewhere, and this is where
                one goes to be in no section at all. */}
            {all.length > 0 && rest.length === 0 && <Quiet>Drop an app here.</Quiet>}
          </Group>
        )}

        {searching && !all.some(shown) && (
          <div style={{ padding: 'var(--space-6) var(--console-padding)' }}>
            <NoMatches what="apps" query={query} />
          </div>
        )}

        {apps.data && all.length > 0 && !searching && <NewSection arrange={arrange} />}
      </main>
    </div>
  );
}

// --- arranging ---------------------------------------------------------------

type Arrange = ReturnType<typeof useArrange>;

/**
 * Every change a person can make to their own launcher, in one place.
 *
 * Favoriting and filing are optimistic — the tile moves the moment it is
 * clicked, and moves back if the server refuses — because a tile that takes a
 * round trip to move is a tile people click twice. Making, renaming and
 * deleting a section wait for the server, which names the section.
 */
function useArrange() {
  const queries = useQueryClient();

  const optimistic = async (change: (apps: App[]) => App[]) => {
    await queries.cancelQueries({ queryKey: KEY });
    const before = queries.getQueryData<MyApps>(KEY);
    queries.setQueryData<MyApps>(KEY, (old) => ({
      apps: change(old?.apps ?? []),
      sections: old?.sections ?? [],
    }));
    return { before };
  };
  const rollback = (_e: unknown, _v: unknown, context?: { before?: MyApps }) => {
    if (context?.before) queries.setQueryData(KEY, context.before);
  };
  const settle = () => void queries.invalidateQueries({ queryKey: KEY });

  const favorite = useMutation({
    mutationFn: ({ app, on }: { app: App; on: boolean }) =>
      on ? api.put<void>(`/me/favorites/${app.id}`) : api.del<void>(`/me/favorites/${app.id}`),
    onMutate: ({ app, on }) => optimistic((apps) => apps.map((a) => (a.id === app.id ? { ...a, favorite: on } : a))),
    onError: rollback,
    onSettled: settle,
  });

  // `to` of null is "Your apps": out of the section it is in.
  const move = useMutation({
    mutationFn: ({ app, to }: { app: App; to: string | null }) =>
      to
        ? api.put<void>(`/me/sections/${to}/apps/${app.id}`)
        : api.del<void>(`/me/sections/${app.section_id}/apps/${app.id}`),
    onMutate: ({ app, to }) =>
      optimistic((apps) => apps.map((a) => (a.id === app.id ? { ...a, section_id: to ?? undefined } : a))),
    onError: rollback,
    onSettled: settle,
  });

  // With an app, from its menu: made with the app already in it. Without, from
  // the foot of the page: made empty, to fill afterwards.
  const create = useMutation({
    mutationFn: async ({ name, app }: { name: string; app?: App }) => {
      const section = await api.post<Section>('/me/sections', { name });
      if (app) await api.put<void>(`/me/sections/${section.id}/apps/${app.id}`);
    },
    onSettled: settle,
  });

  const rename = useMutation({
    mutationFn: ({ id, name }: { id: string; name: string }) => api.patch<Section>(`/me/sections/${id}`, { name }),
    onSettled: settle,
  });

  const remove = useMutation({
    mutationFn: (id: string) => api.del<void>(`/me/sections/${id}`),
    onSettled: settle,
  });

  return { favorite, move, create, rename, remove };
}

/**
 * Which groups are folded away. Per browser, like the theme: it is how a page
 * is being looked at, not a fact about the person, and nobody needs it to
 * follow them to another machine. Storage can be refused; then nothing is
 * remembered and everything starts open.
 */
function useCollapsed(): [Set<string>, (id: string) => void] {
  const STORE = 'pando.launcher.collapsed';
  const [collapsed, setCollapsed] = useState<Set<string>>(() => {
    try {
      return new Set(JSON.parse(window.localStorage.getItem(STORE) ?? '[]') as string[]);
    } catch {
      return new Set();
    }
  });
  useEffect(() => {
    try {
      window.localStorage.setItem(STORE, JSON.stringify([...collapsed]));
    } catch {
      // Remembered for this page only.
    }
  }, [collapsed]);
  const toggle = (id: string) =>
    setCollapsed((old) => {
      const next = new Set(old);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  return [collapsed, toggle];
}

// --- groups ------------------------------------------------------------------

function Group({
  id,
  title,
  collapsed,
  onToggle,
  section,
  arrange,
  onDropApp,
  open,
  children,
}: {
  id: string;
  title: string;
  collapsed: Set<string>;
  onToggle: (id: string) => void;
  /** Shown open whatever was folded — while searching. */
  open?: boolean;
  /** Set for a person's own section, which can be renamed and deleted. */
  section?: Section;
  arrange?: Arrange;
  /** A tile was dropped here. Collapsed or not, the whole group takes it. */
  onDropApp?: (appID: string) => void;
  children: React.ReactNode;
}) {
  const closed = !open && collapsed.has(id);
  const [renaming, setRenaming] = useState<string | null>(null);

  // Counted, not a flag: dragging across a tile inside the group fires a
  // leave for the group and an enter for the tile, and a flag would flicker
  // off while still over the group.
  const [over, setOver] = useState(false);
  const depth = useRef(0);
  const ours = (e: React.DragEvent) => e.dataTransfer.types.includes(DRAG_TYPE);
  const drop = onDropApp && {
    onDragEnter: (e: React.DragEvent) => {
      if (!ours(e)) return;
      depth.current += 1;
      setOver(true);
    },
    onDragOver: (e: React.DragEvent) => {
      if (!ours(e)) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
    },
    onDragLeave: (e: React.DragEvent) => {
      if (!ours(e)) return;
      depth.current = Math.max(0, depth.current - 1);
      if (depth.current === 0) setOver(false);
    },
    onDrop: (e: React.DragEvent) => {
      if (!ours(e)) return;
      e.preventDefault();
      depth.current = 0;
      setOver(false);
      onDropApp(e.dataTransfer.getData(DRAG_TYPE));
    },
  };

  const heading =
    renaming !== null && section && arrange ? (
      <form
        onSubmit={(e) => {
          e.preventDefault();
          const name = renaming.trim();
          if (name && name !== section.name) arrange.rename.mutate({ id: section.id, name });
          setRenaming(null);
        }}
      >
        <Input
          aria-label="Section name"
          value={renaming}
          autoFocus
          onFocus={(e) => e.target.select()}
          onChange={(e) => setRenaming(e.target.value)}
          onBlur={() => setRenaming(null)}
          onKeyDown={(e) => {
            if (e.key === 'Escape') setRenaming(null);
          }}
        />
      </form>
    ) : (
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 'var(--space-2)' }}>
        <button
          type="button"
          onClick={() => onToggle(id)}
          aria-expanded={!closed}
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            gap: 'var(--space-2)',
            border: 'none',
            background: 'transparent',
            padding: 0,
            cursor: 'pointer',
            font: 'inherit',
            color: 'var(--ink)',
          }}
        >
          <Icon name={closed ? 'chevron-right' : 'chevron-down'} size={16} />
          {title}
        </button>
        {section && arrange && (
          <Menu label={`Options for the ${section.name} section`} align="start">
            {(close) => (
              <>
                <MenuItem
                  onSelect={() => {
                    close();
                    setRenaming(section.name);
                  }}
                >
                  Rename
                </MenuItem>
                {/* No confirmation: deleting a section loses nothing. Its apps
                    go back to Your apps, and it can be made again. */}
                <MenuItem
                  onSelect={() => {
                    close();
                    arrange.remove.mutate(section.id);
                  }}
                >
                  Delete section
                </MenuItem>
              </>
            )}
          </Menu>
        )}
      </span>
    );

  return (
    <div
      {...drop}
      style={{
        // Room for the outline, so showing it does not move anything.
        outline: `var(--border-width) dashed ${over ? 'var(--rule-strong)' : 'transparent'}`,
        outlineOffset: 'calc(-1 * var(--space-2))',
        borderRadius: 'var(--radius-md)',
      }}
    >
      <Sheet heading={heading}>{closed ? null : children}</Sheet>
    </div>
  );
}

function Grid({ children }: { children: React.ReactNode }) {
  return (
    <div
      style={{
        display: 'grid',
        // Fixed tracks, not 1fr: a tile that stretched to fill the row would be
        // a different size on every window, and a poster on a wide one.
        gridTemplateColumns: 'repeat(auto-fill, 16ch)',
        gap: 'var(--space-5)',
      }}
    >
      {children}
    </div>
  );
}

// --- tiles -------------------------------------------------------------------

/**
 * Whether opening the app would reach it.
 *
 * Running or degraded — degraded is still serving, just not every part of it
 * — and with an address. Anything else is greyed out and is not a link.
 */
function reachable(app: App): boolean {
  return Boolean(app.address) && (app.state === 'running' || app.state === 'degraded');
}

// Touch screens have no hover, so there the menu button is always shown.
const canHover = typeof window !== 'undefined' && window.matchMedia?.('(hover: hover)').matches;

function Tile({
  app,
  sections,
  arrange,
  onManage,
  onDragChange,
}: {
  app: App;
  sections: Section[];
  arrange: Arrange;
  onManage?: () => void;
  onDragChange: (dragging: boolean) => void;
}) {
  // A card with the app's picture and its name, and no status line. The
  // launcher is for someone who came to open an app (R-005), and "running" is
  // the normal case — a word on every tile saying so is noise. What they need
  // to know is which tiles will not open, and a greyed-out tile says that
  // without a word. The state is still in the accessible name and the hover
  // title, so the difference is never carried by appearance alone.
  const open = reachable(app);
  const label = open ? app.name : `${app.name} — ${statusLabel(app.state)}`;

  // The menu button shows when the tile is pointed at or tabbed into, and
  // while its menu is open: twenty buttons at rest are twenty things to look
  // past.
  const [near, setNear] = useState(false);
  const [menuOpen, setMenuOpen] = useState(false);
  const [lifted, setLifted] = useState(false);
  // Whether this tile's drag is still going. A drag that ends inside a frame
  // would otherwise have its start applied after its end, and leave the page
  // stuck showing a drag that is over.
  const dragActive = useRef(false);

  const card = (
    <div
      style={{
        aspectRatio: '1 / 1',
        display: 'flex',
        flexDirection: 'column',
        alignItems: 'center',
        justifyContent: 'center',
        gap: 'var(--space-3)',
        padding: 'var(--space-4)',
        background: 'var(--paper-raised)',
        border: `var(--border-width) solid ${near && open ? 'var(--rule-strong)' : 'var(--rule)'}`,
        borderRadius: 'var(--radius-md)',
        opacity: lifted ? 0.4 : open ? 1 : 0.45,
        filter: open ? 'none' : 'grayscale(1)',
      }}
    >
      <Picture app={app} />
      <span
        style={{
          maxWidth: '100%',
          font: 'var(--type-body-ui)',
          color: 'var(--ink)',
          textAlign: 'center',
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
        }}
      >
        {app.name}
      </span>
    </div>
  );

  return (
    // The menu is a sibling of the link, not inside it: a button in an anchor
    // is two controls in one, and a click on the menu would open the app.
    <div
      style={{ position: 'relative', cursor: lifted ? 'grabbing' : undefined }}
      // The whole tile drags, link and all. What travels is the app's ID
      // under a type of our own, so a group ignores anything else dragged
      // over it — a file, some text, another tab's link.
      draggable
      onDragStart={(e) => {
        e.dataTransfer.setData(DRAG_TYPE, app.id);
        e.dataTransfer.effectAllowed = 'move';
        // Everything that changes the page waits a frame. Chrome ends a drag
        // the moment it starts if the page moves under it during dragstart —
        // and this moves it: the empty groups appear above, the menu button
        // goes. Done synchronously, every drag was cancelled before the
        // pointer had moved.
        dragActive.current = true;
        requestAnimationFrame(() => {
          if (!dragActive.current) return;
          setLifted(true);
          setNear(false);
          onDragChange(true);
        });
      }}
      onDragEnd={() => {
        dragActive.current = false;
        setLifted(false);
        onDragChange(false);
      }}
      onMouseEnter={() => setNear(true)}
      onMouseLeave={() => setNear(false)}
      onFocus={() => setNear(true)}
      onBlur={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget as Node)) setNear(false);
      }}
    >
      {open ? (
        // An app is a different place from the console, so it opens in its
        // own tab. Every one of these addresses goes through Pando's proxy —
        // there is no other way in (R-023) — and which address depends on how
        // the app is routed, which the server knows.
        <a
          href={app.address}
          target="_blank"
          rel="noopener noreferrer"
          aria-label={label}
          style={{ textDecoration: 'none', color: 'inherit', display: 'block' }}
        >
          {card}
        </a>
      ) : (
        <div title={statusLabel(app.state)} aria-label={label} aria-disabled="true">
          {card}
        </div>
      )}

      {(canHover ? near || menuOpen : true) && (
        <div style={{ position: 'absolute', top: 'var(--space-1)', right: 'var(--space-1)' }}>
          <TileMenu
            app={app}
            open={open}
            sections={sections}
            arrange={arrange}
            onManage={onManage}
            onOpenChange={setMenuOpen}
          />
        </div>
      )}
    </div>
  );
}

/**
 * Launch, favorite, where the app lives — and, for someone who administers
 * it, the way to its admin screen. Filing it is a second page of the same menu
 * rather than a menu of its own, and that page ends with making a new section
 * for it.
 */
function TileMenu({
  app,
  open,
  sections,
  arrange,
  onManage,
  onOpenChange,
}: {
  app: App;
  open: boolean;
  sections: Section[];
  arrange: Arrange;
  onManage?: () => void;
  onOpenChange: (open: boolean) => void;
}) {
  const [page, setPage] = useState<'main' | 'sections' | 'new'>('main');
  const [name, setName] = useState('');

  return (
    <Menu
      label={`Options for ${app.name}`}
      view={page}
      onOpenChange={(o) => {
        onOpenChange(o);
        if (!o) {
          setPage('main');
          setName('');
        }
      }}
    >
      {(close) => {
        if (page === 'new') {
          return (
            <form
              style={{ padding: 'var(--space-2)' }}
              onSubmit={(e) => {
                e.preventDefault();
                const trimmed = name.trim();
                if (!trimmed) return;
                arrange.create.mutate({ name: trimmed, app });
                close();
              }}
            >
              <Input
                aria-label="New section name"
                placeholder="Section name"
                value={name}
                autoFocus
                onChange={(e) => setName(e.target.value)}
              />
            </form>
          );
        }

        if (page === 'sections') {
          return (
            <>
              <MenuItem
                checked={!app.section_id}
                onSelect={() => {
                  if (app.section_id) arrange.move.mutate({ app, to: null });
                  close();
                }}
              >
                Your apps
              </MenuItem>
              {sections.map((s) => (
                <MenuItem
                  key={s.id}
                  checked={app.section_id === s.id}
                  onSelect={() => {
                    if (app.section_id !== s.id) arrange.move.mutate({ app, to: s.id });
                    close();
                  }}
                >
                  {s.name}
                </MenuItem>
              ))}
              <MenuDivider />
              <MenuItem onSelect={() => setPage('new')}>New section…</MenuItem>
            </>
          );
        }

        return (
          <>
            {open && (
              <MenuItem href={app.address} onSelect={close}>
                Launch
              </MenuItem>
            )}
            {onManage && (
              <MenuItem
                onSelect={() => {
                  close();
                  onManage();
                }}
              >
                Open in admin
              </MenuItem>
            )}
            <MenuItem
              onSelect={() => {
                arrange.favorite.mutate({ app, on: !app.favorite });
                close();
              }}
            >
              {app.favorite ? 'Remove from favorites' : 'Add to favorites'}
            </MenuItem>
            <MenuItem onSelect={() => setPage('sections')}>Move to section…</MenuItem>
          </>
        );
      }}
    </Menu>
  );
}

/**
 * Making a section without starting from an app: a quiet line at the foot of
 * the page that becomes a name field. Below everything, because it is the
 * least frequent thing anybody does here.
 */
function NewSection({ arrange }: { arrange: Arrange }) {
  const [name, setName] = useState<string | null>(null);

  return (
    <div style={{ padding: '0 var(--console-padding) var(--space-8)' }}>
      {name === null ? (
        <Button variant="secondary" onClick={() => setName('')}>
          New section
        </Button>
      ) : (
        <form
          style={{ maxWidth: '32ch' }}
          onSubmit={(e) => {
            e.preventDefault();
            const trimmed = name.trim();
            if (trimmed) arrange.create.mutate({ name: trimmed });
            setName(null);
          }}
        >
          <Input
            aria-label="New section name"
            placeholder="Section name"
            value={name}
            autoFocus
            onChange={(e) => setName(e.target.value)}
            onBlur={() => setName(null)}
            onKeyDown={(e) => {
              if (e.key === 'Escape') setName(null);
            }}
          />
        </form>
      )}
    </div>
  );
}

/**
 * The picture inside the card: the app's image when it has one (R-340), and a
 * patch of terrain generated from its ID when it does not.
 */
function Picture({ app }: { app: App }) {
  // The timestamp is in the URL so a new image is a new address, and the
  // browser's cached copy of the old one is never shown.
  const src = app.icon_updated_at
    ? `${base}/apps/${app.id}/icon?v=${encodeURIComponent(app.icon_updated_at)}`
    : null;

  return (
    <div
      style={{
        width: '55%',
        aspectRatio: '1 / 1',
        flex: '0 0 auto',
        borderRadius: 'var(--radius-sm)',
        overflow: 'hidden',
      }}
    >
      {src ? (
        <img src={src} alt="" style={{ width: '100%', height: '100%', objectFit: 'cover', display: 'block' }} />
      ) : (
        <TopoTile seed={app.id} />
      )}
    </div>
  );
}

function Quiet({ children }: { children: React.ReactNode }) {
  return <p style={{ font: 'var(--type-body-ui)', color: 'var(--ink-secondary)', margin: 0 }}>{children}</p>;
}
