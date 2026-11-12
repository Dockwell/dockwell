# 0074. Projects you can see, and that go with their last app

- Status: Proposed. Amends [ADR 0036](0036-deleting-a-project-takes-its-environments.md), where it says what the panel's plan to delete a project takes, and the PRD's progressive-disclosure row "Project in the scope bar".
- Date: 2026-11-12

## Context

A review of the panel on a real server on 2026-11-11 (#520) asked what a project is, whether anyone can manage one, and why what an app used stays behind once the app is deleted. What's true today:

- **A project holds what goes together.** [ADR 0014](0014-object-model-and-store.md) puts environments in a project, and services, routes, volumes, networks, variables, secrets, tasks, backup policies and database users in an environment.
- **Projects come without anyone asking.** Each Compose import, catalog install and website makes a project of its own ([ADR 0026](0026-the-app-screens.md), [0029](0029-a-catalog-format-fed-from-other-catalogs.md), [0039](0039-websites-as-projects-of-their-own.md)). An install with a few apps soon has many projects, and the word appears only in refusals, such as "A project named mysql is here already".
- **The PRD's answer was never built.** Its progressive-disclosure table hides the default project while it's the only one, and puts "Project in the scope bar" from the second. The design system has a `ScopeBar` (organization / project / environment) for the top bar, and nothing uses it.
- **Deleting an app leaves its project.** `useDeleteApp` plans deleting the service and its routes. The project stays, with its variables, secrets, volumes and networks, and nothing in the panel lists it afterwards.
- **The panel's way to delete a project is refused for most projects.** The command bar's "Delete a project" (ADR 0036) adds the project's volumes and networks to its plan, and nothing else. A catalog install always has a sealed secret, so the engine refuses its project, naming the secret.
- **Variables don't scale.** `/variables` isn't in the rail: More on a phone, the command bar and an app's Environment tab reach it. Its switch is one segmented control with every environment as "project / environment", six wide after a few installs, leftovers among them.

How others do it:

- **Railway** makes projects the top of its dashboard, each a canvas of its services. Shared variables live in a project's settings, per environment, and a service uses one as `${{shared.NAME}}`. A project is deleted from its settings, with its name typed.
- **Coolify** has projects of environments of resources, and shared variables at team, project and environment level (`{{project.NAME}}`). Deleting a resource offers checkboxes for its volumes, networks and configuration. A project deletes only once it's empty; that check missed some kinds until 2024, and projects went with their resources left running.
- **Dokploy** has projects with environments, and project variables every service inherits, environment and service ones overriding them. Deleting a project has had the same gap of what's left behind.
- **1Panel** has no projects: each app is an install of its own, values are set per app, and uninstalling offers to delete its backups.

None of them shows what's left in a project, and in each, deleting an app and deleting what it used are separate errands.

## Decision

### A project is what's in it, and it shows as that

A project is an app or a site and everything that goes with it. The panel never explains that in a sentence. It shows it: wherever a project appears, it shows what's in it, by kind and count, such as "2 apps · 1 domain · 3 variables · 2 secrets · 1 volume", and what's in it links to its project.

- **What counts:** apps, databases, websites, domains, variables, secrets, volumes and tasks, each only when there's one. Networks, backup policies and database users go with what uses them, so they count in a plan, not on a card.
- **A project with nothing running is marked.** One that holds no service and no route reads "Nothing running", with what's left, such as "1 secret · 1 volume", and Delete beside it.
- **The default project stays hidden while it's the only one** (ADR 0014). Apps made from an image or Git with no project chosen go in it, as ADR 0026 says. It's never offered for deletion with its last app, as it's where the next one goes.

### Not a scope bar: a property, and a page

The PRD's row said a project would be chosen in a scope bar, as Railway chooses one. Dockwell's lists are install-wide: Apps, Databases, Websites and Domains list everything, and a server is a property of each, not a place you go first (PRD, scaling principle 1). A project switcher would put the whole panel in a mode, hide most apps behind it, and make "where did my app go" a question. So a project is a property and a destination, as a server is:

- **A property.** An app or a database shows its project in its breadcrumb, as a link, once there are two projects. A website is its project, so the list leads to it. Lists group or filter by project where they do now.
- **A destination.** A Projects list and a page for each project, in the rail once there are two (never a choice with one option). The list shows every project with what's in it, marks the ones with nothing running, and puts Delete right there. The project page has what's in it, its domains, its variables and secrets, its environments, who has access ([ADR 0049](0049-roles-and-invites-by-project.md)), and Settings: its display name, and Delete.

The PRD's row becomes "Projects in the rail, a list and a page each; what's in a project links to it". `ScopeBar` stays in the design system, unused, until the Organization edition's organizations need a switcher.

### Deleting an app takes its project when nothing else is left

When the app is the last thing in its project, the plan to delete it offers the project too, on by default.

- **The last thing** means no other service and no other route in any of its environments. What's left once it goes is what it used: variables, secrets, volumes, networks, tasks, backup policies and database users.
- **The plan sheet has a switch, "Also delete the project <name>",** above the plan, on. On, the plan deletes the app, its routes, everything left in the project, and the project. Off, it deletes the app and its routes, as today. Flipping it plans the other one in place, and lets go of the first. The plan lists each item that goes, as any plan does, so the switch needs no words of its own beyond what it deletes.
- **A volume going needs the typed confirmation, and is kept 7 days,** as ADR 0036 has it, so Undo brings back the app, its project and its data together.
- **Deleting a project is an admin's** (ADR 0049). Below an admin the switch is off, with why ([ADR 0059](0059-what-the-panel-knows-of-what-you-may-do.md)), and the plan deletes the app alone.
- **One service of several keeps the project,** as a stack's other services still use it. Its Settings say so, and offer to delete the whole project instead, which opens that plan.
- **Websites already go with their project** (`DeleteWebsite`, ADR 0039).
- **A database deletes from its own page too,** with the same switch. The users Dockwell made in it go with it, as nothing else can hold them, and its volume stays, with every database and user in it.

### One plan deletes a project with everything in it

A plan to delete a project takes everything left in it, not only volumes and networks, in one change. One edit asks for it: a project's or an environment's deletion with `Edit.with_contents`, which the panel sends and the CLI's `dockwell projects delete <name> --with-contents` (and `environments delete`) sends too. The engine lists what lives in its environments as it plans, adds each as a deletion, checks each as any edit, and orders them by what uses what, so routes go before services, and services before what they mount. This replaces ADR 0036's "the panel offers what the panel can't otherwise delete": a person asked to delete the project, and every item is on the plan to review. Listing them as it plans, rather than from what a client read, leaves nothing made in between behind.

- **Without `with_contents` the engine still refuses a project that would keep anything,** and names what, as ADR 0036 says, and its refusal now ends with the command that takes them all. `with_contents` on any other kind, or without a deletion, is refused.
- **What lives inside another resource goes after it.** A database user lives in its database server, and deleting it drops its database. When one change deletes both on a server, the engine now deletes the server first, so the user's step finds nothing to drop, destroys nothing, and only forgets the user: its database stays in the server's volume, kept as that volume is. A kind says so of a reference (`resource.Reference.Inside`), so the rule isn't the database's alone.
- **A plan says what's true of a deleted volume's data:** its server keeps it for 7 days, and undoing the change brings it back. Only data nothing keeps, such as a database dropped with its user, reads as not coming back.
- **Undo brings it all back,** as for any change, with volumes within their 7 days.

### Variables and secrets, by project

The Variables page joins the rail, as "Variables". Variables and secrets live in an environment, and services use them by name ([ADR 0022](0022-secrets-encrypted-at-rest.md)), which is what the others call shared variables, so the page keeps that model and changes how you find the environment:

- **A project picker,** searchable, in place of the segmented switch. Each project shows its counts ("3 variables · 2 secrets") and a mark for nothing running. An environment picker appears only when the project has more than one environment. The page remembers the last choice for its viewer, and a link with an environment opens that one.
- **Finding a name across every project** is one box, one step away: it lists each variable and secret of that name, with its project and environment, from `ListResources`, which names them without values.
- **A project with nothing running offers its Delete** on the page, as the Projects list does.
- **An app's Environment tab shows its own project's values,** and links to that project on the Variables page.

### Interaction principles

- *The interface does the work:* deleting the last app takes what it left, without a second errand; leftovers are found and marked, not looked for.
- *Defaults first, power one step away:* the switch is on; turning it off is one click; finding a name in every project is a box away.
- *Pick, don't type:* a project picker, never a list of every environment.
- *State and action:* "Nothing running · 1 secret, 1 volume" and Delete, not an explanation of projects.
- *One way in for each task:* a project is deleted from wherever it shows (an app's delete, the list, its page, the Variables page, the command bar) with the same plan.

## Consequences

- An install's leftover projects become visible, each two clicks from gone: Delete, then apply.
- Deleting an app usually deletes more than it did, which the plan lists, and Undo reverses for 7 days.
- The command bar's "Delete a project" works for a catalog install's project, which it didn't.
- Three pieces of work follow, in order: this record with the delete; the Projects list and page, where renaming a project's display name is a plain edit of the project, as the engine keeps what it was installed from and what it serves (`ProjectType.Keep`); and the Variables page.
- The CLI deletes a project or an environment with what's in it in one command, `--with-contents`, and the panel sends the same edit, so both get the same plan.
