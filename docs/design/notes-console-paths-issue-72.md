# Console and API paths at install scale (issue #72)

The target is one install with about 100,000 accounts, 20,000 apps and 5,000 people using the
console at once. At that size a request has to cost what its answer costs, not what the install
holds: a page of a list reads a page of rows, a search reads its matches, and a background pass
does not read every app because one of them might need something. This note lists what was found
by reading the code against that target, what each finding became, and which part of the change
carries it out. The change lands as one pull request assembled from several parts.

The owner decided four questions the findings raised, recorded as resolved in
`docs/plan/open-decisions.md`:

- **O-51** — the Logs tab: one shared live log stream per app part (workload), fanned out to every
  open viewer over server-sent events, so the runtime's load does not grow with viewers.
- **O-52** — the reconciler reacts to runtime events (Docker events, Kubernetes watches) for
  crashes at once, and sweeps settled apps on a slow interval as a backstop.
- **O-53** — list totals are exact up to 10,000, then reported as a lower bound ("10,000+").
- **O-54** — the access assistant looks people, apps and groups up through search tools instead
  of being handed whole tables.

## Database and API paths (the server-paths part)

Each was checked against the code before it was changed. Each has a test that fails against the
query it replaced.

| Finding | What it became | Test |
|---|---|---|
| `GET /me/apps` returned every app the caller can open — every app shared with everyone goes to every person — and the console then asked `GET /apps?id=…` about every tile, 100 IDs at a time, to learn which it could manage | Keyset-paged (`limit`, `cursor`, `q`). The page is chosen before routing, icons or anything else is read. Each app carries `can_manage`, the control plane's answer (what `GET /apps` would list), beside the data plane's list — never mixed into it (R-070, R-071). The launcher reads it and pages with "Show more apps" | `TestR264_TheLauncherPagesFavoritesFirstAndSaysWhatYouCanManage`, `TestR264_TheLauncherSaysWhichAppsYouCanAlsoManage` |
| `GET /users/{id}/apps` and `GET /groups/{id}/apps` read every grant reaching the principal, then asked the authorizer two questions per app, each reading the caller's grants again | One query per page of apps: the apps are chosen by name first and their grants read for that page alone. What the caller may do is asked once for the page (`authz.AllowsEach`, with the store's `ControlGrantsForApps`), by the same rules as `Allows` | `TestR071_AnAccountsAppsArePagedByAppWithEveryGrantOnEach`, `TestR081_AllowsEachAsksOnceForEveryAppAndAnswersAsAllowsDoes`, `TestR071_SharedListsArePaged` |
| `GET /apps/{id}/grants` returned every grant on the app; an app shared with every person by name holds a grant per person | Keyset-paged by principal (`grants_app_list_idx`), so the grant to everyone is on the first page and one principal's grants on both planes arrive together | `TestR075_AnAppsGrantsArePagedByPrincipalWithEveryoneFirst` |
| `GET /subscriptions` was unpaged, and its filters were written `$1 = '' OR …`, which a cached generic plan cannot serve from an index | Each narrowing written only when asked for; keyset on `(created_at, id)` with an index per narrowing (000058) | `TestSubscriptionsAndBackupsArePaged` |
| `GET /backups` returned every backup with its manifest, and one backup attempt per app | Keyset-paged on `(created_at, id)`; the first page carries the most recent attempts, at most a page of them | `TestSubscriptionsAndBackupsArePaged` |
| Every list's `total` was a `count(*)` of every match, with a leading-wildcard `ILIKE` no index could serve | Counted over a `LIMIT 10001` subquery (O-53): exact to 10,000, then `total_is_lower_bound`. Searches match one expression per table — username, display name and email, or name and slug, joined by `chr(31)` — served by a `pg_trgm` GIN index on exactly that expression (000055). Also `Users.Search` and `Groups.Search` | `TestO53_AListTotalIsExactUpToTheCapThenALowerBound`, `TestO53_AListPastTheCapSaysItsTotalIsALowerBound`, `TestSubstringSearchesAreServedByTrigramIndexes` |
| `Groups.ByID` built every member into an array, and four handlers called it only to learn the group exists | `Groups.Exists` for the existence checks; `GET /groups/{id}` carries `member_count`; `GET /groups/{id}/members` pages the members, with `q` and `id` to ask about particular people. The console's group editor reads members a page at a time and saves who was added and who removed | `TestR079_AGroupIsReadWithoutItsMembers`, `TestTheAccountsGroupsAndAppsListsPage` |
| The R-088 count of people who can manage accounts ran over every account with an OR'd correlated subquery, before and after every SSO sign-in's group sync and every SCIM change | Counted from the few install grants that carry `install.users.manage`, and taken only when the change can affect who manages: a person who manages now, or a group (or a provider's group linked to one) holding that verb. An ordinary person's sign-in takes no count | `TestR088_ManagersAreCountedOnlyWhenAChangeCanAffectThem`, plus the existing R-088 tests |
| Spec revision pruning was one `DELETE` over every revision in the install with a correlated `max()` | One statement per 500 apps in ID order, removing at most 100 revisions from each, oldest first; a longer backlog goes over later passes. Never a revision that was ever pinned, that a deploy refers to or that a scan describes (R-152) | `TestR152_PruningGoesAppByAppInBatchesAndKeepsWhatRollbackNeeds` |
| The auto-deploy poll read the pinned revision of every live app every minute to look inside its JSON | `apps.auto_deploy`, derived from the pinned revision by a trigger whenever the pin moves and overriding any other write, so it cannot change without a revision; a partial index serves the poll (000056, R-141) | `TestR141_AutoDeployIsReadFromThePinnedRevision` |
| The audit list's `involving`, `on_behalf_of` and `target_id` filters had no index, and the indexes it had were on `(column, occurred_at)` while the list reads by `id DESC` | Indexes on `(column, id DESC)` for app, actor, on-behalf-of and target, and `action text_pattern_ops` for prefixes, made on the partitioned parent so every month's partition has them (000057). The application role still holds no `UPDATE` or `DELETE` on any of it (R-027) | `TestR027_TheAuditLogsFiltersAreServedByIndexesInItsOrder` |
| Retention of old deploys numbered every deployment in the install with a window function on every batch | Walks apps through `deployments_app_idx` past each app's newest N and stops at the batch size; the same rows are kept | the existing retention tests |
| `GET /reference` rebuilt and re-encoded the whole document on every request | Built once, on first use | `TestTheReferenceIsBuiltOnceAndServedWhole` |

**Checked and left as it was:** `PortListeners.sync` reads every allocated host port every 15
seconds on every replica. Port allocations are bounded by the host's port range, port mode is the
loopback adapter's, and the read is one indexed query of integers; it does not grow with anything
the target install has more of.

### Defaults chosen [P]

- **Launcher order:** favorites, then apps filed in one of the person's sections, then the rest,
  each by name. So the first page holds what the person arranged, and sections are complete once
  the pages covering them are read. Sections themselves come with every page.
- **A group's members** are listed oldest account first, in `group_members`' primary-key order,
  so a page reads a page of rows however large the group.
- **An account's or a group's apps** are paged by app, not by grant: every grant on an app is on
  the page that has the app. For somebody else's account, apps the caller cannot see are left out
  after the page is chosen, so a page can hold fewer apps than the limit.
- **Backup attempts** come with the first page of `GET /backups` only, the most recent at most a
  page of them.
- **Pruning batch:** 500 apps a statement, 100 revisions per app per pass.
- **Search expression:** fields joined by `chr(31)`, the ASCII unit separator, so a search cannot
  match across two fields while still being one indexed expression.

## The other parts of the change

| Area | Covered by |
|---|---|
| A shared live log stream per workload, fanned out over server-sent events (O-51) | the log-stream part |
| The reconciler reacting to runtime events — Docker events, Kubernetes watches — with a slow sweep as backstop (O-52) | the reconciler-events part |
| Capacity, the policy preview and the background passes reading what changed rather than every app | the background-costs part (migrations 000060–000061) |
| The access assistant's search tools for people, apps and groups (O-54) | the access-assistant part |
| Pinning Docker Hub images with a `HEAD` request, and readable rate-limit errors | the registry part |
| Console polling: how often each screen asks, and only while it is visible | the console-polling part |

## The console's polling budget (the console-polling part)

What one open console asks the server for while it sits on a screen, after the change. A poll
stops while the browser tab is hidden (React Query runs no `refetchInterval` in the background),
and every interval lives in `console/src/ui/polling.ts` so the budget can be read in one place.

| Where | Asks for | How often | Before |
|---|---|---|---|
| Every screen, signed in | `GET /me/notifications/unread`, the count alone (R-377); the list is read when the bell opens | 60 s | `GET /me/notifications`, a page of the inbox with its count, every 60 s |
| An app's screen, any tab | `GET /apps/{id}/status`, one query shared by the parts table, the log's part picker, the deploy settings and the screen | 3 s while deploying; 5 s while degraded or a part is restarting, stopped, missing or unhealthy; 30 s once settled | 5 s, on Overview and Logs only, whatever the state |
| | the app's record, its deploys and its security report | once, when the status says the app's state moved on (`useRecordFollowsStatus`) | the record never; a finished deploy showed when something else invalidated it |
| Overview | `GET /apps/{id}/deployments` | 30 s while a deploy waits for approval, otherwise never | 3 s while deploying, 15 s while awaiting approval |
| | `GET /apps/{id}/usage` | 30 s, only while the section is on screen | 10 s |
| | `GET /apps/{id}/security` | 5 s while scanning, 10 s while deploying, only while on screen | 2 s while scanning or deploying |
| Logs | `GET /apps/{id}/deployments` | as Overview; the deploy log and the app's output stream (O-51) | 3 s while deploying |
| Approvals, and the sidebar's count | the first page of `GET /approvals`, merged into the pages loaded | 60 s | every loaded page, every 60 s |
| An app's Events tab | the first page of `GET /apps/{id}/events`, merged | 15 s | every loaded page, every 15 s |
| A subscription's dialog | `GET /subscriptions/{id}/deliveries` | 3 s while a delivery awaits its first attempt; when the next retry is due, 3 to 60 s apart; never once none is pending | 3 s while any was pending, through hours of retries |
| Admin sidebar | `GET /apps?limit=1` and `GET /users?limit=1`, for `total` (exact to 10,000, O-53) | kept 5 minutes; a change made in the console asks at once | kept 10 s, and read again on focus |

At 5,000 consoles open on settled apps' overviews this is about 5,000 × (1/60 + 1/30 + 1/30) ≈ 420
requests a second, where it was about 5,000 × (1/60 + 1/5 + 1/10) ≈ 1,580. A console on any other
screen asks once a minute. Unchanged, and bounded by what one person is doing: the apps list asks
about the rows that are deploying or being scanned, by ID, every 5 s (at most 100); detection asks
every 500 ms while it runs, for the app being added; Capacity refreshes at the interval the server
gives; Updates asks hourly, and every 5 s while an upgrade runs.

**Refetch on focus is off by default [P].** On, React Query read every query on the screen again
(a dozen on an app's screen) each time the window took focus, in every open console. It is on for
the queries a person returning to the tab looks at, each one row or one page: who is signed in
(`GET /me`), the inbox count, the app's status, and the first page of approvals and of an app's
events. A screen still reads its queries again when it is opened, once they are older than the
10-second `staleTime`.

**Other changes in the same part.**

- **Pickers and names look accounts up instead of reading them all.** The audit log's names, an
  account's activity and the access assistant's people picker read the newest 500 accounts and
  looked everything up in that. They now ask `GET /users?id=…` about the account IDs on screen (100
  to a request) and `GET /users?q=…&limit=8` for what is typed.
- **A change invalidates what it changed.** Deploy, accept, approve, start, stop, restart, rename,
  image and add-app invalidated every `['apps', …]` query: every app's record and every list in the
  cache. They now invalidate the app's own queries and the app lists (`invalidateApp` and
  `invalidateAppLists` in `console/src/admin/appList.ts`).
- **Paged lists poll their first page [P].** A list with "Show more" polls its first page and
  merges it (`console/src/ui/headPoll.ts`). With one page loaded, the fresh page replaces it. With
  more, a full fresh page leads and the old first page's rows it pushed down follow; a short one
  ends the list there; later pages drop what the fresh page holds. A request answered on a later
  page leaves the list when the screen is next opened.
