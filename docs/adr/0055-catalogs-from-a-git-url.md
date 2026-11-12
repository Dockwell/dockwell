# 0055. Catalogs from a Git URL

- Status: Accepted. Amended by [ADR 0077](0077-dockwells-catalog-in-its-own-repository.md): a source may follow a branch, moved by Dockwell's own change, and holds the images its entries name. What runs still waits for a person.
- Date: 2026-11-08

## Context

[ADR 0029](0029-a-catalog-format-fed-from-other-catalogs.md) feeds the catalog by converting other catalogs where it's built, and the converter writes entries in Dockwell's own format. What it writes from 1Panel's app store is GPL-3.0 and lives outside this repository; what it writes from Dokploy's and Coolify's templates may live in it. Either way, until now an entry reached a server only by being built into the binary, or through `DOCKWELL_CATALOG_DIR`, a folder on the server itself. The PRD's P1 row asks for custom catalogs from a Git URL, and ADR 0029 names it as how someone adds a source of their own.

What a person installs from must be what they chose, and stay so: PRD principle 2 makes every change to managed state a revision with a plan, and a catalog decides what an install runs.

## Decision

### A source is a resource, pinned by commit

- **A catalog source is a resource of kind `catalog.source`, at the organization level,** as a backup destination is. It names a repository by its HTTPS URL, a folder in it, and a commit in full. Adding one, moving it to another commit, and removing it are each a change with a plan, and an undo puts back what was there.
- **It's pinned by commit, never by a branch.** What a server can install from never moves without a person, and a plan that says what changed: the entries a commit adds, changes and takes away. Finding a branch's newest commit, to offer a move to it, is for the CLI and the panel to do, and no part of the source.
- **A source holds entries in Dockwell's format alone,** one `<id>.yaml` each in its folder, as `DOCKWELL_CATALOG_DIR` holds them. Another catalog's format is converted first, where the catalog is built, as ADR 0029 has it, so the control plane still reads no stranger's format.

### What's read is the commit, checked

- **The control plane fetches the one commit over Git's smart HTTP protocol, version 2,** shallow, as a package of its own, `internal/gitfetch`, that the agent build doesn't link. It checks every object it reads against its name, the commit's SHA-1 down to each entry's, so what it reads is that commit, whatever the host sends. It reads a public repository over HTTPS alone, through the one guarded client for addresses people name, `internal/netguard` (#341): a source's address is checked when it's planned and again as each connection is made, whatever its name resolves to, and this server's own addresses, private networks, link-local and metadata addresses are refused. It takes no proxy from the environment, follows no redirect, and no error repeats what a host said. A test passes a client of its own to reach the repository it serves. A private repository, with its credentials as a secret, comes later, as does a source on a private network, which an admin would name, as host access.
- **It reads within bounds,** as a repository is a stranger's: a commit's pack of 64 MiB at most, kept on disk, 2,000 entries, and 1 MiB an entry. A tree's entry named as a path, such as `..` or `a/b`, refuses the commit, and only `<id>.yaml` is read on or kept. A pack holds 100,000 objects at most, and is indexed in one pass, as git index-pack does, into 64 bytes an object. Only what the folder needs is made of its deltas, each object 8 MiB at most, and each object costs 1 KiB against one budget of four times the pack's limit, beside what it inflates to, so a pack of a few hundred bytes, or of millions of empty objects, can't stand for gigabytes. A delta names its base by its offset: a fetch asks for no thin pack, so a delta that names its base by name is refused. The control plane fetches one source's commit at a time, and reads of one commit at once share its fetch.
- **No Git library.** One read of one commit is a small part of Git's protocol, and #292 already speaks its first part, to find what a branch names. A library would bring much more than it's used for into the control plane.
- **What it read is kept,** by commit, in the control plane's data folder, as a commit never changes. A source whose commit's entries are missing, as after a restore, is read again when the catalog next needs it.

### Its entries pass the checks Dockwell's own do

- **Each entry is read as a built-in one is:** `catalog.Parse`, which checks it, and its Compose file read as an install would read it, with stand-ins for what a person types. An entry that fails is left out, and the plan's notes say which and why, so a person sees what a commit brings before they apply it.
- **Dockwell's own entries come first, then each source in its name's order.** An entry whose ID one before it has is left out, saying so, as two of one ID would take over from each other.
- **An install from a source is an install like any other,** through the same plan: host access needs a person, a passkey where it's as good as root.
- **An install upgrades only from where it came from.** Its pin records the catalog it came from, as the control plane knows it: Dockwell's own, at the build's commit, or a source by its name, repository, folder and commit. Never what an entry's own `source` field says, which a source's file writes itself. An upgrade comes from that catalog alone, once a change has moved it to a commit whose entry has a higher release. An entry of the same ID from another catalog is another app, with no upgrade for the install, so a source added later, or one left once another is removed, can't change what an install runs.
- **A source's plan says which commit it is:** which branch or tag of the repository is at it, or that none is now, as a host such as GitHub serves a fork's commits through the repository it was forked from.
- **A person picks a branch, and Dockwell pins its commit.** A source still holds a commit, never a branch. `ListCatalogSourceRefs` lists a repository's branches and tags with the commit each is at, so the panel's form checks the repository as it's typed, offers its branches, and fills in the commit the chosen one is at now. Typing a commit stays possible. Setting, removing and listing refs are an admin's (ADR 0049): a source decides what anyone can install, and each reads the network.
- **A source changes only through its own methods,** `SetCatalogSource` and `RemoveCatalogSource`, which read its commit and write the notes. A plain edit of a `catalog.source` is refused.
- **What's kept is what's applied:** a commit read for a plan is kept apart, the newest four, and kept for good once a source applied names it. One no source names is removed. A source that can't be read is tried again after five minutes, rather than on every view. [The threat model](../security/catalog-sources.md) has the rest.
- **A source's plan says what changes hands:** each ID it takes from another catalog, as it comes first, or that another catalog has in its place, and each install that would have no upgrade after it, or would have one again.
- **A source's entries are its adder's to trust,** as what a person adds is theirs. `ListCatalogSources` says which entries each source shows, for the catalog's page to say where each comes from, and no source's entry is *tested* until it's tested as ADR 0029 says.
- **The CLI comes from the schema,** as every command does: `dockwell catalog set-source`, `dockwell catalog sources` and `dockwell catalog remove-source`, each a plan to apply, as an install is.

## What we turned down

- **A Git library, such as go-git.** It would read one commit with much more code, and its own dependencies, in a service that runs as root.
- **Archives from a host's own HTTP,** such as GitHub's tarballs. They're each host's own, and nothing in one says it's the commit asked for.
- **Following a branch.** What a person could install would change under them with no plan.
- **Reading another catalog's format from a source.** ADR 0029 turned that down for the control plane, and a source is no reason to take it up.

## Consequences

- Converted entries reach a server from whichever repository holds them, through a plan.
- The control plane fetches from the network on a person's change, so a source a host can't serve can't be added, and the plan says why.
- To do: private repositories, with credentials as a secret; offering a move to a branch's newest commit, once #292's `gitref` is in; the panel's own page for sources; and the reviewed list of ADR 0029, to show one app once whichever sources list it.
