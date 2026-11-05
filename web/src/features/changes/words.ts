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
  PlanAction,
  type RouteAnswers,
  RouteReach,
  type ServerPortCheck,
  StepEffect,
  StepState,
  Surface,
} from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useMemo } from "react";

/** How a step is going, as a status and a word. */
export interface StepLook {
  status: Status;
  word: string;
}

/** The words the plan sheet and Activity share, in the active locale. */
export interface ChangeWords {
  /** A resource's kind, as a noun: "Project". */
  kind(kind: string): string;
  /** What a step does: "Create", "Replace". */
  action(action: PlanAction): string;
  /** What a step does to what the resource serves: "Restarts". Empty for nothing. */
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
  /** What would take the proxy's ports back at the next reboot, and how to keep it from happening. It never stops a plan. */
  returning(names: string[], server: string): string;
  /** What DNS said of a route's domain as this was planned, if that is worth saying. It never stops a plan. */
  domain(check: DomainCheck | undefined): string;
  /** What a route that takes no certificate means, as TLS ends before its server. It never stops a plan. */
  unencrypted(server: string): string;
  /** Who a route answers, when it isn't anyone over HTTPS sent to a service, a sentence each. */
  answers(answers: RouteAnswers | undefined, port: ServerPortCheck | undefined, server: string): string[];
}

/** Quotes what could read as the plan's own words, as AccessWords does on the server. */
function plain(s: string): string {
  return /^[A-Za-z0-9_.,/=:@+*-]*$/.test(s) ? s : JSON.stringify(s);
}

export function useChangeWords(): ChangeWords {
  const { t } = useLingui();
  return useMemo<ChangeWords>(
    () => ({
      kind(kind) {
        switch (kind) {
          case "project":
            return t`Project`;
          case "environment":
            return t`Environment`;
          case "service":
            return t`Service`;
          case "container":
            return t`Container`;
          case "volume":
            return t`Volume`;
          case "network":
            return t`Network`;
          case "route":
            return t`Route`;
          case "firewall.rule":
            return t`Firewall rule`;
          case "variable":
            return t`Variable`;
          case "secret":
            return t`Secret`;
        }
        return kind;
      },
      action(action) {
        switch (action) {
          case PlanAction.CREATE:
            return t`Create`;
          case PlanAction.UPDATE:
            return t`Update in place`;
          case PlanAction.REPLACE:
            return t`Replace`;
          case PlanAction.DELETE:
            return t`Destroy`;
        }
        return t`Nothing to do`;
      },
      effect(effect) {
        switch (effect) {
          case StepEffect.RESTART:
            return t`Restarts`;
          case StepEffect.RECREATE:
            return t`Recreated`;
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
        const [path, type, driver, detail] = [access.path, access.type, access.driver, access.detail].map(plain);
        const reads = access.readOnly ? t`read-only` : t`read-write`;
        switch (access.kind) {
          case HostAccessKind.SERVER_PATH: {
            let how = reads;
            if (access.relabel === "z") how = t`${reads}, relabelled for SELinux as shared`;
            if (access.relabel === "Z") how = t`${reads}, relabelled for SELinux as private`;
            return t`the server path ${path} (${how})`;
          }
          case HostAccessKind.DOCKER_SOCKET:
            return t`Docker's socket at ${path}, which gives control of Docker on the server, the same as root`;
          case HostAccessKind.HOST_NETWORK:
            return t`the server's own network`;
          case HostAccessKind.VOLUME_MOUNT: {
            if (driver !== "") return t`the volume driver ${driver}, which reaches what the plugin does (${reads})`;
            const at = plain(access.address + access.path);
            return at === "" ? t`a ${type} mount (${reads})` : t`the ${type} mount ${at} (${reads})`;
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
            return detail === ""
              ? t`the network ${path}, which is on the server already, where it answers to its name: whatever else is on that network, other projects' services too, can reach it, and it can reach them`
              : t`the network ${path}, which is on the server already, where it answers to its name and to ${detail}: whatever else is on that network, other projects' services too, can reach it, and it can reach them`;
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
            return t`${path} as a proxy it believes on where a request came from`;
          case HostAccessKind.PLAIN_FOR_ANYONE:
            return t`plain HTTP on ${path} for anyone who can reach this server, around whatever is in front of it`;
          case HostAccessKind.HTTPS_OFF:
            return t`HTTPS on ${path}, which stops: https:// addresses get an error, and browsers that saw HSTS from it refuse plain HTTP until that runs out`;
        }
        return t`something else on the server`;
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
        const found = check.found.join(", ");
        const here = check.here.join(", ");
        switch (check.state) {
          case DomainCheckState.NO_RECORD:
            return here === ""
              ? t`${domain} has no DNS record yet, so its certificate can't come. Point an A or AAAA record at this server; it comes once you do`
              : t`${domain} has no DNS record yet, so its certificate can't come. Point an A or AAAA record at ${here}; it comes once you do`;
          case DomainCheckState.ELSEWHERE:
            return t`DNS for ${domain} points at ${found}, not this server (${here}), so its certificate can't come yet. Point it here; it comes once you do. If a tunnel or a proxy in front ends TLS, set tls to off on this route instead`;
          case DomainCheckState.CANT_TELL:
            return t`DNS for ${domain} points at ${found}. This server's own addresses are private (${here}), so the agent can't tell whether that is this server. If it isn't, its certificate can't come until it is. If a tunnel or a proxy in front ends TLS, set tls to off on this route instead`;
        }
        return "";
      },
      unencrypted(server) {
        return t`Traffic for this route reaches ${server} unencrypted, as TLS ends before it. That's fine on a tunnel or a private network, and not on the open internet, where anyone on the way can read it`;
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
    [t],
  );
}
