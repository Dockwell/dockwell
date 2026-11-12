// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import {
  BackupMethod,
  type HostAccess,
  HostAccessKind,
  ImageBuild_Builder,
  IntentKind,
  type Plan,
  PlanAction,
  type PlanNote,
  PlanWarning,
  type RestorePlan,
  type StepProgress,
} from "@dockwell/api";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { shownRef } from "../../lib/built";
import { formatBytes, formatDateTime } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { useDetectionWords } from "../apps/detection";
import {
  asGoodAsRoot,
  changing,
  factAndRest,
  installs,
  onlyIfNewer,
  reboots,
  reloadsProxy,
  shownName,
  startsStopped,
} from "./plan";
import { namesARange, plain, type RolloutLine, useChangeWords } from "./words";

/** One thing to know about a plan before applying it. */
export interface Notice {
  key: string;
  /** The fact, short: "web gets root on hz-1". */
  title: ReactNode;
  /** One short sentence: what to do about it, where something resolves it. */
  detail?: ReactNode;
  /** Why, and how it comes about, for whoever opens "How this works". */
  more?: ReactNode;
  /** It comes down to what the resource runs, which only whoever applies it can judge. */
  trust?: boolean;
  /** Something to know that someone may well have chosen, so it stands out, and isn't an alarm. */
  notice?: boolean;
  /** A risk of how this goes, not of what it does, so it's amber beside what's red. */
  amber?: boolean;
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

/** Whether applying a plan asks for a passkey, as the control plane decides it: anything as good as root does. */
export function needsPasskey(plan: Plan | undefined): boolean {
  // An install's maintainer scripts run as root, which its plan says by what it installs.
  return plan !== undefined && (accessOf(plan).some(asGoodAsRoot) || installs(plan) || reboots(plan));
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
  [HostAccessKind.EXTERNAL_VOLUME]: 3,
  [HostAccessKind.TAKES_ON]: 3,
  [HostAccessKind.BACKUP_OF]: 0,
  [HostAccessKind.SERVER_SHELL]: 0,
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
    const key = `${name}/${server}/${access.kind}/${access.path}/${access.address}/${access.readOnly}/${access.holds}/${access.heldBy}/${access.detail}`;
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
    // Whose code writes what's read-write, and as whom (#247), in the control plane's
    // own words for it: root writes over what anyone owns there, and any image can hold
    // a program that runs as root, so code from another image asks again.
    const by = plain(access.writtenBy);
    const writes =
      access.kind === HostAccessKind.SERVER_PATH ||
      access.kind === HostAccessKind.VOLUME_MOUNT ||
      access.kind === HostAccessKind.EXTERNAL_VOLUME;
    const writer =
      !writes || access.readOnly || by === ""
        ? ""
        : access.detail === "root"
          ? t`${by} writes there as root, so it can change any file there, whoever owns it, and leave files that run as root.`
          : access.detail === "named"
            ? t`${by} writes there as a user by name, which only its image can say is root or not.`
            : access.detail === "image"
              ? t`${by} writes there as its own user, who is root unless the image says otherwise.`
              : t`${by} writes there. Code from another image asks again, as a program in any image can run as root.`;
    const andWriter = (detail: ReactNode): ReactNode =>
      writer ? (
        <>
          {detail} {writer}
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
              more: t`That goes beyond what Docker gives a container by default.`,
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
              more: t`That is the same as root if it is the group that owns Docker's socket on the server.`,
            };
      case HostAccessKind.EXTERNAL_NETWORK:
        return {
          key,
          title: leadsTo
            ? t`${name} joins the ${leadsTo} network ${path} on ${server}`
            : t`${name} joins the network ${path} on ${server}`,
          more: leadsTo
            ? t`Dockwell didn't make that network, and it reaches past the server's firewall. Whatever else is on it, other projects' services too, can reach ${name}, and it them.`
            : t`Dockwell didn't make that network. Whatever else is on it, other projects' services too, can reach ${name}, and it them.`,
        };
      case HostAccessKind.EXTERNAL_VOLUME:
        return {
          key,
          title: t`${name} mounts the volume ${path} on ${server}`,
          more: andWriter(
            t`Dockwell didn't make that volume, and won't delete it. Whatever else mounts it shares what it holds.`,
          ),
        };
      case HostAccessKind.SERVER_SHELL:
        return {
          key,
          title: t`${name} runs a shell command as root on ${server}`,
          more: t`It runs on its schedule, and whenever someone runs it now. Whatever it runs, it runs with everything root can do there.`,
        };
      case HostAccessKind.BACKUP_OF:
        return {
          key,
          title: t`${name} gets back what reached root on ${server}`,
          detail: (
            <Trans>
              This backup was taken when the volume reached <code>{path}</code>, so whatever mounts it can read what
              comes back.
            </Trans>
          ),
          trust: true,
        };
      case HostAccessKind.TAKES_ON:
        return {
          key,
          title: t`${name} takes on the container ${path} on ${server}`,
          more: t`Its environment comes into Dockwell with it, sealed. No plan or log shows a value of it, and it keeps running.`,
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
          ...(namesARange(access.path)
            ? {
                detail: t`Name a network the proxy has to itself.`,
                more: t`It takes that proxy's word for where a request came from. Whatever else is in that range can reach the route, and say who its client is.`,
              }
            : { more: t`It takes that proxy's word for where a request came from.` }),
        };
      case HostAccessKind.PLAIN_FOR_ANYONE:
        return {
          key,
          title: t`${name} answers plain HTTP on ${path} on ${server}`,
          more: t`Anyone who can reach this server gets it, around whatever is in front of it.`,
        };
      case HostAccessKind.HTTPS_OFF:
        return {
          key,
          title: t`${name} stops HTTPS on ${path} on ${server}`,
          more: t`https:// addresses get an error, and browsers that saw HSTS from it refuse plain HTTP until that runs out.`,
        };
      case HostAccessKind.HOST_NETWORK:
        return {
          key,
          title: t`${name} shares ${server}'s own network`,
          more: t`It reaches what the server reaches, past Docker's network and the firewall.`,
          trust: true,
        };
      case HostAccessKind.SERVER_PATH: {
        const [held, holdsFor, heldIn, heldBy] = [access.holds, access.holdsFor, access.heldIn, access.heldBy].map(
          plain,
        );
        if (held !== "") {
          const holder = path;
          return {
            key,
            title: t`${name} can move what ${held} leads to on ${server}`,
            detail:
              holdsFor !== ""
                ? t`It can write to ${holder}, which holds ${held}, which ${holdsFor} mounts.`
                : t`It can write to ${holder}, which holds ${held}, which it mounts as well.`,
            more:
              holdsFor !== "" ? (
                <Trans>
                  It can write to <code>{holder}</code>, which holds <code>{held}</code>, which {holdsFor} mounts. So it
                  can turn that into a link to anywhere on the server, which {holdsFor} follows when it next starts.
                  That's as good as root, so from now on any change to {name} but another tag asks for a passkey.
                </Trans>
              ) : (
                <Trans>
                  It can write to <code>{holder}</code>, which holds <code>{held}</code>, which it mounts as well. So it
                  can turn that into a link to anywhere on the server, which it follows when it next starts. That's as
                  good as root, so from now on any change to {name} but another tag asks for a passkey.
                </Trans>
              ),
            trust: true,
          };
        }
        if (heldBy !== "") {
          return {
            key,
            title: t`${heldBy} can move what ${path} leads to on ${server}`,
            detail: t`${name} mounts ${path}, inside ${heldIn}, which ${heldBy} can write.`,
            more: (
              <Trans>
                {name} mounts <code>{path}</code>, inside <code>{heldIn}</code>, which {heldBy} can write. So {heldBy}{" "}
                can turn it into a link to anywhere on the server, which {name} follows when it next starts. That's as
                good as root, so from now on any change to {heldBy} but another tag asks for a passkey.
              </Trans>
            ),
            trust: true,
          };
        }
        return asGoodAsRoot(access)
          ? {
              key,
              title: leadsTo
                ? t`${name} reaches ${path} on ${server}, which leads to ${leadsTo}`
                : t`${name} reaches ${path} on ${server}`,
              more: andRelabel(
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
              title: leadsTo
                ? t`${name} reaches ${path} on ${server}, which leads to ${leadsTo}`
                : t`${name} reaches ${path} on ${server}`,
              // Whether it writes there is the fact; who writes, and a relabel, are how.
              detail: access.readOnly
                ? t`It can read that path on the server.`
                : t`It can read and write that path on the server.`,
              more: [access.readOnly ? "" : writer, relabel].filter(Boolean).join(" ") || undefined,
            };
      }
    }
    const reach = words.access(access);
    const isDomain = access.kind === HostAccessKind.DOMAIN || access.kind === HostAccessKind.PANEL_DOMAIN;
    return {
      key,
      title: t`${name} reaches ${reach} on ${server}`,
      ...(isDomain
        ? {}
        : {
            more: asGoodAsRoot(access)
              ? t`The server's security rests on what that reaches, so it is as good as root.`
              : andWriter(t`That goes beyond what Docker keeps for it.`),
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

/** A note that a tool it can name still manages a container. */
const namesItsManager = (note: PlanNote) => note.reason === "managed_elsewhere" && (note.names[0] ?? "") !== "";

/** What a plan says of what else may run a container it takes on or replaces. */
function useManagedElsewhere(): (note: PlanNote, key: string) => Notice {
  const { t } = useLingui();
  return (note, key) => {
    const [tool = "", container = "", project = "", folder = ""] = note.names;
    const where = folder || t`the folder it runs from`;
    if (tool === "1Panel") {
      return {
        key,
        title: t`1Panel still manages ${container}`,
        detail: t`Leave it alone in 1Panel from now on, and never uninstall it there.`,
        more: t`It runs through the Compose project ${project}. Until Dockwell changes its image, anything done to it in 1Panel acts on this container. After that, 1Panel shows it in error, as it can't find the container. Leave that as it is: Start, Rebuild, Upgrade or editing its settings there starts a second copy, on the same folders and under the same name. Don't uninstall it in 1Panel, at either point: that deletes ${where}, which holds what this service mounts.`,
      };
    }
    return tool !== ""
      ? {
          key,
          title: t`${tool} still manages ${container}`,
          detail: t`Leave it alone in ${tool} from now on.`,
          more: t`It runs through the Compose project ${project}. Until Dockwell changes its image, what you do to it in ${tool} acts on this container. After that, starting or redeploying it there can start a second copy, on the same folders and under the same name. Before you remove it there, find out what that does to ${where}, as it may delete what this service mounts.`,
        }
      : {
          key,
          title: t`Something may still manage ${container}`,
          detail: t`Don't start its Compose project again.`,
          more: t`It came from the Compose project ${project}. Until Dockwell changes its image, whatever runs that acts on this container. After that, starting the project again starts a second copy, on the same folders and under the same name. If a panel or a script runs it, find out what its remove does to ${where} before you use it, as it may delete what this service mounts. If you ran docker compose yourself, don't run that file again.`,
        };
  };
}

/**
 * What a plan can't take back, worst first: what is as good as root on its
 * server, what data it takes away, and whether it may cut off access. They are
 * for the top of the plan, in the words the form used, and they are why it asks
 * for its fingerprint. They come from the plan as it is, so a plan undoing or
 * rolling back reads the same as a first deploy.
 */
/** What a restore replaces, and what stops for it, plainly: it's why the plan needs a person. */
function useRestoreNotices(): (restore: RestorePlan, name: string, key: string, server: string) => Notice[] {
  const { t, i18n } = useLingui();
  return (restore, name, key, server) => {
    const taken = restore.takenAt ? formatDateTime(timestampDate(restore.takenAt), i18n.locale) : "";
    const size = formatBytes(restore.sizeBytes, i18n.locale);
    const database = restore.database;
    const list = new Intl.ListFormat(i18n.locale, { style: "long", type: "conjunction" }).format(restore.stops);
    const out: Notice[] = [];
    if (restore.undoes !== "") {
      out.push({
        key: `restore-${key}`,
        title: t`Everything in ${name} on ${server} goes back to what was there before the restore`,
        detail: t`That's what the server kept on ${taken}, ${size}.`,
        more: t`What's in the volume now goes to a copy the server keeps for 7 days.`,
      });
    } else if (restore.method === BackupMethod.DUMP) {
      out.push({
        key: `restore-${key}`,
        title: t`Everything in ${name} on ${server} is replaced with the dump from ${taken}`,
        detail: t`${database} starts anew on an empty volume and loads the dump, ${size}, so what was made since goes.`,
        more: t`Passwords go back to what they were. If it won't load, what's there now comes back. The server keeps a copy of that for 7 days, and undoing this puts it back.`,
      });
    } else {
      out.push({
        key: `restore-${key}`,
        title: t`Everything in ${name} on ${server} is replaced with the copy from ${taken}`,
        detail: t`That copy is ${size}, and what was made since goes.`,
        more: t`The server keeps a copy of what's there now for 7 days, and undoing this puts it back.`,
      });
    }
    if (restore.method === BackupMethod.COPY_RUNNING) {
      out.push({
        key: `restore-running-${key}`,
        title: t`This copy was taken with the app running`,
        more: t`Files changed as they were read, so it can hold half a write, and the app may not start from it.`,
      });
    }
    if (restore.stops.length > 0) {
      out.push({
        key: `restore-stops-${key}`,
        title: plural(restore.stops.length, {
          one: `${list} stops while this happens`,
          other: `${list} stop while this happens`,
        }),
        more: t`What stops starts again once the volume is restored. If the restore fails, what's there now goes back first.`,
      });
    }
    if (restore.writtenBy !== "") {
      const { writtenBy, runsOn } = restore;
      out.push({
        key: `restore-release-${key}`,
        amber: true,
        title: t`${writtenBy} wrote these files, and ${runsOn} runs on them now`,
        detail: t`Deploy ${writtenBy} first, or restore a dump instead.`,
        more: t`The database may not start on files from another release.`,
      });
    }
    return out;
  };
}

export function useRisks(plan: Plan | undefined, applied = false): Notice[] {
  const { t } = useLingui();
  const restoreNotices = useRestoreNotices();
  const { root } = useReaches(plan);
  const managed = useManagedElsewhere();
  if (!plan) return [];
  // What a tool would delete leads, as acting on it there is what loses data, and it
  // can't wait below everything a plan of many services has to say.
  const out: Notice[] = [
    ...plan.notes.filter(namesItsManager).map((note, i) => managed(note, `managed-${i}`)),
    ...root,
  ];

  for (const server of plan.servers) {
    for (const step of server.steps) {
      const name = step.resource?.name ?? step.resourceId;
      const key = `${server.serverId}/${step.resourceId}`;
      if (step.restore) {
        out.push(...restoreNotices(step.restore, name, key, server.serverName));
      } else if (step.keptPaths.length > 0) {
        const paths = step.keptPaths.join(", ");
        out.push({
          key: `data-${key}`,
          title: t`${name} loses what its image keeps at ${paths}`,
          more: t`The server keeps that data for 7 days, and undoing this brings it back. After that it is gone.`,
        });
      } else if (step.destroysData) {
        out.push({
          key: `data-${key}`,
          title: t`${name} loses its data on ${server.serverName}`,
          detail: t`Back it up first if you need it: undo may not bring it back.`,
        });
      }
      // Only a person can say whether this data can take it, so it needs one.
      const rollout = step.rollout;
      if (rollout?.rolling && rollout.chosen) {
        const shared = [...rollout.volumes, ...rollout.paths, ...rollout.imageData, ...rollout.cantTell].join(", ");
        out.push({
          key: `writers-${key}`,
          // What it can do to the data is the fact to weigh, so it's in the title.
          title: t`Two containers may write to ${shared}, which can corrupt a database`,
          detail: t`Apply this only if the app can take it.`,
          more: t`${name}'s rollout order says to start first, so the old and new containers both write there for as long as this takes.`,
        });
      }
      if (step.warnings.includes(PlanWarning.MAY_LOCK_OUT) && !applied) {
        out.push({
          key: `lock-${key}`,
          title: t`This may cut off access to ${server.serverName}`,
          detail: t`Check that you have another way in, such as your provider's console, before you apply it.`,
        });
      }
      if (step.closes.length > 0) {
        const closes = step.closes.join(", ");
        out.push({
          key: `closes-${key}`,
          amber: true,
          title: t`Deny everything else closes what no rule lets in on ${server.serverName}`,
          detail: t`It closes ${closes}: add a rule for any you mean to keep open.`,
          more: t`These ports listen, or Docker publishes them, and no rule lets them in from anywhere. SSH stays open as this plan says, and on the control server its own ports stay open.`,
        });
      }
      // Who SSH stays open from, by name: a pin holds only while its address does.
      const stays = step.stays;
      if (stays && stays.sshPorts.length > 0 && !applied) {
        const from = [...stays.pinned, ...stays.allowed].join(", ");
        const ports = stays.sshPorts.join(", ");
        out.push({
          key: `stays-${key}`,
          amber: true,
          title: from
            ? t`SSH on ${server.serverName} stays open only from ${from}`
            : t`SSH on ${server.serverName} stays open from nowhere`,
          detail: t`To keep it open from more, add a rule that allows port ${ports} first.`,
          more: t`If your address isn't one of these, or stops being one, SSH closes to you, and no timer catches that later.`,
        });
      }
      if (stays && stays.tunnels.length > 0 && !applied) {
        const tunnels = stays.tunnels.join(", ");
        out.push({
          key: `tunnels-${key}`,
          amber: true,
          title: t`No rule allows the WireGuard tunnel ${tunnels} on ${server.serverName}`,
          detail: t`Keep a rule that allows the tunnel's UDP port.`,
          more: t`Deny everything else drops the tunnel's next handshake, and SSH over it with it, though it passes the test while the tunnel is up.`,
        });
      }
      // A jail bans whoever keeps failing to sign in, whoever they are: the
      // person applying it too, and for what they failed before it applied.
      if (step.resource?.kind === "fail2ban.jail" && step.action !== PlanAction.DELETE && !applied) {
        const jail = step.jail;
        const never = (jail?.neverBans ?? []).join(", ");
        const exposed = (jail?.signedInNotIgnored ?? []).join(", ");
        // Undo puts the jail back as it was, and a ban stays where fail2ban made it.
        const lifting = t`Undoing this, or removing the jail, doesn't lift a ban already made: sign in from another address or your provider's console, and run fail2ban-client unban with the address.`;
        const counts = t`fail2ban counts the failures from just before this applies, so a ban can land as it does.`;
        if (jail?.ignoresEverything) {
          out.push({
            key: `bans-${key}`,
            amber: true,
            title: t`This jail ignores every address on ${server.serverName}, so it bans nobody`,
            detail: t`Take out the range that covers everything for it to ban anyone.`,
            more: t`It never bans ${never}.`,
          });
        } else if (exposed) {
          out.push({
            key: `bans-${key}`,
            amber: true,
            title: t`This jail can ban ${exposed}, signed in to ${server.serverName} over SSH now`,
            detail: t`Add it to what the jail never bans first, or apply this knowing it.`,
            more: t`It never bans ${never}. ${counts} ${lifting}`,
          });
        } else if (jail) {
          out.push({
            key: `bans-${key}`,
            amber: true,
            title: t`This jail never bans ${never} on ${server.serverName}`,
            detail: t`It bans any other address that keeps failing to sign in, yours included.`,
            more: t`${counts} ${lifting}`,
          });
        } else {
          out.push({
            key: `bans-${key}`,
            amber: true,
            title: t`This jail can ban your own address on ${server.serverName}`,
            detail: t`It never bans the server itself or the control server.`,
            more: t`${counts} ${lifting}`,
          });
        }
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
 * What a failed plan left in the data, when it took no backup: a rollback puts back
 * what runs, and whatever ran meanwhile may have changed what that runs on.
 */
export function useDataLeft(
  plan: Plan | undefined,
  progress: readonly StepProgress[],
): Omit<Notice, "key"> | undefined {
  const { t } = useLingui();
  // A backup changes no data, and one that failed stopped everything after it.
  const backups = (plan?.servers ?? []).flatMap((server) =>
    server.steps.filter((step) => step.backupFirst).map((step) => `${server.serverId}/${step.resourceId}`),
  );
  const ranPast = progress.some((step) => step.startTime && !backups.includes(`${step.serverId}/${step.resourceId}`));
  const backedUp = plan?.notes.some((note) => note.reason === "backup_first");
  if (backedUp && !ranPast) return undefined;
  if (backedUp || plan?.notes.some((note) => note.reason === "undo_backup_first")) {
    return {
      title: t`The data stays as it is now`,
      detail: t`Check that the app reads its data, and restore the backup taken first if it doesn't.`,
      more: t`What ran before this failed may have changed the data, such as with a migration, and the version that runs now may refuse it or misread it.`,
    };
  }
  if (!plan?.notes.some((note) => ["no_backup", "undo_no_backup", "unbacked_move"].includes(note.reason)))
    return undefined;
  return {
    title: t`No backup was taken, and the data stays as it is now`,
    detail: t`Check that the app reads its data, and restore a backup of your own if it doesn't.`,
    more: t`What ran before this failed may have changed the data, such as with a migration, and the version that runs now may refuse it or misread it.`,
  };
}

/**
 * What a person should know before applying a plan, which stops nobody: a folder
 * of the server's that an app reaches, a rollback leaving a service's data as it
 * is, or a Compose variable written where anyone can read it. The API says a
 * note in its own message; the panel has its own words for the reasons it knows,
 * so they translate, and leads with the fact of any it doesn't.
 */
export function useWarnings(plan: Plan | undefined, applied = false, serving: readonly string[] = []): Notice[] {
  const { t, i18n } = useLingui();
  const managed = useManagedElsewhere();
  const words = useChangeWords();
  const { rest } = useReaches(plan);
  const rollouts = useRolloutLines(plan);
  // Going back is when this is news. Any other plan says it among what it means.
  const rollingBack = plan?.intent?.kind === IntentKind.ROLL_BACK;
  // One a tool still manages leads the plan, as a person acts on it first.
  const worthWarning = (plan?.notes ?? [])
    .filter((note) => (note.reason !== "volumes_stay" || rollingBack) && !namesItsManager(note))
    .sort((a, b) => Number(b.reason === "managed_elsewhere") - Number(a.reason === "managed_elsewhere"));
  const notes = worthWarning.map((note, i): Notice => {
    const key = `note-${i}`;
    const name = note.resource?.name ?? "";
    switch (note.reason) {
      case "managed_elsewhere":
        return managed(note, key);
      case "shares_a_name": {
        const [shared = "", network = "", ...others] = note.names;
        const also = others.join(", ");
        return {
          key,
          title: t`More than one container answers to ${shared} on ${network}`,
          detail: t`This one and ${also} do: point what uses it at a name only one of them has.`,
          more: t`What asks for ${shared} there can reach either.`,
        };
      }
      case "volumes_stay": {
        const names = note.names.join(", ");
        return {
          key,
          title: t`${name} keeps what is in ${names} as it is`,
          detail: applied
            ? t`Check that it reads its data, and restore a backup if it won't.`
            : t`Check that it will before you apply, and restore a backup if it won't.`,
          more: t`A rollback puts back its spec, not its data, so if a newer version changed that data, such as with a migration, the older one may not read it.`,
        };
      }
      case "first_steps":
        return { key, title: t`What to do first`, detail: note.message };
      case "backup_first":
        return {
          key,
          title: t`Its volumes are backed up first`,
          more: t`If a backup fails, the upgrade stops there, with nothing else done. Undoing this puts the old version back, but not what the new one changed in its data: restore the backup for that.`,
        };
      case "backup_unmeasured":
        return {
          key,
          title: t`The hour ${name}'s backup gets is a guess`,
          detail: t`Back it up once from Backups, and the next plan goes by how large that was.`,
          more: t`It took too long to measure. If an hour is too short, this stops there, with nothing else done.`,
        };
      case "undo_backup_first": {
        const [taken = "", ...ids] = note.names;
        const which = new Intl.ListFormat(i18n.locale, { style: "long", type: "conjunction" }).format(ids);
        const when = taken ? formatDateTime(new Date(taken), i18n.locale) : "";
        const gone = ids.length === 0;
        return {
          key,
          title: t`The old version may not read its data`,
          detail: applied
            ? gone
              ? t`Check that the app reads its data.`
              : t`Check that the app reads its data, and if it doesn't, restore ${which}, from ${when}.`
            : gone
              ? t`Back up what you can't lose before this.`
              : t`Restore ${which}, from ${when}, from Backups after this.`,
          more: applied
            ? gone
              ? t`This put the old version back, not its data. If the new version changed that data, such as with a migration, the old one may refuse it or misread it. The backup the upgrade took first is gone.`
              : t`This put the old version back, not its data. If the new version changed that data, such as with a migration, the old one may refuse it or misread it. The upgrade backed it up first.`
            : gone
              ? t`This puts the old version back, not its data. If the new version changed that data, such as with a migration, the old one may refuse it or misread it. The backup the upgrade took first is gone.`
              : t`This puts the old version back, not its data. If the new version changed that data, such as with a migration, the old one may refuse it or misread it. The upgrade backed it up first.`,
        };
      }
      case "no_backup":
        return {
          key,
          title: t`No backup is taken first`,
          detail: t`Back up what you can't lose first.`,
          more: t`You asked for none. Undoing this puts the old version back, but not what the new one changed in its data, such as a database it migrated.`,
        };
      case "raises_features": {
        const [from = "", to = ""] = note.names;
        return {
          key,
          title: t`${name}'s features go up to ${from} before it moves to ${to}`,
          detail: t`Undoing this puts back ${from}. The release before it can't run the data.`,
          more: t`MongoDB moves only from data at the release it leaves, so its feature compatibility version goes up to ${from} first, and its data may then use ${from}'s features.`,
        };
      }
      case "tasks_as_root": {
        const tasks: string[] = [];
        for (let i = 0; i + 1 < note.names.length; i += 2) tasks.push(`${note.names[i]}: ${note.names[i + 1]}`);
        const list = tasks.join("; ");
        return {
          key,
          title: t`${name}'s scheduled tasks get the same reach`,
          detail: t`They are ${list}.`,
          more: t`They run in what ${name} runs, so they reach what it reaches on its server too.`,
        };
      }
      case "lowers_release": {
        const [, to = "", features = ""] = note.names;
        return features
          ? {
              key,
              title: t`${name}'s data uses ${features}'s features, which ${to} can't run`,
              detail: t`Lower its feature compatibility version to ${to} first.`,
              more: t`Its last reading says so. Applying this stops before it moves, with nothing changed.`,
            }
          : {
              key,
              title: t`${name} moves back to ${to} only if its data allows`,
              detail: t`Lower its feature compatibility version to ${to} first if it's higher.`,
              more: t`Before it moves, Dockwell asks what features its data uses, and stops with nothing changed if ${to} can't run them.`,
            };
      }
      case "unbacked_move":
        return {
          key,
          title: t`${name}'s data isn't backed up first`,
          detail: t`Back it up yourself before you apply this.`,
          more: t`It isn't in a volume Dockwell made. Undoing this puts the old release back, not what the new one changed in its data.`,
        };
      case "tmpfs_no_size": {
        const [target = ""] = note.names;
        return {
          key,
          title: t`${name}'s tmpfs at ${target} has no size`,
          detail: t`Give it one, such as ${target}:size=64m, or give ${name} a memory limit.`,
          more: t`It can take half the server's memory.`,
        };
      }
      case "undo_no_backup":
        return {
          key,
          title: t`The old version may not read its data`,
          detail: applied
            ? t`Check that the app reads its data, and restore a backup of your own if it doesn't.`
            : t`Back up what you can't lose first, or stay on the new version.`,
          more: applied
            ? t`This put the old version back, not its data. If the new version changed that data, such as with a migration, the old one may refuse it or misread it, and no backup was taken.`
            : t`This puts the old version back, not its data. If the new version changed that data, such as with a migration, the old one may refuse it or misread it, and no backup was taken.`,
        };
      case "values_latest": {
        const names = note.names.join(", ");
        return {
          key,
          title: t`${name} runs with the latest value of ${names}`,
          detail: t`To go back on a value, change it in Variables and secrets.`,
          more: t`A rollback puts back which variables and secrets it uses, not what they held then.`,
        };
      }
      case "kept_logs_offline": {
        const [server = ""] = note.names;
        return {
          key,
          title: t`${server} is offline, so what it kept of ${name} stays on its disk until it's back`,
          more: t`It can't be read meanwhile, and it goes once the server is back and the 7 days are up.`,
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
          // What it deletes is the fact to weigh, so it's in the title.
          title:
            deleted !== ""
              ? t`${project} already runs ${names}, and this deletes ${deleted}`
              : t`${project} already runs ${names}`,
          detail: t`If that isn't what you meant, change name: in the file to give it a project of its own.`,
          more:
            deleted !== ""
              ? t`A service of the same name in the file replaces it, and one the file doesn't name is deleted.`
              : t`A service of the same name in the file replaces it.`,
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
      case "catalog_source_refuses": {
        // The catalog's own check says why, in its words, after the file it's about.
        const [file = ""] = note.names;
        const at = note.message.indexOf(": ");
        const why = at < 0 ? note.message : note.message.slice(at + 2);
        return { key, title: t`Leaves out ${file}`, detail: why };
      }
      case "catalog_source_takes_away": {
        const names = note.names.join(", ");
        return {
          key,
          title: t`Takes away ${names}`,
          more: t`Installs of them keep running, with no upgrades.`,
        };
      }
      case "catalog_source_removed":
        return {
          key,
          title: t`Its apps leave the catalog`,
          more: t`Installs of them keep running, with no upgrades.`,
        };
      // A host such as GitHub serves a fork's commits through the repository it was forked from.
      case "catalog_source_unnamed":
        return {
          key,
          title: t`No branch or tag is at this commit now`,
          detail: t`Check it's the commit you mean.`,
          more: t`It may be an older commit, or a fork's.`,
          notice: true,
        };
      case "catalog_source_unlisted":
        return {
          key,
          title: t`Dockwell couldn't list the repository's branches and tags`,
          detail: t`Check it's the commit you mean.`,
          notice: true,
        };
      case "catalog_source_takes": {
        const ids = note.names.join(", ");
        return {
          key,
          title: t`Takes ${ids} from another catalog`,
          more: t`This catalog comes first, so the catalog shows its apps.`,
        };
      }
      case "catalog_source_gives": {
        const ids = note.names.join(", ");
        return {
          key,
          title: t`${ids} will come from another catalog`,
          more: t`Nothing installed from this catalog upgrades to them.`,
        };
      }
      case "catalog_source_strands": {
        const projects = note.names.join(", ");
        return {
          key,
          title: t`${projects} will have no upgrades`,
          more: t`Each keeps running as it is.`,
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
          ...(field === "command" || field === "entrypoint"
            ? {
                detail: t`To keep it hidden, write the ${where} as a list, with each word its own variable.`,
                more: t`Anyone who can read this app can see it. Its value has a space, a quote or another character Compose reads as part of the line, so one name can't stand for it.`,
              }
            : field === "environment"
              ? {
                  detail: t`Don't write a secret in a variable's name.`,
                  more: t`Anyone who can read this app can see it. Only a variable's value is hidden.`,
                }
              : {
                  detail: t`If it's a secret, set it in environment for the app to read instead.`,
                  more: t`Anyone who can read this app can see it, as it can't be hidden there.`,
                }),
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
  // And what an edit kept of an app's own values: information, which an aside says once.
  const quiet = new Set([
    "pulls_again",
    "leaves_pin",
    "starts_stopped",
    "keeps_env",
    "catalog_source_reads",
    "catalog_source_adds",
    "catalog_source_changes",
    "catalog_source_commit",
    "catalog_source_restores",
  ]);
  const fromRollouts = rollouts
    .filter((line) => line.warn)
    .map(
      (line): Notice => ({
        key: line.key,
        title: line.lead ?? line.text,
        detail: line.lead ? `${line.text}.` : undefined,
      }),
    );
  return [
    ...rest,
    ...fromRollouts,
    ...routes,
    ...notes.filter((_, i) => {
      const note = worthWarning[i];
      // Older source plans carry only a sentence, including unnamed-commit warnings.
      if (note?.reason === "catalog_source_commit" && note.names.length === 0) return true;
      return !quiet.has(note?.reason ?? "");
    }),
  ];
}

/**
 * What is worth knowing beyond the diff, quietly, from the plan's steps: data
 * it brings back, and a health check it can't know yet.
 */
export function useAsides(plan: Plan | undefined): Notice[] {
  const { t } = useLingui();
  const words = useChangeWords();
  const detected = useDetectionWords();
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
    // What an edit kept unread of an app's own values, as the server says it, once for each app:
    // the server notes it for each server the app's step is on.
    if (note.reason === "keeps_env") {
      const key = `keeps-${note.resource?.scope ?? ""}/${name}`;
      if (!out.some((n) => n.key === key)) out.push({ key, title: `${note.message}.` });
    }
    // What a catalog's commit holds is what a person reviews it for, and no warning.
    if (note.reason === "catalog_source_reads") {
      const count = note.names.length;
      const names = note.names.join(", ");
      out.push({
        key: `catalog-reads-${i}`,
        title:
          count === 0
            ? t`The catalog shows no apps from this commit.`
            : plural(count, {
                one: `The catalog shows # app from this commit: ${names}.`,
                other: `The catalog shows # apps from this commit: ${names}.`,
              }),
      });
    }
    if (note.reason === "catalog_source_adds") {
      const names = note.names.join(", ");
      out.push({ key: `catalog-adds-${i}`, title: t`Adds ${names}.` });
    }
    if (note.reason === "catalog_source_changes") {
      const names = note.names.join(", ");
      out.push({
        key: `catalog-moves-${i}`,
        title: note.names.length ? t`Moves ${names} on, which an install of each can upgrade to.` : note.message,
      });
    }
    // Which branches and tags are at the commit, as refs/heads/main and refs/tags/v1.
    if (note.reason === "catalog_source_commit" && note.names.length > 0) {
      const at = note.names
        .map((ref) =>
          ref.startsWith("refs/tags/")
            ? t`the tag ${ref.slice("refs/tags/".length)}`
            : t`the branch ${ref.slice("refs/heads/".length)}`,
        )
        .join(", ");
      out.push({
        key: `catalog-commit-${i}`,
        title: plural(note.names.length, {
          one: `It's the commit ${at} is at.`,
          other: `It's the commit ${at} are at.`,
        }),
      });
    }
    if (note.reason === "catalog_source_restores") {
      const projects = note.names.join(", ");
      out.push({
        key: `catalog-restores-${i}`,
        title: t`${projects} can upgrade again, from the catalog each came from.`,
      });
    }
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
      if (step.build) {
        // What a step builds comes before anything about it stops (ADR 0046).
        const repository = step.build.url.replace(/^https?:\/\//, "").replace(/\.git$/, "");
        const commit = step.build.commit.slice(0, 7);
        const ref = shownRef(step.build.ref);
        // What reading the commit found, when the plan read it: its Dockerfile, or
        // what Railpack makes of it, which is what it builds.
        const d = step.build.detection;
        const railpack = d?.builder === ImageBuild_Builder.RAILPACK;
        out.push({
          key: `build-${key}`,
          title: ref
            ? t`Builds ${repository} at ${commit} (${ref}) on ${server.serverName} first. If the build fails, what runs stays as it is.`
            : t`Builds ${repository} at ${commit} on ${server.serverName} first. If the build fails, what runs stays as it is.`,
        });
        // What it builds with, on a line of its own after the build.
        // One span, so its words and code flow as a sentence in the note's row.
        const builtWith = d ? (
          railpack ? (
            <span>
              <Trans>With Railpack, as {detected.what(d)}.</Trans> {detected.steps(d)}
              {detected.packages(d).length > 0 ? ` ${t`Installs ${detected.packages(d).join("; ")}.`}` : null}
            </span>
          ) : d.dockerfile === "Dockerfile" ? (
            d.baseImage ? (
              <span>
                <Trans>
                  With its Dockerfile, from <code>{d.baseImage}</code>.
                </Trans>
              </span>
            ) : (
              <Trans>With its Dockerfile.</Trans>
            )
          ) : d.baseImage ? (
            <span>
              <Trans>
                With its Dockerfile at <code>{d.dockerfile}</code>, from <code>{d.baseImage}</code>.
              </Trans>
            </span>
          ) : (
            <span>
              <Trans>
                With its Dockerfile at <code>{d.dockerfile}</code>.
              </Trans>
            </span>
          )
        ) : step.build.builder === ImageBuild_Builder.RAILPACK ? (
          <Trans>With Railpack, which reads the commit as the build starts.</Trans>
        ) : undefined;
        if (builtWith) out.push({ key: `build-with-${key}`, title: builtWith });
        for (const warning of d && railpack ? detected.warnings(d) : []) {
          out.push({ key: `build-warns-${key}-${warning}`, title: t`Railpack says: ${warning}`, notice: true });
        }
      } else if (step.healthUnknown) {
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
