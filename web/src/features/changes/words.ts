// SPDX-License-Identifier: AGPL-3.0-only

import {
  ActorKind,
  ChangeState,
  type DomainCheck,
  DomainCheckState,
  type HostAccess,
  HostAccessKind,
  IngressChange,
  type IngressEffect,
  type Intent,
  IntentKind,
  PlanAction,
  type RolloutPlan,
  type RouteAnswers,
  type RouteGoes,
  type RouteMove,
  RouteReach,
  type ServerPortCheck,
  StepEffect,
  StepState,
  Surface,
  VisitorsFind,
} from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";
import { useCallback, useMemo } from "react";

import { isPrivateAddress } from "../servers/server";
import { type Fold, kindWord } from "./plan";
import { upgradeTo } from "./upgrade";

/** Things as a list, "a, b and c", as the CLI writes it. British English leaves out the serial comma, as these words do. */
function listWords(locale: string, items: readonly string[], type: Intl.ListFormatType = "conjunction"): string {
  const words = locale.startsWith("en") ? "en-GB" : locale;
  return new Intl.ListFormat(words, { style: "long", type }).format(items);
}

/** How a step is going, as a status and a word. */
export interface StepLook {
  status: Status;
  word: string;
}

/** Where a step is, which says how its verb reads. */
export type Tense = "plan" | "doing" | "done" | "failed";

/** The words the plan sheet and Activity share, in the active locale. */
export interface ChangeWords {
  /** A resource's kind, as a word after its name: "project". */
  kind(kind: string): string;
  /** What a step does, in the tense of where it is: "Create", "Creating", "Created", "Couldn't create". */
  verb(action: PlanAction, tense: Tense): string;
  /** Taking on a container Dockwell didn't make, in place of making one. */
  takesOn(tense: Tense): string;
  /** Handing one back, in place of deleting it. */
  letsGo(tense: Tense): string;
  /** What a change is for, in the tense of where it is: "Deploy nginx:1.29", "Deploying nginx:1.29", "Couldn't deploy nginx:1.29". */
  intent(intent: Intent, tense: Tense): string;
  /** What a change is for, as its row's tag: "Deploy", "Deployed". */
  intentTag(intent: Intent, done: boolean): string;
  /** What a step will do to what the resource serves: "Restarts". Empty for nothing. */
  effect(effect: StepEffect): string;
  step(state: StepState): StepLook;
  /** Where a change stands, as a status and a word, or undefined for an applied one, which stays quiet. */
  state(state: ChangeState): StepLook | undefined;
  /** Where something came from, as a value: "The panel". */
  surface(surface: Surface): string;
  /** Where something came from, as a line of its own: "From the panel". */
  via(surface: Surface): string;
  /** What kind of actor someone is: "API token". Empty for a person. */
  actorKind(kind: ActorKind): string;
  /** What a container reaches on its server: "the server path /srv/app (read-only)". */
  access(access: HostAccess): string;
  /** What a route's step does to its server's ingress, a sentence each. Empty for nothing. */
  ingress(effect: IngressEffect | undefined, server: string): string[];
  /** What changing a route costs the sites on a proxy that stays up, as it reloads. */
  reload(server: string): string;
  /** What would take the proxy's ports back at the next reboot, and how to keep it from happening. It never stops a plan. */
  returning(names: string[], server: string): string;
  /** What DNS said of a route's domain as this was planned, if that is worth saying. It never stops a plan. */
  domain(check: DomainCheck | undefined): string;
  /** What a route that takes no certificate means, as TLS ends before its server. It never stops a plan. */
  unencrypted(server: string): string;
  /** What someone visiting a route finds once a step takes it away. */
  goes(goes: RouteGoes): string;
  /** A route leaving the app it sent to, which is easy to miss among a plan's fields. */
  moves(moves: RouteMove): string;
  /** Who a route answers, when it isn't anyone over HTTPS sent to a service, a sentence each. */
  answers(answers: RouteAnswers | undefined, port: ServerPortCheck | undefined, server: string): string[];
  /** How a step replaces what runs, and what that means, a sentence each. `span` words a duration in milliseconds. */
  rollout(
    plan: RolloutPlan | undefined,
    span: (ms: number) => string,
    restart?: boolean,
    stopped?: boolean,
  ): RolloutLine[];
}

