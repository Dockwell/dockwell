# 0076. Optional companions in a catalog entry

- Status: Accepted
- Date: 2026-11-11

## Context

The catalog's PostgreSQL installs Adminer beside the database, and its MySQL and MariaDB install phpMyAdmin, whether or not anyone wants them. The install sheet asks for the tool's port first, shows a note about it, and lists it among what it installs, with no way to leave it out. A review of the sheet on 2026-11-11 asked for exactly that: make these tools optional, so a person installs one only if they use it (#519).

An entry's Compose file is one file, and an install is that file read with its inputs ([ADR 0029](0029-a-catalog-format-fed-from-other-catalogs.md)). Leaving a service out has to stay a plan like any other change, survive an upgrade, which merges the entry's old file, its new one and what runs, and be undone.

## Decision

### An entry marks a service as a companion

`Entry.companions` lists services the app runs without. Each has a label ("Adminer"), what it's for in a few words ("Browse and edit tables in your browser"), whether it's on unless a person turns it off, the inputs it alone uses, and notes for while it's on. The format is Apache-2.0, as the rest of the entry ([ADR 0012](0012-apache-for-api-schema-and-clients.md)), and an entry without the field installs every service, as before.

`catalog.Check` refuses a companion the app can't run without, or that says what the entry hasn't: a service the file doesn't run, one of the entry's databases, one another service depends on, a list of companions that leaves nothing to install, an input it names that another service uses too, and an input only it uses that it doesn't name. A companion's inputs can't be secrets, as turning one on later reads the install's file again with stand-ins for its secrets, which can't make a new one. Its purpose is one line of 60 characters at most, and each note one line of 120, as an entry's.

### The install records the choice, and everything after keeps it

- **Installing.** `InstallCatalogEntryRequest.companions` says which to install, by service, and one left out is as its entry says. The control plane reads the file without the companions left out, and without the volumes only they used, and asks for no input only they use; a value given for one is refused, naming the companion. The plan's first steps leave out how to sign in to a tool that isn't there.
- **The pin.** `CatalogPin.companions` records the choice for every companion, in the same revision as the install, and keeps the entry's whole file, so the choice can change later at the same release.
- **Upgrading.** The old file and the new one are both read with the install's choice, so what the install left out stays out and what it ran stays. A companion with no record is on when the file the install ran had its service, as an install from before companions ran every service, and otherwise as its entry says, as for one a newer release adds.
- **Adding or taking one away later.** `CatalogService.SetCatalogCompanion` plans it at the release the install is on, as a three-way merge of the file as it runs now and as it would, the same merge an upgrade makes, with the pin's new choice in the same change, so an undo puts both back. Adding one publishes it on a free port unless a value is given. Taking one away keeps any volume it had, as an upgrade does, and takes no backup, as no data moves. It's a developer's, as an upgrade is ([ADR 0049](0049-roles-and-invites-by-project.md)).
- **What runs reads as it is.** `CatalogInstall.companions` says whether each runs, and the panel's install sheet, app page and database page follow it.

### Adminer and phpMyAdmin are off unless a person turns them on

PostgreSQL's Adminer and MySQL's and MariaDB's phpMyAdmin are companions, off by default:

- **Most databases here are an app's store.** A database installed from the catalog usually backs an app, and its page already gives what that needs: how to connect, its users and its backups. A browser of tables is for the person who wants to look inside, and they turn it on.
- **1Panel installs each database alone,** with phpMyAdmin and Adminer as apps of their own in its store, so people moving off it expect the database by itself.
- **A tool left out is one less sign-in page.** Each answers on a port with the database's root password, and each costs memory on a small server. Off by default keeps both to those who use them.
- **One step away either way.** The install sheet shows each as a switch with its port and notes beside it once on, and the database's page adds or takes it away later as a plan.

Installs made before this keep their tool through upgrades, by the rule for a companion with no record. Each entry's release goes up, and its file is the same, so nobody is offered an upgrade for it.

### What we turned down

- **Separate entries for the tools, linked from the database.** Adminer is in the catalog on its own already, for a database it doesn't run. Beside a database it reaches that database by name on the project's network and signs in with its secret, which a separate install in another project can't, so the companion stays in the database's entry.
- **Deriving which inputs belong to a companion.** The file says it, but an entry that names them says what a form asks for in one place a person reads, and the check holds the two to each other.
- **A companion's secret.** It would need generating when a person turns the companion on later, outside the install that made the others. No tool here needs one, so the format refuses it until one does.

## Consequences

- A new install of PostgreSQL, MySQL or MariaDB runs the database alone, and lands on its page, with how to connect.
- The Docker checks that sign in through Adminer and phpMyAdmin turn them on, with `--companions adminer=true`.
- An entry from another catalog can mark companions the same way, and one that doesn't installs as before.
