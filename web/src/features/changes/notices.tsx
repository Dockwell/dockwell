// SPDX-License-Identifier: AGPL-3.0-only

import { type HostAccess, HostAccessKind, type Plan, PlanWarning } from "@dockwell/api";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { asGoodAsRoot, changing, factAndRest, onlyIfNewer } from "./plan";
import { plain, useChangeWords } from "./words";

/** One thing to know about a plan before applying it. */
export interface Notice {
  key: string;
  /** The fact, short: "web gets root on hz-1". */
  title: ReactNode;
  /** The consequence, and the fix if there is one. */
  detail?: ReactNode;
  /** It comes down to what the resource runs, which only whoever applies it can judge. */
  trust?: boolean;
  /** Something to know that someone may well have chosen, so it stands out, and isn't an alarm. */
  notice?: boolean;
}

/** Everything a plan says it reaches, on its resources and its steps. */
function accessOf(plan: Plan): HostAccess[] {
  return [
    ...plan.resources.flatMap((r) => r.hostAccess),
    ...plan.servers.flatMap((s) => s.steps.flatMap((step) => step.hostAccess)),
  ];
}

/**
 * Whether a plan asks whoever applies it to type its fingerprint: for destroying
 * data, and for what is as good as root. Every plan that reaches the server needs
 * a signed-in person, and the fingerprint goes with Apply, whatever this says: a
 * folder of the app's own stops at a warning, so that this means something when
 * it comes (ADR 0028). A plan that says it reaches the server and doesn't say what
 * is treated as the worst.
 */
export function needsConfirming(plan: Plan | undefined): boolean {
  if (!plan) return false;
  if (plan.destructive) return true;
  const access = accessOf(plan);
  return plan.hostAccess && access.length === 0 ? true : access.some(asGoodAsRoot);
}

/** How much worse one reach is than another, so the worst leads. */
const WORST: Readonly<Partial<Record<HostAccessKind, number>>> = {
  [HostAccessKind.DOCKER_SOCKET]: 0,
  [HostAccessKind.PRIVILEGED]: 0,
  [HostAccessKind.HOST_NETWORK]: 1,
  [HostAccessKind.CAPABILITY]: 2,
  [HostAccessKind.GROUP]: 2,
  [HostAccessKind.NETWORK_DRIVER]: 3,
  [HostAccessKind.EXTERNAL_NETWORK]: 3,
  [HostAccessKind.SERVER_PORT]: 3,
  [HostAccessKind.PLAIN_FOR_ANYONE]: 4,
  [HostAccessKind.TRUSTED_PROXY]: 4,
  [HostAccessKind.HTTPS_OFF]: 4,
  [HostAccessKind.VOLUME_MOUNT]: 5,
  [HostAccessKind.SERVER_PATH]: 6,
};

