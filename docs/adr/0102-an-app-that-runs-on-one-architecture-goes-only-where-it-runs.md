# 0102. An app that runs on one architecture goes only where it runs

- Status: Accepted. Amends [ADR 0078](0078-other-stores-read-on-the-server.md), which left out a store's app that runs on amd64 alone, and [ADR 0085](0085-most-of-other-stores-apps-installed-safely.md).
- Date: 2026-11-16
- Issue: #570

## Context

Dockwell's catalog is one for the whole fleet, so the converters left out every store app that its store says runs on one architecture: 1Panel's `architectures`, Runtipi's `supported_architectures`, CasaOS's `architectures`, and Coolify's `amd_only`. That was 69 apps across 1Panel, Runtipi, Coolify and CasaOS, nearly all amd64 alone.

A fleet may run amd64 and arm64 servers side by side. Such an app runs on any amd64 server and on no arm64 one: an image with no arm64 build fails only as Docker pulls it there, after the plan was applied.

## Decision

### An entry says the architectures it runs on

- **The field:** `Entry.architectures` (32), as a server's inventory names them: `amd64`, `arm64`. Empty, it runs on both, as every entry has until now. Dockwell's own catalog leaves it empty: ADR 0100's proof runs each entry on both.
- **The check:** each value is `amd64` or `arm64`, once each. Both is as empty.
- **The converters** set it from the store's own field, keeping the architectures Dockwell's servers run and dropping the rest, such as `arm/v7`. They no longer leave such an app out. An app whose store names neither amd64 nor arm64 is still left out, saying so. Coolify's `amd_only` is amd64. The converter that publishes a catalog checks each image's platforms against what the app says, not both.

### The service carries it, and the engine holds the rule

- **The field:** `Service.architectures` (190), the same words. Empty runs anywhere, as today.
- **Who sets it:** a catalog install copies its entry's list onto each service it makes, and an upgrade copies its release's, so a release that runs anywhere takes the limit away. A change that keeps the release, such as a companion's or a need's, keeps what each service has, and a service it adds takes the install's.
- **The rule:** the engine refuses to place a service on a server whose inventory reports another architecture, wherever it is, and on a server that hasn't reported one yet where the service is new to it or its list changes, giving the fact, the consequence and the fix. A change that leaves a service where it runs, as one to a value it uses does, isn't held up by a report that's missing, as after a restore that keeps none:
  - "web runs on amd64 alone, and edge-1 is arm64, so its images have no build there and it wouldn't start. Put web on an amd64 server."
  - "web runs on amd64 alone, and edge-2 hasn't said its architecture yet, so Dockwell can't tell whether web runs there. It says once its agent connects; plan this again then, or put web on an amd64 server."
- **One rule for every path:** the check is in the engine's placement, which every plan goes through, so an install, an upgrade, a move by any edit of its servers or a fleet action, and an undo all meet it. A service that stays where it is is checked against what its server reports, so an upgrade whose release drops the architecture of a server the install runs on can't be planned, and an automatic update leaves it, as any upgrade that can't be planned.
- **Only the control plane keeps it:** the agent never sees it, so drift its server reports keeps it as it was, and changing it alone restarts nothing.
- **An edit that leaves it out keeps it.** It's a fact about the images, not a choice an edit makes by omission, and a script, a file written by hand or a client older than the field would otherwise drop it unseen. Only an edit that says it sets the list clears it, as a catalog upgrade to a release that runs anywhere does. An undo, a rollback, a restore and adopted drift put back exactly what was there.
- **The store's word is all an on-server source has.** An install pins each image by its tag's digest alone, and reads no platforms, so a store that says both while its image has one build is planned, and fails only as Docker pulls it. Only the converter that publishes a catalog reads each image's platforms. Reading them at conversion or at pin time is #769, which would also widen a store's list that names fewer architectures than its images have.

### The panel

- **The install form** offers only the servers the app runs on: a server of another architecture shows it beside its name, one that hasn't said shows "Architecture not known yet", and neither can be chosen. A line under the servers says "Runs on amd64 alone."
- **With one server it can't take,** the form says why and plans nothing: "This app runs on amd64 alone, and pi-1 is arm64. Add a server it runs on."
- **With a fleet where no server fits,** it says so and plans nothing: "Runs on amd64 alone, and none of your servers is amd64. Add an amd64 server."

## What we turned down

- **Checking in the catalog's handlers only.** It covers an install and an upgrade, but a move is an edit of the service, which never passes through them.
- **Reading each image's platforms at every placement.** It's the ground truth, but it asks a registry for every service of every plan. Reading them once, at conversion or at pin time, is the better place: #769.
- **Leaving such apps out.** A fleet with amd64 servers runs them.

## Consequences

- 1Panel goes from 158 to 175 apps, Runtipi from 175 to 188, and CasaOS from 107 to 121, each offered only on the servers it runs on. Coolify's one, mattermost, then stops at Compose's `platform:`, which Dockwell can't run yet and could set the same field later.
- A server whose agent hasn't reported yet can't take such an app until it does.
- A store's list can be narrower than its images: metabase, nexus, excalidraw and anythingllm name amd64 alone while their images have arm64 builds, so they're offered on amd64 servers alone until platforms are read (#769).
