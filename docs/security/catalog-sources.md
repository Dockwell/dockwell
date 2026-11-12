# Catalog sources: threat model

What a catalog source can do to the control plane and to what's installed, and what stops it. The design is in [ADR 0055](../adr/0055-catalogs-from-a-git-url.md), and for another store's catalog, which the control plane converts as it reads it, in [ADR 0078](../adr/0078-other-stores-read-on-the-server.md); the code is in `internal/gitfetch`, `internal/netguard`, `internal/catalog/convert` and `internal/control/catalogapi`.

## What there is to protect

| Asset | Where it lives | Why it matters |
| --- | --- | --- |
| The control plane's disk and memory | The server it runs on, as root | A source is read when a plan is made, before anyone applies anything |
| What the control plane can reach on its network | This server, its private networks and its cloud's metadata service | A request it makes for a source could reach what no person can |
| What an install runs | Its project's pin, and the catalog it upgrades from | An upgrade runs whatever image the entry it comes from names |

## Trust boundaries

- **A source's repository, and the host that serves it,** are a stranger's, whoever named them. Everything it sends is checked as if it means harm: its pack, its trees' names, its entries and its words.
- **An admin** can add, move or remove a source. Adding one reads the network and the disk when the plan is made, outside the revision store, and decides what anyone can install, so `SetCatalogSource` and `RemoveCatalogSource` need an admin's role on the install, checked before anything is read (ADR 0049). The revision engine holds the resource to an admin too, as it belongs to no project. Anyone with a role can list the sources. `ListCatalogSourceRefs`, which a form uses to check a repository as it's typed and to offer its branches, is an admin's too: it reads the network for whoever asks, through the same guarded client, with the same refusals, and answers with names and commit IDs alone.
- **Whoever applies the plan** chooses to trust the source's entries, as anything they add is theirs. A source's entry is never *tested*, as ADR 0029 says of the reviewed list.

## Threats and mitigations