/** What a plan lets a resource reach on its server, in words, worst first, with what is as good as root apart. */
function useReaches(plan: Plan | undefined): { root: Notice[]; rest: Notice[] } {
  const { t } = useLingui();
  const words = useChangeWords();
  if (!plan) return { root: [], rest: [] };

  const reaches: { name: string; server: string; access: HostAccess }[] = [];
  const seen = new Set<string>();
  const add = (name: string, server: string, access: HostAccess) => {
    const key = `${name}/${server}/${access.kind}/${access.path}/${access.address}/${access.readOnly}`;
    if (seen.has(key)) return;
    seen.add(key);
    reaches.push({ name, server, access });
  };
  for (const server of plan.servers) {
    for (const step of server.steps) {
      for (const access of step.hostAccess) add(step.resource?.name ?? step.resourceId, server.serverName, access);
    }
  }
  // At the plan's level too, as a plan with no steps has nowhere else.
  for (const resource of plan.resources) {
    const name = resource.resource?.name ?? resource.id;
    const server = plan.servers.find((s) => resource.servers.includes(s.serverId))?.serverName ?? t`its server`;
    for (const access of resource.hostAccess) add(name, server, access);
  }
  reaches.sort((a, b) => (WORST[a.access.kind] ?? 9) - (WORST[b.access.kind] ?? 9));

  const notice = ({ name, server, access }: (typeof reaches)[number], key: string): Notice => {
    // What a spec holds is quoted where it isn't what a name holds, so it can't write these words.
    const [path, leadsTo] = [plain(access.path), plain(access.leadsTo)];
    switch (access.kind) {
      case HostAccessKind.DOCKER_SOCKET:
        return {
          key,
          title: t`${name} gets root on ${server}`,
          detail: leadsTo ? (
            <Trans>
              <code>{path}</code> leads to <code>{leadsTo}</code> through a symlink, which gives it control of Docker on
              the server, the same as root.
            </Trans>
          ) : (
            <Trans>
              <code>{path}</code> gives it control of Docker on the server, the same as root.
            </Trans>
          ),
          trust: true,
        };
      case HostAccessKind.PRIVILEGED:
        return {
          key,
          title: t`${name} gets root on ${server}`,
          detail: t`It runs in privileged mode, which gives it every device and capability on the server.`,
          trust: true,
        };
      case HostAccessKind.CAPABILITY:
        return access.sameAsRoot
          ? {
              key,
              title: t`${name} gets root on ${server}`,
              detail: (
                <Trans>
                  It has the Linux capability <code>{path}</code>, which is the same as root.
                </Trans>
              ),
              trust: true,
            }
          : {
              key,
              title: t`${name} has the Linux capability ${path} on ${server}`,
              detail: t`That goes beyond what Docker gives a container by default.`,
            };
      case HostAccessKind.GROUP:
        return access.sameAsRoot
          ? {
              key,
              title: t`${name} gets root on ${server}`,
              detail: (
                <Trans>
                  It joins the group <code>{path}</code>, which is the same as root.
                </Trans>
              ),
              trust: true,
            }
          : {
              key,
              title: t`${name} joins the group ${path} on ${server}`,
              detail: t`That is the same as root if it is the group that owns Docker's socket on the server.`,
            };
      case HostAccessKind.EXTERNAL_NETWORK:
        return {
          key,
          title: t`${name} joins the network ${path} on ${server}`,
          detail: t`Dockwell didn't make that network. Whatever else is on it, other projects' services too, can reach ${name}, and it them.`,
        };
      case HostAccessKind.SERVER_PORT: {
        const at = plain(access.detail);
        return {
          key,
          title: t`${name} reaches the port ${path} on ${server}`,
          detail:
            access.type === "http"
              ? t`That is the server itself, past its containers, at ${at} over plain HTTP.`
              : t`That is the server itself, past its containers, at ${at}.`,
        };
      }
      case HostAccessKind.TRUSTED_PROXY:
        return {
          key,
          title: t`${name} believes the proxy ${path} on ${server}`,
          detail: t`It takes that proxy's word for where a request came from.`,
        };
      case HostAccessKind.PLAIN_FOR_ANYONE:
        return {
          key,
          title: t`${name} answers plain HTTP on ${path} on ${server}`,
          detail: t`Anyone who can reach this server gets it, around whatever is in front of it.`,
        };
      case HostAccessKind.HTTPS_OFF:
        return {
          key,
          title: t`${name} stops HTTPS on ${path} on ${server}`,
          detail: t`https:// addresses get an error, and browsers that saw HSTS from it refuse plain HTTP until that runs out.`,
        };
      case HostAccessKind.HOST_NETWORK:
        return {
          key,
          title: t`${name} shares ${server}'s own network`,
          detail: t`It reaches what the server reaches, past Docker's network and the firewall.`,
          trust: true,
        };
      case HostAccessKind.SERVER_PATH:
        return asGoodAsRoot(access)
          ? {
              key,
              title: t`${name} reaches ${path} on ${server}`,
              detail: leadsTo ? (
                <Trans>
                  It leads to <code>{leadsTo}</code> through a symlink, and the server's security rests on what is
                  there, so reaching it is as good as root.
                </Trans>
              ) : (
                t`The server's security rests on what is there, so reaching it is as good as root.`
              ),
              trust: true,
            }
          : {
              key,
              title: t`${name} reaches ${path} on ${server}`,
              detail: leadsTo ? (
                access.readOnly ? (
                  <Trans>
                    It leads to <code>{leadsTo}</code> through a symlink, and it can read what is there.
                  </Trans>
                ) : (
                  <Trans>
                    It leads to <code>{leadsTo}</code> through a symlink, and it can read and write what is there.
                  </Trans>
                )
              ) : access.readOnly ? (
                t`It can read that path on the server.`
              ) : (
                t`It can read and write that path on the server.`
              ),
            };
    }
    const reach = words.access(access);
    const isDomain = access.kind === HostAccessKind.DOMAIN || access.kind === HostAccessKind.PANEL_DOMAIN;
    return {
      key,
      title: t`${name} reaches ${reach} on ${server}`,
      ...(isDomain
        ? {}
        : {
            detail: asGoodAsRoot(access)
              ? t`The server's security rests on what that reaches, so it is as good as root.`
              : t`That goes beyond what Docker keeps for it.`,
          }),
      ...(asGoodAsRoot(access) ? { trust: true } : {}),
    };
  };
  const named = reaches.map((reach, i) => ({ reach, key: `reach-${i}` }));
  return {
    root: named.filter(({ reach }) => asGoodAsRoot(reach.access)).map(({ reach, key }) => notice(reach, key)),
    rest: named.filter(({ reach }) => !asGoodAsRoot(reach.access)).map(({ reach, key }) => notice(reach, key)),
  };
}

