# 0051. Files that merge without conflicts

- Status: Accepted
- Date: 2026-11-07

## Context

About ten tasks keep pull requests open at once, and each one waits hours for CI ([ADR 0050](0050-ci-runs-what-a-change-needs.md)). A pull request that conflicts with main can't merge until it's rebased, and a rebase is a push, so it waits for CI again. On 2026-11-07, #200, #220 and #230 merged, and #215, #222 and #218 conflicted twice. The conflicts weren't in what they changed. They were in three files every pull request adds to:

- **The message catalogs**, `web/src/locales/{en,zh-CN}/messages.po`, in alphabetical order. A pull request's new messages land among everyone else's: "Adding up what takes the disk", from a server's page, went right before "Address", from Settings. And each message's `#:` lines changed whenever another file used it, so two pull requests that each used "Cancel" somewhere new changed the same lines.
- **The index of decision records**, a table in `docs/adr/README.md`, which every pull request with a record added a row to, at the end.
- **The panel's end-to-end runner**, `web/e2e/sign-in.ts`, which imported and called each flow, and listed them in its header. Every pull request with a flow added an import, a call and a line there, each beside the last one.

Replaying #215 on main as it was before #230, and merging #230, conflicted in 4 files, in 10 places: 3 in each catalog, 3 in the runner and 1 in the index. 27 of the open pull requests changed the catalogs, 15 the index, and 10 the runner.

## Decision

### Messages in the order of the files that use them

The catalogs stay one file per locale. Their messages are ordered by the first file that uses each one, by path, and then by their text (`web/lingui.config.ts`), and they carry no `#:` lines. A feature's messages sit together, so pull requests in different features add to different parts of the file, and git merges them. A message's position no longer moves with every new use, and nothing else about it changes when another file uses it.

The obvious way, a catalog per feature with Lingui's `{name}` catalogs, does as well across features, and was measured against this. Replaying #215, #222 and #218 against #230, and against #230 and #217, removed every catalog conflict either way. But a catalog per feature duplicates the 172 messages more than one feature uses, which can then drift apart in translation, and the panel would load 16 files per language instead of one. One file in file order needs neither.

Messages in alphabetical order without `#:` lines, and a catalog per feature in alphabetical order, kept conflicts with #230 in every pair. A translator now finds where a message is used with a search of `web/src`. The comments Lingui writes about placeholders stay.

A git merge of two catalogs in this order is usually what `lingui extract` writes for the merged code, but not always, even when git merges them without a conflict. Two pull requests that each add one message, in different features, with different translations, leave it in twice. And when one side stops using a message while the other uses it in a file that sorts later, git keeps the first side's removal, and the message and its translation are lost. `make i18n` then brings it back untranslated.

So after any merge, rebase or cherry-pick that touches the catalogs, conflicted or not, `make i18n-merge` merges them again from the three versions: what both sides started from, and each side. It keeps every message either side has, with the translation a side changed, a cleared one included. It merges the header a field at a time, with its date as the catalog had it, so two branches merged at different times don't differ there. Then it runs `lingui extract`, and fills what that left untranslated from either side. It stops, and says which, when both sides translated one message differently, or when one side already holds a message twice with two translations, as it does after a rebase that git finished on its own. It finds the three versions itself while a merge, rebase or cherry-pick is stopped, or with a merge commit checked out. Git can leave `REBASE_HEAD` behind after a rebase that stopped and then finished, so that counts only while a rebase is under way. After a rebase that finished, `make i18n-merge FROM=ORIG_HEAD` names the branch as it was, and after a cherry-pick, `FROM=` names the commit picked; `FROM` wins over anything git says is going on.

### The records are their own index

`docs/adr/README.md` has no table. The records are the numbered files, and each one's Status line says what changed it since. `make adrs` lists their numbers, titles and statuses from the files.

### Each flow says where it runs

Each `web/e2e/*-flow.ts` exports a `flow`: its order among the others and how to run it, with what it needs from what `sign-in.ts` gives every flow. `web/e2e/flows.ts` runs every one it finds in the folder, in order. A new flow is a new file, and `sign-in.ts` doesn't change. Flows with the same order run by file name, so two pull requests that pick the same number don't conflict either. The flows run in this order now: apps 10, plan 20, health 30, catalog 40, backups 50, domains 60, containers 70, network 80, settings 90.

`web/e2e` and `web/scripts` are type-checked with the panel (`web/tsconfig.node.json`), so a flow's `run` that leaves out an option its flow needs fails CI, rather than skipping a check at run time.

## Consequences

- Replayed on tonight's merges, #215 and #222 merge cleanly with #230, and with #230 and #217, where each conflicted in 4 files before. #218 still conflicts with both, but only where it and #230 change the same code: the event schema and what's generated from it, the route tree, and a test helper.
- Pull requests in one feature, and stacked ones, still conflict where they add to the same place, though in fewer places.
- This change conflicts once with each open pull request that touches one of the three files, as it reorders the catalogs and moves every flow's call. CONTRIBUTING.md says how to rebase one: `make i18n-merge` for the catalogs; for the runner, take main's `sign-in.ts`, keep the pull request's other edits to it, move its flow's call into a `flow` export with the call's options, and pick an order between its neighbours; for the index, drop the row. The open pull requests' flows fit as databases 55, websites 65, a server's overview 85, the layout 85 and processes 95.
- A flow that exports no `flow` fails the run, saying so, rather than being left out.
- The route tree, `web/src/routeTree.gen.ts`, is generated by TanStack Router from the routes and still conflicts when two pull requests add routes beside each other. That's left for when it costs a round.
