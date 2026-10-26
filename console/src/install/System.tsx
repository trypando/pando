// System: how the installation itself is set up and kept (issue #154). A list
// of its pages on the left — the same list as an app's Settings — and one
// page at a time, each at an address of its own. At phone width the list is a
// picker at the top.
//
// Each page keeps the verb it had as a screen of its own, and is listed only
// to somebody who holds it, so nobody sees a page here they could not see
// before. Policy's draft is held here, above its pages, so that changes on
// several of them are saved together from one bar.

import { useState } from 'react';
import { Badge, Select } from '@design';

import { POLICY_PAGES, format, type PolicyPage, type Route, type SystemTab } from '../app/route';
import { RouteLink } from '../ui/RouteLink';
import { Sheet } from '../ui/Sheet';
import { useNarrow } from '../ui/narrow';
import { AIButton } from '../ui/AskAI';
import { useAIFunctionOn } from './AIFunctions';
import { Backups } from './Backups';
import { Hosts } from './Hosts';
import { Installation } from './Installation';
import { Overview } from './Overview';
import { POLICY_LABELS, PolicyPageView, SaveBar, UpdateSettings } from './PolicyPages';
import { PolicyAI } from './PolicyAI';
import { SystemPage } from './SystemPage';
import { PolicyDraftContext, changedPages, usePolicy, usePolicyDraft } from './policyDraft';
import { LineSkeleton } from '../ui/Loading';
import '../admin/settings.css';
import './system.css';

const LABELS: Record<SystemTab, string> = {
  overview: 'Overview',
  adapters: 'Adapters',
  policy: 'Policy',
  backups: 'Backups',
  hosts: 'Hosts',
};

/** The pages a person may see, in order. */
export function systemPages(can: {
  view: boolean;
  managePolicy: boolean;
  manageBackups: boolean;
  /** install.cluster.manage: the cluster's hosts and join tokens (R-414). */
  manageCluster?: boolean;
}): SystemTab[] {
  const pages: SystemTab[] = [];
  if (can.view) pages.push('overview', 'adapters');
  if (can.view || can.managePolicy) pages.push('policy');
  if (can.manageBackups) pages.push('backups');
  if (can.manageCluster) pages.push('hosts');
  return pages;
}

export function System({
  route,
  go,
  pages,
  canManagePolicy,
  behind,
}: Readonly<{
  route: Route;
  go: (next: Route, replace?: boolean) => void;
  pages: SystemTab[];
  canManagePolicy: boolean;
  /** Releases this installation is behind, beside Overview. */
  behind: number;
}>) {
  const narrow = useNarrow();
  const draft = usePolicyDraft(pages.includes('policy'), canManagePolicy);
  const dirty = changedPages(draft.changed);
  const tab = pages.find((p) => p === route.tab) ?? pages[0];
  const policyPage: PolicyPage = POLICY_PAGES.find((p) => p === route.sub) ?? 'sources';
  if (!tab) return null;

  const to = (t: SystemTab, sub?: string): Route => ({ view: 'admin', section: 'system', tab: t, ...(sub && { sub }) });
  const href = (t: SystemTab, sub?: string) => format(to(t, sub));
  // Moving between pages replaces the address, as an app's tabs do.
  const open = (t: SystemTab, sub?: string) => go(to(t, sub), true);

  return (
    <PolicyDraftContext.Provider value={draft}>
      <Sheet heading="System">
        <div className={narrow ? undefined : 'pando-system'}>
          {narrow ? (
            <div style={{ marginBottom: 'var(--space-5)' }}>
              <Select
                aria-label="Page"
                value={tab === 'policy' ? `policy/${policyPage}` : tab}
                options={pages.flatMap((p) =>
                  p === 'policy'
                    ? POLICY_PAGES.map((pp) => ({
                        value: `policy/${pp}`,
                        label: `Policy · ${POLICY_LABELS[pp]}${dirty.has(pp) ? ' (unsaved)' : ''}`,
                      }))
                    : [{ value: p, label: p === 'overview' && behind > 0 ? `Overview (${behind})` : LABELS[p] }],
                )}
                onChange={(e) => {
                  const [t, sub] = e.target.value.split('/');
                  open(t as SystemTab, sub);
                }}
              />
            </div>
          ) : (
            <nav aria-label="System" className="pando-settings-nav">
              {pages.map((p) => (
                <SystemLinks
                  key={p}
                  page={p}
                  current={tab}
                  policyPage={policyPage}
                  dirty={dirty}
                  behind={behind}
                  href={href}
                  onOpen={open}
                />
              ))}
            </nav>
          )}

          <div style={{ minWidth: 0 }}>
            {tab === 'overview' && (
              <SystemPage title="Overview">
                <Overview settings={pages.includes('policy') ? <UpdateSettings /> : undefined} />
              </SystemPage>
            )}
            {tab === 'adapters' && (
              <Installation
                query={route.query}
                category={route.sub}
                onCategory={(c) => open('adapters', c)}
                hrefOf={(c) => href('adapters', c)}
              />
            )}
            {tab === 'policy' && <Policy page={policyPage} canEdit={canManagePolicy} />}
            {tab === 'backups' && <Backups />}
            {tab === 'hosts' && <Hosts />}
            <SaveBar />
          </div>
        </div>
      </Sheet>
    </PolicyDraftContext.Provider>
  );
}