/**
 * What a plan can't take back, worst first: what is as good as root on its
 * server, what data it takes away, and whether it may cut off access. They are
 * for the top of the plan, in the words the form used, and they are why it asks
 * for its fingerprint. They come from the plan as it is, so a plan undoing or
 * rolling back reads the same as a first deploy.
 */
export function useRisks(plan: Plan | undefined): Notice[] {
  const { t } = useLingui();
  const { root } = useReaches(plan);
  if (!plan) return [];
  const out: Notice[] = [...root];

  for (const server of plan.servers) {
    for (const step of server.steps) {
      const name = step.resource?.name ?? step.resourceId;
      const key = `${server.serverId}/${step.resourceId}`;
      if (step.keptPaths.length > 0) {
        const paths = step.keptPaths.join(", ");
        out.push({
          key: `data-${key}`,
          title: t`${name} loses what its image keeps at ${paths}`,
          detail: t`The server keeps that data for 7 days, and undoing this brings it back. After that it is gone.`,
        });
      } else if (step.destroysData) {
        out.push({
          key: `data-${key}`,
          title: t`${name} loses its data on ${server.serverName}`,
          detail: t`Undo may not bring it back. Back it up first if you need it.`,
        });
      }
      if (step.warnings.includes(PlanWarning.MAY_LOCK_OUT)) {
        out.push({
          key: `lock-${key}`,
          title: t`This may cut off access to ${server.serverName}`,
          detail: t`Check that you have another way in, such as your provider's console, before you apply it.`,
        });
      }
    }
  }
  // A plan that says it destroys data, with no step of it to say which.
  if (out.length === 0 && plan.destructive) {
    out.push({
      key: "destructive",
      title: t`This plan destroys data or may cut off access`,
      detail: t`Undo may not bring everything back.`,
    });
  }
  return out;
}

/**
 * What a person should know before applying a plan, which stops nobody: a folder
 * of the server's that an app reaches, a rollback leaving a service's data as it
 * is, or a Compose variable written where anyone can read it. The API says a
 * note in its own message; the panel has its own words for the reasons it knows,
 * so they translate, and leads with the fact of any it doesn't.
 */
