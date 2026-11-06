// SPDX-License-Identifier: AGPL-3.0-only

import { type HostAccess, HostAccessKind, IntentKind, type Plan, PlanWarning } from "@dockwell/api";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { useTimeWords } from "../../lib/time";
import { asGoodAsRoot, changing, factAndRest, onlyIfNewer, reloadsProxy, shownName, startsStopped } from "./plan";
import { namesARange, plain, type RolloutLine, useChangeWords } from "./words";

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

/**
 * What a plan says of how its steps replace what runs, each thing once, as servers
 * mostly agree. A plan of several services says which it means.
 */
function useRolloutLines(plan: Plan | undefined): (RolloutLine & { key: string })[] {
  const { t } = useLingui();
  const words = useChangeWords();
  const time = useTimeWords();
  const steps = (plan?.servers ?? []).flatMap((s) => s.steps).filter((step) => step.rollout);
  const several = new Set(steps.map((step) => step.resourceId)).size > 1;
  const out = new Map<string, RolloutLine & { key: string }>();
  for (const step of steps) {
    const name = step.resource?.name ?? step.resourceId;
    for (const line of words.rollout(step.rollout, time.span, step.restart !== "", startsStopped(step))) {
      const text = several && !line.lead ? t`${name}: ${line.text}` : line.text;
      const lead = several && line.lead ? t`${name}: ${line.lead}` : line.lead;
      const key = `rollout-${lead ?? ""}-${text}`;
      if (!out.has(key)) out.set(key, { ...line, text, lead, key });
    }
  }
  return [...out.values()];
}

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
      const planned = plan?.resources.find((r) => r.id === step.resourceId);
      const name = (planned && shownName(planned)) || step.resource?.name || step.resourceId;
      for (const access of step.hostAccess) add(name, server.serverName, access);
    }
  }
  // At the plan's level too, as a plan with no steps has nowhere else.
  for (const resource of plan.resources) {
    const name = shownName(resource) || resource.id;
    const server = plan.servers.find((s) => resource.servers.includes(s.serverId))?.serverName ?? t`its server`;
    for (const access of resource.hostAccess) add(name, server, access);
  }
  reaches.sort((a, b) => (WORST[a.access.kind] ?? 9) - (WORST[b.access.kind] ?? 9));

  const notice = ({ name, server, access }: (typeof reaches)[number], key: string): Notice => {
    // What a spec holds is quoted where it isn't what a name holds, so it can't write these words.
    const [path, leadsTo] = [plain(access.path), plain(access.leadsTo)];
    // A relabel writes on the server, however read-only the mount is (ADR 0021).
    const relabel =
      access.relabel === "Z"
        ? t`Where SELinux is enforced, Docker relabels everything there as private to this app, so nothing else can use it.`
        : access.relabel === "z"
          ? t`Where SELinux is enforced, Docker relabels everything there, to share it between containers.`
          : "";
    const andRelabel = (detail: ReactNode): ReactNode =>
      relabel ? (
        <>
          {detail} {relabel}
        </>
      ) : (
        detail
      );
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
          title: leadsTo
            ? t`${name} joins the ${leadsTo} network ${path} on ${server}`
            : t`${name} joins the network ${path} on ${server}`,
          detail: leadsTo
            ? t`Dockwell didn't make that network, and it reaches past the server's firewall. Whatever else is on it, other projects' services too, can reach ${name}, and it them.`
            : t`Dockwell didn't make that network. Whatever else is on it, other projects' services too, can reach ${name}, and it them.`,
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
          detail: namesARange(access.path)
            ? t`It takes that proxy's word for where a request came from. Whatever else is in that range can reach the route, and say who its client is, so name a network the proxy has to itself.`
            : t`It takes that proxy's word for where a request came from.`,
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
              detail: andRelabel(
                leadsTo ? (
                  <Trans>
                    It leads to <code>{leadsTo}</code> through a symlink, and the server's security rests on what is
                    there, so reaching it is as good as root.
                  </Trans>
                ) : (
                  t`The server's security rests on what is there, so reaching it is as good as root.`
                ),
              ),
              trust: true,
            }
          : {
              key,
              title: t`${name} reaches ${path} on ${server}`,
              detail: andRelabel(
                leadsTo ? (
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
      // Only a person can say whether this data can take it, so it needs one.
      const rollout = step.rollout;
      if (rollout?.rolling && rollout.chosen) {
        const shared = [...rollout.volumes, ...rollout.paths, ...rollout.imageData, ...rollout.cantTell].join(", ");
        out.push({
          key: `writers-${key}`,
          title: t`Two containers may write to ${shared}`,
          detail: t`${name}'s rollout order says to start first, so the old and new containers both write there for as long as this takes, which can corrupt a database. Apply this only if the app can take it.`,
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
      title: t`This plan destroys data, may cut off access, or lets two containers write to one place`,
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
export function useWarnings(plan: Plan | undefined, applied = false, serving: readonly string[] = []): Notice[] {
  const { t } = useLingui();
  const words = useChangeWords();
  const { rest } = useReaches(plan);
  const rollouts = useRolloutLines(plan);
  // Going back is when this is news. Any other plan says it among what it means.
  const rollingBack = plan?.intent?.kind === IntentKind.ROLL_BACK;
  const worthWarning = (plan?.notes ?? []).filter((note) => note.reason !== "volumes_stay" || rollingBack);
  const notes = worthWarning.map((note, i): Notice => {
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
      case "first_steps":
        return { key, title: t`What to do first`, detail: note.message };
      case "no_backup":
        return {
          key,
          title: t`No backup is taken first`,
          detail: t`Dockwell can't make one yet. Undoing this puts the old version back, but not what the new one changed in its data, such as a database it migrated. Back up what you can't lose first.`,
        };
      case "values_latest": {
        const names = note.names.join(", ");
        return {
          key,
          title: t`${name} runs with the latest value of ${names}`,
          detail: t`A rollback puts back which variables and secrets it uses, not what they held then. To go back on a value, change it in Variables and secrets.`,
        };
      }
      case "kept_logs_offline": {
        const [server = ""] = note.names;
        return {
          key,
          title: t`${server} is offline, so what it kept of ${name} stays on its disk until it's back`,
          detail: t`It can't be read meanwhile, and it goes once the server is back and the 7 days are up.`,
        };
      }
      case "updates_project": {
        // A plan kept from before it said what would go has names all the same, and only
        // its message says what it deletes.
        if (!note.named) break;
        const [project = "", ...running] = note.names;
        const names = running.join(", ");
        const deleted = note.deleted.join(", ");
        return {
          key,
          title: t`${project} already runs ${names}`,
          detail:
            deleted !== ""
              ? t`This changes what runs: a service of the same name in the file replaces it, and ${deleted}, which the file doesn't name, would be deleted. If that isn't what you meant, change name: in the file to give it a project of its own.`
              : t`This changes what runs: a service of the same name in the file replaces it. If that isn't what you meant, change name: in the file to give it a project of its own.`,
        };
      }
      case "digest_pinned": {
        const [image = "", revision = "", digest = ""] = note.names;
        if (digest === "") break;
        return {
          key,
          title: t`${name} runs ${image} as revision ${revision} ran it, ${digest}, even if the tag has moved since`,
        };
      }
      case "digest_gone": {
        const [image = "", revision = "", digest = ""] = note.names;
        if (digest === "") break;
        return {
          key,
          title: t`The ${image} that revision ${revision} ran, ${digest}, isn't on the server, and its registry won't give it, so ${name} runs what ${image} is now`,
        };
      }
      case "digest_differs": {
        const [image = "", revision = ""] = note.names;
        if (revision === "") break;
        return {
          key,
          title: t`Revision ${revision} ran different images of ${image} on each server, so ${name} runs what ${image} is now`,
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
        const where =
          field === "healthcheck"
            ? t`health check`
            : field === "driver_opts"
              ? t`driver options`
              : field === "environment"
                ? t`environment variable names`
                : field;
        // The API says where the value came from, and an .env is where someone may not have looked.
        const fromEnv = note.from === "env_file";
        // A file's own field, in its own words, so its name says how many it is.
        const several = field === "environment" || (field.endsWith("s") && field !== "dns" && field !== "tmpfs");
        const title = several
          ? fromEnv
            ? t`${name}'s ${where} hold the value of ${variable} from your .env`
            : t`${name}'s ${where} hold the value of ${variable}`
          : fromEnv
            ? t`${name}'s ${where} holds the value of ${variable} from your .env`
            : t`${name}'s ${where} holds the value of ${variable}`;
        return {
          key,
          title,
          detail:
            field === "command" || field === "entrypoint"
              ? t`Anyone who can read this app can see it. Its value has a space, a quote or another character Compose reads as part of the line, so one name can't stand for it. To keep it hidden, write the ${where} as a list, with each word its own variable.`
              : field === "environment"
                ? t`Anyone who can read this app can see it. Only a variable's value is hidden, so don't write a secret in its name.`
                : t`Anyone who can read this app can see it. It can't be hidden there, so if it's a secret, set it in environment for the app to read instead.`,
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
      // What DNS said as this was planned stops mattering once the domain serves.
      const dns = serving.includes(step.domainCheck?.domain ?? "") ? "" : words.domain(step.domainCheck);
      if (dns) out.push({ key: `dns-${key}`, title: dns });
      return out;
    }),
  );
  // These say what a pull does, which is no warning: there is nothing to weigh.
  const quiet = new Set(["pulls_again", "leaves_pin", "starts_stopped"]);
  const fromRollouts = rollouts
    .filter((line) => line.warn)
    .map(
      (line): Notice => ({
        key: line.key,
        title: line.lead ?? line.text,
        detail: line.lead ? `${line.text}.` : undefined,
      }),
    );
  return [...rest, ...fromRollouts, ...routes, ...notes.filter((_, i) => !quiet.has(worthWarning[i]?.reason ?? ""))];
}

/**
 * What is worth knowing beyond the diff, quietly, from the plan's steps: data
 * it brings back, and a health check it can't know yet.
 */
export function useAsides(plan: Plan | undefined): Notice[] {
  const { t } = useLingui();
  const words = useChangeWords();
  const rollouts = useRolloutLines(plan);
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
  // After what says it starts or pulls, as these go on from that.
  for (const line of rollouts) {
    if (!line.warn) out.push({ key: line.key, title: `${line.text}.`, notice: line.notice });
  }
  // A rollback says this as a warning, as that is when it is news.
  if (plan?.intent?.kind !== IntentKind.ROLL_BACK) {
    for (const [i, note] of (plan?.notes ?? []).entries()) {
      if (note.reason !== "volumes_stay") continue;
      const name = note.resource?.name ?? "";
      const names = note.names.join(", ");
      out.push({
        key: `stays-${i}`,
        title: t`If ${name} is rolled back, ${names} stays as it is: what the new version writes there isn't undone.`,
      });
    }
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
      if (step.goes) out.push({ key: `goes-${key}`, title: words.goes(step.goes), notice: true });
      if (step.moves) out.push({ key: `moves-${key}`, title: words.moves(step.moves), notice: true });
      if (reloadsProxy(step)) out.push({ key: `reload-${key}`, title: `${words.reload(server.serverName)}.` });
      if (step.restores) out.push({ key: `restores-${key}`, title: t`${name} gets back the data the server kept.` });
      if (step.hidesLogs) {
        out.push({
          key: `hides-logs-${key}`,
          title: t`${name}'s kept logs can't be read from then on, and go in 7 days. Undoing this brings them back.`,
        });
      }
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