/** One page's link, and Policy's pages under it, each marked when it holds
 *  an unsaved change. */
function SystemLinks({
  page,
  current,
  policyPage,
  dirty,
  behind,
  href,
  onOpen,
}: Readonly<{
  page: SystemTab;
  current: SystemTab;
  policyPage: PolicyPage;
  dirty: Set<string>;
  behind: number;
  href: (t: SystemTab, sub?: string) => string;
  onOpen: (t: SystemTab, sub?: string) => void;
}>) {
  const link = (
    <RouteLink
      href={href(page)}
      aria-current={page === current && page !== 'policy' ? 'page' : undefined}
      onFollow={() => onOpen(page)}
    >
      {LABELS[page]}
      {page === 'overview' && behind > 0 && <Badge count={behind} />}
      {page === 'overview' && dirty.has('overview') && <Unsaved />}
    </RouteLink>
  );
  if (page !== 'policy') return link;
  return (
    <>
      {link}
      {POLICY_PAGES.map((pp) => (
        <RouteLink
          key={pp}
          className="pando-system-sub"
          href={href('policy', pp)}
          aria-current={current === 'policy' && policyPage === pp ? 'page' : undefined}
          onFollow={() => onOpen('policy', pp)}
        >
          {POLICY_LABELS[pp]}
          {dirty.has(pp) && <Unsaved />}
        </RouteLink>
      ))}
    </>
  );
}

function Unsaved() {
  return <span role="img" aria-label="Unsaved changes" className="pando-system-dot" />;
}

/** A page of Policy, with drafting a change with AI (R-344) beside its name. */
function Policy({ page, canEdit }: Readonly<{ page: PolicyPage; canEdit: boolean }>) {
  const draftOn = useAIFunctionOn('draft_policy');
  const [drafting, setDrafting] = useState(false);
  const d = usePolicy();
  return (
    <SystemPage
      title={POLICY_LABELS[page]}
      action={canEdit && draftOn && d.changed.length === 0 && <AIButton onClick={() => setDrafting(true)} />}
    >
      {drafting && <PolicyAI onClose={() => setDrafting(false)} />}
      {d.policy.isPending ? (
        <output aria-label="Loading" style={{ display: 'flex', flexDirection: 'column', gap: 'var(--space-3)' }}>
          {[0, 1, 2].map((n) => (
            <LineSkeleton key={n} width="40ch" />
          ))}
        </output>
      ) : (
        <PolicyPageView page={page} />
      )}
    </SystemPage>
  );
}

