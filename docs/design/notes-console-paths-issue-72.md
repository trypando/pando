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
