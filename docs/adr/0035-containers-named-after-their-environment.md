# 0035. A service's containers are named after its project and environment

- Status: Accepted
- Date: 2026-11-07

## Context

[ADR 0021](0021-the-docker-driver.md) runs each environment as a Compose project named after the environment's ID, and names a service's container as Compose would, `<project>-<service>-<number>`. On the first real server, `docker ps` showed `env_01kat4zr6e3v0y5xbkzb5d9q6m-nginx-1` for an app made in the panel, and `env_01kat5c2m8w1q9r7x3d6v0b4hn-shop-1` for one imported into a project of its own (#112). People read `docker ps` when something is wrong, often beside containers Dockwell didn't make, and neither name says what it is.

The ID is in the name for reasons that still hold:

- **It never changes.** An environment keeps its ID for as long as it's there, so nothing has to be made again for its name.
- **It names more than containers.** Volumes and networks are `<project>_<name>`, and Compose checks the project label on each. A new project name would mean new volumes, and Docker can't rename a volume, so moving one means copying its data with the service stopped.
- **It marks what's Dockwell's.** A container, volume or network run by hand can't take a name that starts `env_`, so it never manages, mounts or joins an environment's.

### Options considered

1. **Name the Compose project after the environment, such as `default-production`.** Everything would read well, `docker volume ls` too. But every existing volume and network would need moving, a database's data among them, with downtime and a migration of data Dockwell doesn't own. The `env_` guard would go too. Rejected.
2. **Name the project that way for new environments only.** Old ones stay unreadable, which is the user who asked, and every place that derives the project from an ID would need a lookup instead. Rejected.
3. **Keep the project, and give only the container a readable name.** Compose lists a project's containers by their labels, and so do Dockwell's rollouts, health checks and logs, so a container's name is free to be what a person reads. Chosen.

## Decision

**A service's container is named `<project>.<environment>.<service>-<number>`,** such as `default.production.nginx-1` or `shop.production.web-1`. Its Compose project, its labels, its volumes and its networks stay as ADR 0021 has them, by the environment's ID.

- **Dots join the parts.** Project, environment and service names are DNS labels, with no dots, so two environments never come to one name, as `a-b` and `c` against `a` and `b-c` would with dashes. What follows the last dot is the service and its number, `nginx-1`, which is what a log calls a container ([ADR 0027](0027-health-checks-rollouts-and-rollback.md)).
- **The control plane names it, and the agent names the container.** A plan and a step carry what people call the resource's environment (`DesiredResource.scope_name`), which the control plane reads from its store each time, and the agent passes to the service driver with the call. An agent has no store of its own. A call without it, as from a control plane before this, names the container as Compose would, and nothing else differs.
- **Nothing that runs is renamed or replaced for it.** A container's name isn't in its spec, so the name alone plans nothing. A container named the old way goes on being its service's, found by its labels, until a deploy makes a new one, which takes the new form. A container that a rollout was stopping, and that goes back into service, gets its own name back.
- **A container `docker compose` makes is its service's, under Compose's name.** Compose names what it makes itself, so `docker compose -p env_… up`, or `up` on an export, recreates a container it finds out of date as `env_…-api-1`, and the readable name is gone. Of the three things Dockwell could do then, it takes either name for good:
  - It doesn't see the name as drift, or put its own back. Drift is what differs from a spec, and a name isn't in one, so nothing plans a rename, and the agent reconciles nothing behind a person's back. Renaming what someone just made with Compose would fight them, for a name only.
  - It finds the container as it finds any of a service's, by the labels Compose gives it too, with its number. So health, logs, rollouts and the ingress go on with it, and drift reads by its spec. A log calls it `api-1` either way, as what follows the first dash of `env_…-api-1` and the last dot of `default.production.api-1` are the same.
  - The next deploy that makes a container names it the readable way, under the next number.
  - Anything that looks for a service's container, a script or a test, finds it by its labels, as `docker compose ps` does, and never by a name it saw before.
- **What a deleted service's image kept is listed under a name that never changes:** the one Compose gives its first container, `env_…-db-1`, which is what the list has had all along. So undoing a deletion made before the upgrade restores its data, and so does a step applied with another name than it was planned with, as when the store couldn't say what people call the environment for one of them.
- **A port a plan frees for another service is found by its holder's labels,** as it may be named either way, or under a later number.
- **The ingress finds an instance's container by its labels** (`docker.Client.ContainerOf`), not by the name Compose would give it, and skips one a rollout has under a name for now.
- **A container run by hand can't take the form when it's made anew,** `<project>.<environment>.<service>-<number>` exactly, as it couldn't take `env_…`, so it never reads as a service's. One stored with such a name before goes on being edited, undone and adopted, and other names with dots, such as `blog.example.com`, are anyone's. Nor can another name for a container on a network, an alias, take the form, as Docker's DNS would answer it with both.

## Consequences

- `docker ps` says which project, environment and service each container is. `docker volume ls` and `docker network ls` still show the environment's ID, as moving them would cost a service's data.
- `docker compose -p env_… ps`, `logs` and `exec` work as before, as they go by labels. A container `docker compose up` makes is named as Compose names it, `env_…-api-1`, and stays the service's, until a deploy makes the next one. So after `docker compose up`, `docker ps` can show both forms for a while, and a name a script saw before Compose ran doesn't find what runs now.
- For a while after upgrading, a server has both forms, as each service keeps its container until its next deploy. Anything that names a container should ask the API or use `docker compose exec`, as ADR 0027 says already, since a rolling deploy changes the number anyway.