/** One thing a plan says of a rollout. */
export interface RolloutLine {
  text: string;
  /** Worth reading before applying, so it goes with the plan's warnings. */
  warn: boolean;
  /** The fact, short, where a warning leads with one and goes on with `text`. */
  lead?: string;
  /** Something to know that stands out, and isn't an alarm. */
  notice?: boolean;
}

function millis(d: { seconds: bigint; nanos: number } | undefined): number {
  return d ? Number(d.seconds) * 1000 + d.nanos / 1e6 : 0;
}

/**
 * True for a proxy that covers more than one address, so what else is on that network
 * can be it. A lone address is written as a range of one, and a preset has its name.
 * The server says the same in what it words for the CLI.
 */
export function namesARange(proxy: string): boolean {
  const [, bits] = /^[0-9a-f:.]+\/(\d+)$/i.exec(proxy) ?? [];
  return bits !== undefined && bits !== (proxy.includes(":") ? "128" : "32");
}

/** Quotes what could read as the plan's own words, as AccessWords does on the server. */
export function plain(s: string): string {
  return /^[A-Za-z0-9_.,/=:@+*-]*$/.test(s) ? s : JSON.stringify(s);
}

export function useChangeWords(): ChangeWords {
  const { t, i18n } = useLingui();
  return useMemo<ChangeWords>(
    () => ({
      kind(kind) {
        switch (kind) {
          case "project":
            return t`project`;
          case "environment":
            return t`environment`;
          case "service":
            return t`app`;
          case "container":
            return t`container`;
          case "volume":
            return t`volume`;
          case "network":
            return t`network`;
          case "route":
            return t`route`;
          case "firewall.rule":
            return t`firewall rule`;
          case "variable":
            return t`variable`;
          case "secret":
            return t`secret`;
        }
        return kindWord(kind);
      },
      verb(action, tense) {
        switch (action) {
          case PlanAction.CREATE:
            return { plan: t`Create`, doing: t`Creating`, done: t`Created`, failed: t`Couldn't create` }[tense];
          case PlanAction.UPDATE:
            return { plan: t`Update`, doing: t`Updating`, done: t`Updated`, failed: t`Couldn't update` }[tense];
          case PlanAction.REPLACE:
            return { plan: t`Replace`, doing: t`Replacing`, done: t`Replaced`, failed: t`Couldn't replace` }[tense];
          case PlanAction.DELETE:
            return { plan: t`Delete`, doing: t`Deleting`, done: t`Deleted`, failed: t`Couldn't delete` }[tense];
        }
        return { plan: t`Check`, doing: t`Checking`, done: t`Checked`, failed: t`Couldn't check` }[tense];
      },
      takesOn(tense) {
        return { plan: t`Take on`, doing: t`Taking on`, done: t`Took on`, failed: t`Couldn't take on` }[tense];
      },
      letsGo(tense) {
        return { plan: t`Let go of`, doing: t`Letting go of`, done: t`Let go of`, failed: t`Couldn't let go of` }[
          tense
        ];
      },
      intent(intent, tense) {
        const { image, port } = intent;
        const revision = `r${intent.revision}`;
        switch (intent.kind) {
          case IntentKind.DEPLOY:
            if (intent.commit) {
              // A deploy of an app that builds from Git builds a commit, which is what it deploys (ADR 0046).
              const commit = intent.commit.slice(0, 7);
              return {
                plan: t`Build and deploy ${commit}`,
                doing: t`Building and deploying ${commit}`,
                done: t`Deployed ${commit}`,
                failed: t`Couldn't deploy ${commit}`,
              }[tense];
            }
            return {
              plan: t`Deploy ${image}`,
              doing: t`Deploying ${image}`,
              done: t`Deployed ${image}`,
              failed: t`Couldn't deploy ${image}`,
            }[tense];
          case IntentKind.PUBLISH_PORT:
            return {
              plan: t`Publish port ${port}`,
              doing: t`Publishing port ${port}`,
              done: t`Published port ${port}`,
              failed: t`Couldn't publish port ${port}`,
            }[tense];
          case IntentKind.ROLL_BACK:
            return {
              plan: t`Roll back to ${revision}`,
              doing: t`Rolling back to ${revision}`,
              done: t`Rolled back to ${revision}`,
              failed: t`Couldn't roll back to ${revision}`,
            }[tense];
          case IntentKind.REPULL:
            return {
              plan: t`Pull ${image} again`,
              doing: t`Pulling ${image} again`,
              done: t`Pulled ${image} again`,
              failed: t`Couldn't pull ${image}`,
            }[tense];
          case IntentKind.UPGRADE: {
            // What a person names a version by: a catalog install's release, or the image's tag (ADR 0080).
            const to = upgradeTo(intent);
            return {
              plan: t`Update to ${to}`,
              doing: t`Updating to ${to}`,
              done: t`Updated to ${to}`,
              failed: t`Couldn't update to ${to}`,
            }[tense];
          }
        }
        return "";
      },
      intentTag(intent, done) {
        switch (intent.kind) {
          case IntentKind.DEPLOY:
            return done ? t`Deployed` : t`Deploy`;
          case IntentKind.PUBLISH_PORT:
            return done ? t`Published port` : t`Publish port`;
          case IntentKind.ROLL_BACK:
            return done ? t`Rolled back` : t`Roll back`;
          case IntentKind.REPULL:
            return done ? t`Pulled again` : t`Pull again`;
          case IntentKind.UPGRADE:
            return done ? t`Updated` : t`Update`;
        }
        return "";
      },
      effect(effect) {
        switch (effect) {
          case StepEffect.RESTART:
            return t`Restarts`;
          case StepEffect.RECREATE:
            return t`Recreates its container`;
          case StepEffect.ROLL:
            return t`Rolling, with nothing stopped`;
        }
        return "";
      },
      step(state) {
        switch (state) {
          case StepState.RUNNING:
            return { status: "deploying", word: t`Running` };
          case StepState.DONE:
            return { status: "healthy", word: t`Done` };
          case StepState.FAILED:
            return { status: "down", word: t`Failed` };
          case StepState.SKIPPED:
            return { status: "stopped", word: t`Skipped` };
          case StepState.WAITING:
            return { status: "degraded", word: t`Waiting` };
        }
        return { status: "stopped", word: t`Queued` };
      },
      state(state) {
        switch (state) {
          case ChangeState.PLANNED:
            return { status: "stopped", word: t`Planned` };
          case ChangeState.APPLYING:
            return { status: "deploying", word: t`Applying` };
          case ChangeState.FAILED:
            return { status: "down", word: t`Failed` };
          case ChangeState.DISCARDED:
            return { status: "stopped", word: t`Discarded` };
        }
        return undefined;
      },
      surface(surface) {
        switch (surface) {
          case Surface.PANEL:
            return t`The panel`;
          case Surface.API:
            return t`The API`;
          case Surface.CLI:
            return t`The CLI`;
          case Surface.AUTOMATIC:
            return t`Automatic`;
        }
        return t`Unknown`;
      },
      via(surface) {
        switch (surface) {
          case Surface.PANEL:
            return t`From the panel`;
          case Surface.API:
            return t`From the API`;
          case Surface.CLI:
            return t`From the CLI`;
          case Surface.AUTOMATIC:
            return t`Automatic`;
        }
        return "";
      },
      access(access) {
        const [path, type, driver, detail, leadsTo] = [
          access.path,
          access.type,
          access.driver,
          access.detail,
          access.leadsTo,
        ].map(plain);
        const reads = access.readOnly ? t`read-only` : t`read-write`;
        // Whose code writes what's read-write, and as whom where that's root or may be,
        // in the control plane's own words for it (#247). The repository is quoted
        // where it could read as more of the line, as a path is.
        const withWriter = (base: string): string => {
          const by = plain(access.writtenBy);
          if (by === "" || access.readOnly || access.sameAsRoot) return base;
          switch (access.detail) {
            case "root":
              return t`${base}, written by ${by} as root`;
            case "named":
              return t`${base}, written by ${by} as a user by name, which only its image can say is root or not`;
            case "image":
              return t`${base}, written by ${by} as its own user, who may be root`;
          }
          return t`${base}, written by ${by}`;
        };
        switch (access.kind) {
          case HostAccessKind.SERVER_PATH: {
            const [held, holdsFor, heldIn, heldBy] = [access.holds, access.holdsFor, access.heldIn, access.heldBy].map(
              plain,
            );
            if (held !== "") {
              return holdsFor !== ""
                ? t`the server path ${path}, which holds ${held}, which ${holdsFor} mounts, so what writes there can make that lead anywhere, as good as root. Any change to it but another tag asks for a passkey from now on`
                : t`the server path ${path}, which holds ${held}, which it mounts as well, so what writes there can make that lead anywhere, as good as root. Any change to it but another tag asks for a passkey from now on`;
            }
            if (heldBy !== "") {
              return t`the server path ${path}, inside ${heldIn}, which ${heldBy} can write, so it can make that lead anywhere, as good as root. Any change to ${heldBy} but another tag asks for a passkey from now on`;
            }
            let how = reads;
            if (access.relabel === "z") how = t`${reads}, relabelled for SELinux as shared`;
            if (access.relabel === "Z") how = t`${reads}, relabelled for SELinux as private`;
            const at = leadsTo
              ? t`the server path ${path}, which leads to ${leadsTo} (${how})`
              : t`the server path ${path} (${how})`;
            return withWriter(at);
          }
          case HostAccessKind.DOCKER_SOCKET:
            return leadsTo
              ? t`the server path ${path}, which leads to Docker's socket at ${leadsTo}, which gives control of Docker on the server, the same as root`
              : t`Docker's socket at ${path}, which gives control of Docker on the server, the same as root`;
          case HostAccessKind.HOST_NETWORK:
            return t`the server's own network`;
          case HostAccessKind.VOLUME_MOUNT: {
            if (driver !== "" && path !== "") {
              return withWriter(
                t`the volume driver ${driver} at ${path}, which reaches what the plugin does (${reads})`,
              );
            }
            if (driver !== "") {
              return withWriter(t`the volume driver ${driver}, which reaches what the plugin does (${reads})`);
            }
            const at = plain(access.address + access.path);
            return withWriter(at === "" ? t`a ${type} mount (${reads})` : t`the ${type} mount ${at} (${reads})`);
          }
          case HostAccessKind.NETWORK_DRIVER: {
            let out = path === "" ? t`a ${driver} network` : t`a ${driver} network on ${path}`;
            if (driver !== "bridge") out = t`${out}, which reaches past the server's firewall`;
            if (detail !== "") out = t`${out}, with ${detail}`;
            return out;
          }
          case HostAccessKind.DOMAIN:
            return t`the domain ${path}, which the environment ${detail} held within the last 7 days`;
          case HostAccessKind.PANEL_DOMAIN:
            return t`the panel's domain, ${path}, which moves behind ${detail}`;
          case HostAccessKind.EXTERNAL_NETWORK:
            if (leadsTo !== "") {
              return detail === ""
                ? t`the ${leadsTo} network ${path}, which reaches past the server's firewall and is on the server already, where it answers to its name: whatever else is on that network, other projects' services too, can reach it, and it can reach them`
                : t`the ${leadsTo} network ${path}, which reaches past the server's firewall and is on the server already, where it answers to its name and to ${detail}: whatever else is on that network, other projects' services too, can reach it, and it can reach them`;
            }
            return detail === ""
              ? t`the network ${path}, which is on the server already, where it answers to its name: whatever else is on that network, other projects' services too, can reach it, and it can reach them`
              : t`the network ${path}, which is on the server already, where it answers to its name and to ${detail}: whatever else is on that network, other projects' services too, can reach it, and it can reach them`;
          case HostAccessKind.EXTERNAL_VOLUME:
            return withWriter(
              t`the volume ${path}, which is on the server already and which Dockwell didn't make: whatever else mounts it shares what it holds`,
            );
          case HostAccessKind.BACKUP_OF:
            return t`a backup of a volume that reached ${path}, which can hold what's there, as good as root`;
          case HostAccessKind.SERVER_SHELL:
            return t`a shell command on the server, which runs as root`;
          case HostAccessKind.TAKES_ON:
            return t`the container ${path}, which Dockwell didn't make, and its environment, which comes in with it`;
          case HostAccessKind.CAPABILITY:
            return access.sameAsRoot
              ? t`the Linux capability ${path}, which is the same as root`
              : t`the Linux capability ${path}`;
          case HostAccessKind.GROUP:
            return access.sameAsRoot
              ? t`the group ${path}, which is the same as root`
              : t`the group ${path}, which is the same as root if it is the group that owns Docker's socket on the server`;
          case HostAccessKind.PRIVILEGED:
            return t`privileged mode, which gives it every device and capability on the server, the same as root`;
          case HostAccessKind.SERVER_PORT:
            return type === "http"
              ? t`the port ${path} on the server itself, past its containers, at ${detail} over plain HTTP`
              : t`the port ${path} on the server itself, past its containers, at ${detail}`;
          case HostAccessKind.TRUSTED_PROXY:
            return namesARange(access.path)
              ? t`${path} as a proxy it believes on where a request came from: whatever else is in that range can reach the route, and say who its client is`
              : t`${path} as a proxy it believes on where a request came from`;
          case HostAccessKind.PLAIN_FOR_ANYONE:
            return t`plain HTTP on ${path} for anyone who can reach this server, around whatever is in front of it`;
          case HostAccessKind.HTTPS_OFF:
            return t`HTTPS on ${path}, which stops: https:// addresses get an error, and browsers that saw HSTS from it refuse plain HTTP until that runs out`;
        }
        return t`something else on the server`;
      },
      reload(server) {
        return t`This reloads the proxy on ${server}, which can reset a connection on its way in to any site there, and lose a POST on a kept HTTP/1.1 connection. A browser sends such a request again, and an app's own client may not`;
      },
      ingress(effect, server) {
        if (!effect) return [];
        const { proxy, panelDomain } = effect;
        const ports = effect.ports.join(", ");
        const out: string[] = [];
        switch (effect.change) {
          case IngressChange.STARTS:
            out.push(t`${proxy} isn't running on ${server}, so this route starts it, and it takes ports ${ports}`);
            if (panelDomain !== "") {
              out.push(
                t`The panel's domain, ${panelDomain}, moves behind ${proxy}. Its address, passkeys and sessions stay as they are, and it keeps its present certificate until ${proxy} has its own. HTTPS on it is down for a moment as the ports change hands; the SSH tunnel keeps working`,
              );
            }
            break;
          case IngressChange.STOPS:
            out.push(
              t`This is the last route on ${server}, so ${proxy} stops, and ports ${ports} are free. Its certificates stay on the server for when a route comes back`,
            );
            if (panelDomain !== "") {
              out.push(
                t`The panel takes those ports back for its domain, ${panelDomain}. Its address, passkeys and sessions stay as they are`,
              );
            }
            break;
          case IngressChange.RESTARTS:
            out.push(
              t`${proxy} restarts on ${server}, for a newer version, so everything it serves is down for that long`,
            );
            break;
          case IngressChange.TAKES_PORTS:
            out.push(
              t`This is the first route on ${server} that takes a certificate, so ${proxy} takes ports ${ports} too, and nothing it serves stops`,
            );
            break;
          case IngressChange.FREES_PORTS:
            out.push(
              t`No route on ${server} takes a certificate after this, so ${proxy} frees ports ${ports}. Its routes that take none go on answering`,
            );
            break;
        }
        return out;
      },
      returning(names, server) {
        if (names.length === 0) return "";
        const list = names.join(", ");
        const command = names.join(" ");
        return t`Docker starts ${list} again when ${server} next restarts, which may take port 80 or 443 from the proxy, and stop this route answering. Keep it stopped with docker update --restart=no ${command}`;
      },
      domain(check) {
        if (!check) return "";
        const { domain } = check;
        const found = listWords(i18n.locale, check.found);
        const here = listWords(i18n.locale, check.here);
        // A record pointed at a private address can't work, so none is named as where to
        // point one, whatever an agent said. What's known goes first for its family, as
        // resource.PointHere has it, and a way out is only named with what it may not be.
        const told = check.publicAddresses.filter((ip) => !isPrivateAddress(ip));
        const family = (ip: string) => ip.includes(":");
        const own = check.here.filter(
          (ip) => !isPrivateAddress(ip) && !told.includes(ip) && !told.some((known) => family(known) === family(ip)),
        );
        const addresses = [...told, ...own];
        const proved = addresses.filter((ip) => !check.unprovenAddresses.includes(ip));
        const unproven = addresses.filter((ip) => check.unprovenAddresses.includes(ip));
        const kinds = (list: string[]) =>
          listWords(
            i18n.locale,
            [...new Set(list.map((ip) => (family(ip) ? "AAAA" : "A")))].sort((x, y) => x.length - y.length),
            "disjunction",
          );
        const publics = listWords(i18n.locale, proved);
        const either = listWords(i18n.locale, proved, "disjunction");
        const record = kinds(proved);
        const goesOut = listWords(i18n.locale, unproven, "disjunction");
        const goesOutRecord = kinds(unproven);
        const provedWords = t`Point an ${record} record at this server, ${either}`;
        const unprovenWords = t`Point an ${goesOutRecord} record at ${goesOut}, which is how this server reaches the internet. If it's behind a router, that record won't reach it without a forwarded port, and a tunnel needs neither`;
        const point =
          proved.length > 0 && unproven.length > 0
            ? `${provedWords}. ${unprovenWords}`
            : unproven.length > 0
              ? unprovenWords
              : check.carrierNat
                ? t`This server is behind carrier NAT (${here}), so a DNS record can't reach it, and neither can a forwarded port. Use a tunnel, which needs neither`
                : here === ""
                  ? t`Point an A or AAAA record at this server's public address`
                  : check.here.length === 1
                    ? t`Point an A record at this server's public address. Dockwell only sees ${here} here, which is private, so the internet can't reach it`
                    : t`Point an A record at this server's public address. Dockwell only sees ${here} here, which are private, so the internet can't reach it`;
        const plainly = proved.length > 0 && unproven.length === 0;
        if (check.proxied) {
          return t`DNS for ${domain} points at Cloudflare's proxy, which can block the certificate authority's check, so its certificate may not come. Set the record to DNS only. If a tunnel or a proxy in front ends TLS, set tls to off on this route instead`;
        }
        switch (check.state) {
          case DomainCheckState.NO_RECORD:
            return plainly
              ? t`${domain} has no DNS record yet, so its certificate can't come. Point an ${record} record at ${either}; it comes once you do`
              : t`${domain} has no DNS record yet, so its certificate can't come until it has one. ${point}`;
          case DomainCheckState.ELSEWHERE:
            return plainly
              ? t`DNS for ${domain} points at ${found}, not this server (${publics}), so its certificate can't come yet. Point it here; it comes once you do. If a tunnel or a proxy in front ends TLS, set tls to off on this route instead`
              : t`DNS for ${domain} points at ${found}, not this server, so its certificate can't come yet. ${point}. If a tunnel or a proxy in front ends TLS, set tls to off on this route instead`;
          case DomainCheckState.CANT_TELL:
            if (unproven.length > 0) {
              return t`DNS for ${domain} points at ${found}. This server goes out from ${goesOut}, which may not be its way in, so the agent can't tell whether that is this server. If it isn't, its certificate can't come until it is. If a tunnel or a proxy in front ends TLS, set tls to off on this route instead`;
            }
            return t`DNS for ${domain} points at ${found}. This server's own addresses are private (${here}), so the agent can't tell whether that is this server. If it isn't, its certificate can't come until it is. If a tunnel or a proxy in front ends TLS, set tls to off on this route instead`;
        }
        return "";
      },
      unencrypted(server) {
        return t`Traffic for this route reaches ${server} unencrypted, as TLS ends before it. That's fine on a tunnel or a private network, and not on the open internet, where anyone on the way can read it`;
      },
      goes({ where, domain, find }) {
        switch (find) {
          case VisitorsFind.REFUSED:
            return t`Anyone visiting ${where} gets a refused connection, as nothing answers on this server's port any more.`;
          case VisitorsFind.TLS_ERROR:
            return t`Anyone visiting ${where} gets a browser's connection error, as nothing here answers for that name over HTTPS any more.`;
          case VisitorsFind.NOT_FOUND:
            return t`What's in front gets a 404 for ${where}, so visitors see whatever it shows for one.`;
          case VisitorsFind.NOTHING_BEHIND:
            return t`What's in front can't connect for ${where} any more, so visitors see whatever it shows for that.`;
          case VisitorsFind.REST:
            return t`Anyone visiting ${where} gets what the rest of ${domain} serves, or a 404 if nothing covers that path.`;
          default:
            return t`Anyone visiting ${where} stops reaching it.`;
        }
      },
      moves({ where, from, to, toPort }) {
        return to
          ? t`${where} leaves ${from}, and goes to ${to}.`
          : t`${where} leaves ${from}, and goes to port ${toPort} on the server.`;
      },
      answers(answers, port, server) {
        if (!answers) return [];
        const out: string[] = [];
        const list = answers.proxies.join(", ");
        let who = t`anyone on the internet`;
        switch (answers.reach) {
          case RouteReach.THIS_SERVER:
            who = t`what runs on ${server} itself`;
            out.push(
              t`It answers only what runs on ${server} itself, such as a tunnel on its own network. Anyone else gets what a name with no route gets`,
            );
            break;
          case RouteReach.PROXIES:
            who = t`whoever comes through ${list}`;
            out.push(t`It answers only ${list}. Anyone else gets what a name with no route gets`);
            break;
          case RouteReach.ANYONE:
            if (answers.plain) {
              who = t`anyone who can reach ${server}`;
              out.push(
                t`It answers anyone who can reach ${server} on port 80, over plain HTTP, around whatever is in front of it`,
              );
            }
            break;
        }
        if (!port) return out;
        const number = String(port.port);
        out.push(
          t`That lets ${who} reach port ${number}, and what's there sees each caller as this server, 127.0.0.1, which an app that trusts localhost will believe`,
        );
        const listening = port.listening;
        if (listening !== "") out.push(t`Listening there now: ${listening}`);
        else if (port.nothingListening)
          out.push(t`Nothing listens on port ${number} yet, so this answers 502 until something does`);
        return out;
      },
      rollout(plan, span, restart = false, stopped = false) {
        if (!plan) return [];
        const out: RolloutLine[] = [];
        const say = (text: string, how: Partial<RolloutLine> = {}) => out.push({ text, warn: false, ...how });
        const ports = plan.ports.join(", ");
        const volumes = plan.volumes.join(", ");
        const paths = plan.paths.join(", ");
        const imageData = plan.imageData.join(", ");
        const cantTell = plan.cantTell.join(", ");
        const edited = plan.editedRoutes.join(", ");
        const networks = plan.earlyNetworks.join(", ");
        const names = plan.earlyNames.join(", ");
        const healthyWithin = span(millis(plan.healthyWithin));
        const soak = span(millis(plan.soak));
        const drain = span(millis(plan.drain));
        const shared = [...plan.volumes, ...plan.paths, ...plan.imageData, ...plan.cantTell].join(", ");
        if (plan.rolling) {
          if (plan.noCheck && millis(plan.healthyWithin) > 0) {
            // What nothing checks is healthy as it runs: how long it must is all there is to say.
            const kept = millis(plan.soak) > 0;
            if (stopped) {
              say(
                restart
                  ? kept
                    ? t`Nothing checks it, so it has to keep running for ${soak}. If it doesn't, the new one goes, and what was stopped stays stopped`
                    : t`Nothing checks it, so it only has to start. If it doesn't, the new one goes, and what was stopped stays stopped`
                  : kept
                    ? t`Nothing checks it, so it has to keep running for ${soak}. If it doesn't, Dockwell rolls this back`
                    : t`Nothing checks it, so it only has to start. If it doesn't, Dockwell rolls this back`,
              );
            } else {
              say(
                restart
                  ? kept
                    ? t`Nothing checks it, so a new container takes over once it has kept running for ${soak}, and what runs goes on serving until then. If it doesn't, the new one goes, and what runs stays`
                    : t`Nothing checks it, so a new container takes over once it starts, and what runs goes on serving until then. If it doesn't, the new one goes, and what runs stays`
                  : kept
                    ? t`Nothing checks it, so a new container takes over once it has kept running for ${soak}, and what runs goes on serving until then. If it doesn't, Dockwell rolls this back`
                    : t`Nothing checks it, so a new container takes over once it starts, and what runs goes on serving until then. If it doesn't, Dockwell rolls this back`,
              );
            }
          } else if (stopped) {
            // Nothing runs, so nothing goes on serving, and nothing is in flight.
            say(
              restart
                ? t`It has to stay healthy for ${soak}. If it isn't healthy within ${healthyWithin}, the new one goes, and what was stopped stays stopped`
                : t`It has to stay healthy for ${soak}. If it isn't healthy within ${healthyWithin}, Dockwell rolls this back`,
            );
          } else if (millis(plan.healthyWithin) > 0) {
            if (restart) {
              say(
                t`A new container takes over once it has been healthy for ${soak}, and what runs goes on serving until then. If it isn't healthy within ${healthyWithin}, the new one goes, and what runs stays`,
              );
            } else {
              say(
                t`A new container takes over once it has been healthy for ${soak}, and what runs goes on serving until then. If it isn't healthy within ${healthyWithin}, Dockwell rolls this back`,
              );
            }
          } else {
            say(t`What this step wants already runs, so nothing stops for it`);
          }
          if (plan.others.length > 0 && !stopped) {
            say(
              t`What is in flight gets up to ${drain} to finish. What outlasts that, such as a long connection, ends with the old container`,
            );
          }
          if (networks !== "" && !stopped) {
            say(
              names === ""
                ? t`On ${networks}, other services can reach the new container by its service's name before it has passed its check, beside the old one, as Docker can't add a name to a container later`
                : t`On ${networks}, other services can reach the new container by its service's name and by ${names} before it has passed its check, beside the old one, as Docker can't add a name to a container later`,
              { notice: true },
            );
          }
        } else if (!stopped) {
          // Nothing runs, so nothing stops first.
          if (ports !== "") say(t`Two containers can't publish ${ports} at once, so the old one stops first`);
          if (plan.hostNetwork) {
            say(
              t`A container on the server's own network listens on the server's ports itself, so two can't run at once, and the old one stops first`,
            );
          }
          if (volumes !== "") {
            say(
              t`Two containers mustn't write to the volume ${volumes} at once, so the old one stops first. If the app can, set its rollout order to start first`,
            );
          }
          if (paths !== "") {
            say(
              t`Two containers mustn't write to the server path ${paths} at once, so the old one stops first. If the app can, set its rollout order to start first`,
            );
          }
          if (imageData !== "") {
            say(
              t`Its image keeps data at ${imageData}, which two containers mustn't write to at once, so the old one stops first. If the app can, set its rollout order to start first`,
            );
          }
          if (cantTell !== "") {
            say(
              t`Dockwell can't read where ${cantTell} really lands, so it can't say two containers wouldn't write to one place, and the old one stops first. If the app can, set its rollout order to start first`,
            );
          }
          if (edited !== "") {
            say(
              t`The route ${edited} was edited by hand, and Dockwell leaves it as it is, so the new container has to keep the old one's address, and the old one stops first. Revert or adopt the edit to roll`,
            );
          }
          if (ports === "" && shared === "" && edited === "" && !plan.hostNetwork && plan.chosen)
            say(t`Its rollout order says to stop first, so there are never two`);
          if (restart) {
            say(t`If the new one doesn't come up, nothing of this runs, and a restart has nothing to go back to`, {
              warn: true,
              lead: t`The old container stops first`,
            });
          }
        }
        for (const m of plan.dropped) {
          const source = m.source;
          const target = m.target;
          say(
            m.emptyVolume
              ? t`Its image puts an empty volume there. ${source} stays on the server, as it was`
              : t`${source} stays on the server, as it was`,
            { warn: true, lead: t`${source} stops being what the service sees at ${target}` },
          );
        }
        // Only what a rollout left, or someone made by hand: one old container is what any replace removes.
        if (plan.others.length > 1) {
          const extra = String(plan.others.length - 1);
          say(
            t`It also removes ${extra} more containers of this service, which a rollout that stopped partway or docker compose up --scale left`,
          );
        }
        return out;
      },
      actorKind(kind) {
        switch (kind) {
          case ActorKind.TOKEN:
            return t`API token`;
          case ActorKind.DOCKWELL:
            return t`Automatic`;
          case ActorKind.SERVER:
            return t`Server`;
          case ActorKind.SHELL:
            return t`In a shell on the control server`;
        }
        return "";
      },
    }),
    [t, i18n.locale],
  );
}

/** What comes along with a plan's apps, as one row says it: "Also creates project blog, a network and 2 volumes". */
export function useAlsoWords() {
  const { t, i18n } = useLingui();
  return useCallback(
    (fold: Fold, done: boolean): string => {
      const { project, environment, networks, volumes } = fold;
      const parts: string[] = [];
      if (project) parts.push(t`project ${project}`);
      if (environment) parts.push(t`environment ${environment}`);
      if (networks > 0) parts.push(plural(networks, { one: "a network", other: "# networks" }));
      if (volumes > 0) parts.push(plural(volumes, { one: "a volume", other: "# volumes" }));
      const list = listWords(i18n.locale, parts);
      return done ? t`Also created ${list}` : t`Also creates ${list}`;
    },
    [t, i18n.locale],
  );
}