| Threat | Mitigation |
| --- | --- |
| A tree names an entry `../../root/.ssh/authorized_keys`, and it's written there as root | gitfetch refuses a tree with an entry named empty, `.`, `..`, `.git` in any case, with a slash, a backslash or NUL in it, or twice, as Git does. Only `<id>.yaml` is read on or kept, written through `os.Root`, in a folder of each read's own. |
| A pack of a few hundred bytes stands for gigabytes: a delta that copies its base thousands of times, a chain made again for each delta on it, or a zlib bomb | The pack is kept on disk, 64 MiB at most, and indexed in one pass that only hashes what it inflates. Only the commit, the trees on the path and the folder's blobs are made of their deltas, each 8 MiB at most, what's made is kept within 32 MiB, and everything inflated or made counts against one budget of four times the pack's limit. The read stops with its context, a minute at most. |
| A pack of millions of tiny or empty objects, a few hundred KiB gzipped, costs the index gigabytes | A pack holds 100,000 objects at most, by its header's count, refused before anything is read. Each costs 1 KiB against the budget, whatever it holds, and the index keeps 64 bytes of it, in one slice: the most a pack may hold indexes in under 8 MiB. |
| Deltas that name their bases by name, each after the one it's made from, take a pass over the pack for each | A delta names its base by its offset, before it. A fetch asks for ofs-delta and no thin pack, so a delta that names its base by name never comes from an honest host, and is refused. |
| Many reads at once add up | The control plane fetches one source's commit at a time, and reads of one commit at once share its fetch. |
| A source's URL, or what its name resolves to, leads to loopback, a private network or a metadata service | It's read through `internal/netguard`, the one guarded client for addresses people name (#341): the address is checked when the plan is made and again as each connection is made, with no proxy from the environment. HTTPS alone. |
| A redirect leads elsewhere | None is followed, whatever the client: a host that moved says so in its answer, which names its status alone. |
| A host's words reach a person: an `ERR` line telling them to run a command | No error repeats what a host sent. Each names the host and a status, or says what was wrong with what it sent. |
| A host serves a commit other than the one named, or objects that aren't what their names say | Every object is named by its SHA-1, with the collision detection Git's own SHA-1DC has, from the commit down, and the pack's checksum is checked. A repository that names its objects by SHA-256 is refused, saying so. |
| A commit a fork made is served through the repository it was forked from, as GitHub does | The plan says which branch or tag of the repository the commit is, or that none is now, and that it may be a fork's. |
| An entry names the logo and screenshots Dockwell ships for one of its own apps, to pass for it | An entry's artwork is a file of the panel's, never a URL, so browsing asks nothing of another host. A source's entry is shown without any: the catalog drops its icon and screenshots, and the panel says which catalog it comes from on its card, its page and its install form. Its links open only as HTTPS addresses with no sign-in details. |
| An entry runs an image that moves | Each service's image, as an install reads the file, must name its digest. A check of lines alone missed a flow mapping and a quoted key. |
| A source added later, or one left when another goes, upgrades an install to a stranger's image | An install's pin records the catalog it came from, as the control plane knows it, never as the entry says. It upgrades only from there, and a source's plan names each ID that changes hands and each install that loses or regains its upgrades. |
| A source is added by a plain edit, with no read and no notes | A `catalog.source` changes only through `SetCatalogSource` and `RemoveCatalogSource`. |
| Plans fill the disk | A commit read for a plan is kept apart, the newest four, and kept for good only once a source applied names it. One no source names is removed. A commit's files are 16 MiB at most, 2,000 of them, 1 MiB each. |
| A host that's down makes every view of the catalog wait | A source that couldn't be read is taken as unread for five minutes, and the catalog shows the rest. |

### Another store's catalog, converted on the server

A source may be 1Panel's app store, Dokploy's templates or Coolify's (ADR 0078). The control plane then parses YAML, TOML and JSON a stranger wrote, and once a source follows a branch, it does so with nobody watching.

| Threat | Mitigation |
| --- | --- |
| A store's commit is far larger than a source reads, as 1Panel's is 152 MiB of archives and databases | It's fetched in two rounds: its folders first, with `filter blob:limit=1`, in a pack of 16 MiB at most, then only the files its adapter reads, by their objects' names: 5,000 at most, 1 MiB each, 32 MiB together, in a pack of 64 MiB at most. A walk lists 20,000 files and folders at most, 8 deep. Every object is checked against its name, as above. A host that can't filter is fetched whole, within 64 MiB. |
| A host that says it filters sends every file anyway | The first round's pack is held to 16 MiB, and refused past it. |
| A file nests thousands of levels deep, to recurse a parser out of its stack | Each file read of a store is 256 KiB at most, and nests 64 levels at most, counted from its brackets and its indentation before any parser reads it. YAML's own parser stops at 10,000. |
| YAML aliases of aliases stand for millions of nodes | Expanding a file's aliases makes 100,000 nodes at most. |
| A template asks Dockwell to generate a secret of a billion characters, which an install would allocate | A secret any entry generates is 256 characters at most, checked for every source's entries, Dockwell's own format's too. A template's helper that asks for more leaves its app out. |
| A store's app names an image on this server's registry, a private network's or a metadata address, so pinning it at a plan probes the network | It's left out when it's converted: an image's registry may not be one netguard refuses. |
| A store holds thousands of apps, to take the control plane's time | 2,000 at most, counted before the adapter lists them. Converting a commit takes a minute at most, checked between apps, one commit at a time, once per commit and converter. |
| A store's app runs a script on the server as it installs | Nothing a store holds is ever run. An app with scripts, a build or shipped files is left out, with the reason. |
| An entry hides an image from the check of lines, in a flow mapping or a quoted key | Each service's image, as an install reads the file, must be one the file names plainly on an `image:` line by a version's tag, or be pinned by digest. A moving tag, such as `latest`, is refused. |
| A tag is pushed again upstream, to change what an install runs | An install pins each image by digest when it's planned, through the agent of its server, and the plan shows it. An upgrade keeps the digest an install runs for an image whose tag didn't change, and a companion keeps every digest, so only a plan that moves a tag moves what runs. |
| Pinning at a plan asks registries too often | Only a plan asks, never a refresh: a HEAD each, which no registry counts as a pull, 20 images a call, two calls reaching registries at once on a server, and digests kept 10 minutes, shared with a form's reads. |
| A store says it's under one licence and isn't | The licence is read from the repository's own licence file and matched to four licences' texts; anything else is `NOASSERTION`, and the plan says Dockwell can't tell it. |
| A store's words reach a person as markup, or as an instruction | An entry's summary is the store's line, one line of 160 characters at most, shown as text. The converter writes no first steps. |

## Not covered

- **A private repository,** with credentials as a secret, and a source on a private network, which an admin would name, as host access, come later.
- **What a source's entries say in words,** such as a summary, is shown as written. The panel shows it as text, never as markup.
- **A store's images aren't held back 14 days,** as Dockwell's own catalog's are: the store chose that version, and a tag rebuilt every few weeks would never be installable. The plan shows the digest it installs (ADR 0078).