export function useWarnings(plan: Plan | undefined, applied = false): Notice[] {
  const { t } = useLingui();
  const words = useChangeWords();
  const { rest } = useReaches(plan);
  const notes = (plan?.notes ?? []).map((note, i): Notice => {
    const key = `note-${i}`;
    const name = note.resource?.name ?? "";
    switch (note.reason) {
      case "volumes_stay": {
        const names = note.names.join(", ");
        return {
          key,
          title: t`${name} keeps what is in ${names} as it is`,
          detail: applied
            ? t`A rollback puts back its spec, not its data, so if a newer version changed that data, such as with a migration, the older one may not read it. Check that it does, and restore a backup if it won't.`
            : t`A rollback puts back its spec, not its data, so if a newer version changed that data, such as with a migration, the older one may not read it. Check before you apply, and restore a backup if it won't.`,
        };
      }
      case "values_latest": {
        const names = note.names.join(", ");
        return {
          key,
          title: t`${name} runs with the latest value of ${names}`,
          detail: t`A rollback puts back which variables and secrets it uses, not what they held then. To go back on a value, change it in Variables and secrets.`,
        };
      }
      case "exposed_more":
        return {
          key,
          title: t`The file writes more variables in plain sight than these`,
          detail: t`Read it for the rest before you pass one a secret.`,
        };
      case "exposed_variable": {
        const [variable = "", field = ""] = note.names;
        const where = field === "healthcheck" ? t`health check` : field;
        return {
          key,
          title: t`${name}'s ${where} holds the value of ${variable}`,
          detail: t`Anyone who can read this app can see it. To keep it hidden, pass it in environment, and use $$${variable} in the ${where}.`,
        };
      }
    }
    const [title, detail] = factAndRest(note.message);
    return { key, title, detail };
  });
  const routes = (plan?.servers ?? []).flatMap((server) =>
    server.steps.flatMap((step, i) => {
      const key = `${server.serverId}/${step.resourceId}/${i}`;
      const out: Notice[] = [];
      const returning = words.returning(step.returningContainers, server.serverName);
      if (returning) out.push({ key: `returning-${key}`, title: returning });
      const dns = words.domain(step.domainCheck);
      if (dns) out.push({ key: `dns-${key}`, title: dns });
      return out;
    }),
  );
  // These say what a pull does, which is no warning: there is nothing to weigh.
  const quiet = new Set(["pulls_again", "leaves_pin", "starts_stopped"]);
  return [...rest, ...routes, ...notes.filter((_, i) => !quiet.has(plan?.notes[i]?.reason ?? ""))];
}

/**
 * What is worth knowing beyond the diff, quietly, from the plan's steps: data
 * it brings back, and a health check it can't know yet.
 */
export function useAsides(plan: Plan | undefined): Notice[] {
  const { t } = useLingui();
  const words = useChangeWords();
  const out: Notice[] = [];
  // Nothing to warn of: it says only that this may come to nothing.
  const image = plan?.intent?.image ?? "";
  for (const [i, note] of (plan?.notes ?? []).entries()) {
    const name = note.resource?.name ?? "";
    if (note.reason === "leaves_pin") {
      out.push({
        key: "leaves-pin",
        title:
          image && name
            ? t`Pulls ${image} again, and replaces ${name} with what that names now, in place of the digest it's pinned to.`
            : note.message,
      });
    }
    // Only the server can say a plan will start what isn't running, and it says how it found it.
    if (note.reason === "starts_stopped") out.push({ key: `starts-${i}`, title: `${note.message}.` });
    if (note.reason !== "pulls_again") continue;
    // More than its image may differ on a server, and then the pull doesn't decide it.
    out.push({
      key: "pulls-again",
      title: onlyIfNewer(plan)
        ? t`Pulls ${image} again, and replaces ${name} only if that's a newer image.`
        : t`Pulls ${image} again, and replaces ${name} whatever that finds, as more than its image differs.`,
    });
  }
  for (const server of plan?.servers ?? []) {
    for (const step of server.steps) {
      const name = step.resource?.name ?? step.resourceId;
      const key = `${server.serverId}/${step.resourceId}`;
      for (const line of words.ingress(step.ingress, server.serverName))
        out.push({ key: `ingress-${key}-${line}`, title: line });
      if (step.unencrypted) {
        out.push({ key: `unencrypted-${key}`, title: words.unencrypted(server.serverName), notice: true });
      }
      for (const line of words.answers(step.answers, step.serverPort, server.serverName)) {
        out.push({ key: `answers-${key}-${line}`, title: line, notice: true });
      }
      if (step.restores) out.push({ key: `restores-${key}`, title: t`${name} gets back the data the server kept.` });
      if (step.healthUnknown) {
        out.push({
          key: `health-${key}`,
          title: t`${name} may take longer to be healthy, as its image isn't on the server yet.`,
        });
      }
    }
  }
  return out;
}

/** What a plan is about, for the words around it: its one resource's name, or how many. */
export function useSubject(plan: Plan | undefined): string {
  const all = changing(plan);
  if (all.length === 1) return all[0]?.resource?.name ?? "";
  return plural(all.length, { one: "# resource", other: "# resources" });
}
