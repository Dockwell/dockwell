# 0101. A port a catalog app publishes to everyone, on purpose

- Status: Accepted. Amends [ADR 0084](0084-what-an-app-needs-met-by-a-new-service-one-you-run-or-a-connection.md), where a catalog app's port is held to the server alone only while it waits for its owner, and [ADR 0100](0100-dockwells-catalog-keeps-its-apps-current.md), where the proof's floor refuses every port that answers past the server.
- Date: 2026-11-16
- Issue: #752

## Context

The catalog's proof (ADR 0100) ran nginx-proxy-manager for the first time and failed it on the floor's rule that every port answers on the server alone until it has a domain. Two things were tangled there:

- **Its console was public by default.** Port 81, the admin console, was published on every address of the server, at 8881, guarded only by its password, before the person had done anything.
- **A proxy's own 80 and 443 are public by design.** That's the point of the app, and the floor had no way to tell them from the console.

Dockwell had no notion of a port that is public on purpose:
- An entry's Compose file set each port's address itself. A store's apps are converted onto 127.0.0.1, but an entry written by hand could leave the address out, which publishes on every address. Three in Dockwell/catalog did: nginx-proxy-manager's console, 80 and 443, and the mysql and postgres entries' Adminer.
- A plan said nothing of a port on every address, and nothing asked a person to approve one.

## Decision

### An entry declares a port public, on its port input

- **The declaration:** a port input says `public: true` (`catalogv1.Input.public`, 83). It means the port answers on every address of the server, on purpose, as a proxy's 80 and 443 do.
- **What the check refuses:** `public` on an input that isn't a port, a public input that no service publishes whole as its host port (`"${HTTP_PORT}:80"`, or the long form's `published`), and one published on an address that isn't every address.
- **The install form** says "Public" beside such a port.

### Every other port answers on the server alone

- **Install and upgrade:** each port a catalog app publishes is written on 127.0.0.1, whatever its file says, but a public input's. That covers a port with no address, `0.0.0.0` or `::`, another address, and a container port alone, which Docker would publish on a port of its own choosing. It happens wherever Dockwell reads an entry for an install: `catalog.Need`, so an install, an upgrade and a change of need, and a change of companion through `catalog.Loopback`.
- **What the rewrite can't see:** the catalog's check refuses a service whose ports it couldn't write: a `ports` list that's an alias, a port that's one, a merge key that brings ports, and a short port whose host it can't read, as a whole port from a default, `"${SPEC:-18123:83}"`. And each plan reads its file as Compose does, after the rewrite, and is refused where any port it doesn't declare public isn't on 127.0.0.1, so the rule holds for a form neither knows.
- **The pin:** an install's pin keeps its file as it ran, with those addresses. An upgrade then reads what Dockwell ran, not a person's edit, where the entry's file said no address.
- **What's an upgrade:** a release is compared with the pin as an install would run it. So a release that only takes `public` away is one, and so is any newer release to an install from before, whose pin has its ports as the entry wrote them, which moves its console.
- **Existing installs:** an install made before this ran its unaddressed ports on every address, and its pin says no address. Its next upgrade moves each such port that isn't declared public to 127.0.0.1. The plan says so for each service, as `port_to_server_alone`, with the way that reaches it in any case first: "proxy's port 8881 answers on this server alone after this, not on every address. Reach it with the SSH tunnel on its page." It names the page's tunnel only for the port the page shows one for, the web service's first port on the server alone, and otherwise the tunnel's port: "Reach it through an SSH tunnel to port 15432." A domain is offered too only for the page an app is meant to serve on one, its web service, never a companion tool such as Adminer, nor a database, nor a service of an app that holds 80 or 443 itself, as nginx-proxy-manager may, which leaves Dockwell's ingress none. A change of companion and a change of need say the same where they move a port.
- **A person sees it first:** moving a port to the server alone narrows what reaches the app, so a token may apply it, but an automatic update leaves such a plan for a person, as it does a release that needs something new. Whoever reached the port on the server's address would otherwise find it gone the next morning.
- **A person's own edit:** a port a person opened to every address after the install stays as they set it. The merge reads it as theirs, and the plan doesn't say it moves.

### A person approves a public port in the plan

- **The host access:** each port a catalog install or upgrade makes public is host access, `HOST_ACCESS_KIND_PUBLIC_PORT` (19). Its words are "the port 80 on every address of the server, for proxy: anyone who reaches the server reaches it there".
- **What that brings,** as with any host access (ADR 0097): a signed-in person approves it, a token can't apply it, and an automatic update leaves it for a person. A role in the app's project may approve it, as with plain HTTP for anyone; it isn't an admin's alone.
- **Only what's new:**
  - The catalog's install and upgrade pass the ports through `revision.Edit.Public`, a field of the control plane's own that the API doesn't take.
  - They pass only a port the release the install runs didn't publish on every address, by what each release declares, not how the port is now. So a later release of nginx-proxy-manager asks nothing it asked before, and its automatic updates go on, held or not.
  - The change keeps the ports with each item (`change_items.public_ports`, migration 0045), as applying it plans it again.

### Only catalog installs

- **Unchanged:** a port of any other app, from a Compose file, a Git build or a service made in the panel, is as it was. It's published as written and approved by nobody.
- **A separate decision:** making every public port host access would change who may publish a port to everyone across Dockwell, tokens and automatic runs included. It's tracked as its own issue.

### The floor

- **The rule:** the proof allows an address other than 127.0.0.1 only on the host port of a public input, as the install's pin has its value, on the service and protocol the entry publishes it on, and only on every address. `dockwell-catalog public-ports` says which, from the entry and the install's values. Anything else answering past the server fails it.
- **The proof's apply:** a plan with a public port is applied as a person does, as for other host access.

## What we turned down

- **An explicit `0.0.0.0` in the entry's file as the declaration.** It's cheaper, but it reads as an accident as easily as a choice, and it would show as a plan note only, which a token and an automatic update don't stop for.
- **Host access for every public port in Dockwell, now.** It's the principled rule, but it reaches every Compose import and Git deploy, not just this issue. It's left as its own decision.

## Consequences

- **nginx-proxy-manager:** its console answers on the server alone, reached through a domain, and its 80 and 443 answer everyone once a person approves them. The mysql and postgres entries' Adminer answer on the server alone.
- **The catalog's entries:** Dockwell/catalog's change marks nginx-proxy-manager's `HTTP_PORT` and `HTTPS_PORT` public, and writes 127.0.0.1 on its console's port and Adminer's. It lands after this.
- **Installs from before:** an install of one of those three moves its console to the server alone at its next upgrade, with the note.
