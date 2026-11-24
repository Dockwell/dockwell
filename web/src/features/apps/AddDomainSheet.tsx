// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  CatalogService,
  type Change,
  ChangeService,
  ChangeState,
  CloudflareName,
  type Domain,
  type DomainRoute,
  DomainService,
  PortAnswer,
  RouteWay,
  type Server,
  type ServiceInfo,
} from "@dockwell/api";
import {
  Advanced,
  Button,
  Callout,
  cx,
  Icon,
  PHONE_QUERY,
  SelectField,
  Sheet,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import { type FormEvent, useEffect, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { dnsProviderEdit, useDnsProviders, useProviderAccessPlan } from "../dns/dns";
import { type FleetState, useFleet } from "../servers/useFleet";
import { CompanionChoice, useCompanion, useCompanionChoice } from "../websites/Companion";
import { useWebsiteOfEnvironment } from "../websites/website";
import { appOf, environmentOf, nameOf } from "./app";
import { appPortsOf, knownFor, usePortLabel, useServicePorts } from "./appPorts";
import { useRouteTarget, useTunnelSwitchName } from "./DomainNote";
import { DomainRecord } from "./DomainRecord";
import { DomainName, NameState, ProviderReach, StateLine, WayIn } from "./DomainStep";
import {
  formRefusal,
  freshRouteName,
  proxyList,
  redirectTarget,
  refusedField,
  refusesTarget,
  routeName,
} from "./domains";
import { FrontFields, frontOf, type InFront } from "./FrontFields";
import { validPort } from "./form";
import { PortState } from "./PortState";
import { routeAsItIs } from "./PrivateSheet";
import {
  nameIn,
  pickedOf,
  publicAddressOf,
  publishOf,
  routeOfWay,
  subdomainOf,
  type TakeOverInit,
  useCloudflareCheck,
  useInstallAdmin,
  useZones,
  wayOf,
  zoneOfName,
} from "./publish";
import { usePlaces } from "./useServices";

/** A domain that's refused a certificate, moved behind what's in front by editing the routes it has. */
export interface MoveBehind {
  domain: string;
  port: string;
  /** The route this form's port is for. */
  route: string;
  /** All of the domain's routes, as a domain is served one way or the other. */
  routes: readonly DomainRoute[];
}

/** What the form asks to move a domain behind a tunnel or a proxy, from the route someone asked from. */
export function moveBehindOf(domain: Domain, route: DomainRoute): MoveBehind {
  return {
    domain: domain.name,
    port: String(route.hostPort || route.port),
    route: route.name,
    routes: domain.routes,
  };
}

/** Whether a redirect sends to a domain, as one a website's list wrote does: one a person pointed elsewhere is theirs. */
function sendsTo(to: string | undefined, domain: string): boolean {
  if (!to) return false;
  try {
    return new URL(to).hostname === domain;
  } catch {
    return false;
  }
}

/** An app the form can send a domain to, and what to call it where there's a choice. */
export interface AppChoice {
  service: ServiceInfo;
  label: string;
}

/** The port a domain likely goes to on an app, to start from. */
function portOf(service: ServiceInfo | undefined): string {
  const app = service ? appOf(service) : undefined;
  // On the server's own network, there's no port it's likely to be: 80 is the proxy's.
  if (app?.hostNetwork) return "";
  // The control plane says which port serves its pages (#791): the one a route
  // sends to, its health check's, or a published one that isn't SSH's or a
  // database's. Never just the first, which is 22 for an app that lists SSH first.
  if (service?.webPort) return String(service.webPort);
  const published = app?.ports[0]?.containerPort;
  return published ? String(published) : "80";
}

/** The server a route goes on: the one chosen, or the only one there is. */
function serverOf(fleet: FleetState, id: string): Server | undefined {
  if (fleet.status === "connecting") return undefined;
  return fleet.servers.find((s) => s.id === id) ?? (fleet.servers.length === 1 ? fleet.servers[0] : undefined);
}

/**
 * Sends a domain to this app: a domain and the port it listens on, then the plan,
 * which says what a server's first route does to its ports 80 and 443. How the
 * domain gets its certificate is the server's to work out, unless a tunnel or a
 * proxy in front handles HTTPS, when there's none to get, and you say what's in front,
 * as that's who the route answers and who the app trusts to identify visitors. An app
 * on the server's own network can't be dialled by name, so the route names it and dials
 * its port on the server. The plan says what each of those means. Opened from the app
 * it's for, or from a list of domains, which isn't about any one app: then the form
 * asks which, once there's more than one, and what it starts from follows that choice.
 */
export function AddDomainSheet({
  service: given,
  apps,
  onClose,
  start,
  named,
}: {
  /** The app it's for, when it's opened from one. */
  service?: ServiceInfo;
  /** The apps to choose from, when it isn't. */
  apps?: readonly AppChoice[];
  onClose: () => void;
  /** A domain that needs moving behind a tunnel, as it's refused a certificate. */
  start?: MoveBehind;
  /** A name to start from, as Domains opens it on one made in Cloudflare to take on. */
  named?: string;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const places = usePlaces();
  // From Domains, a domain that belongs to no app, such as a vanity one, can redirect
  // to a URL instead (ADR 0070).
  const [redirecting, setRedirecting] = useState(false);
  const [to, setTo] = useState("");
  const [toError, setToError] = useState<string>();
  const [temporary, setTemporary] = useState(false);
  const [dropPath, setDropPath] = useState(false);
  const [placeChosen, setPlaceChosen] = useState("");
  const [chosen, setChosen] = useState("");
  const choosing = given === undefined && (apps?.length ?? 0) > 1 && !redirecting;
  const offersRedirect = given === undefined && start === undefined;
  const service = redirecting
    ? undefined
    : (given ?? (choosing ? apps?.find((a) => a.service.id === chosen) : apps?.[0])?.service);
  const name = service ? nameOf(service) : "";
  const environment = redirecting
    ? placeChosen || places.defaultEnvironment?.id || ""
    : service
      ? environmentOf(service)
      : "";
  const known = useQuery(DomainService.method.listDomains, { environment }, { enabled: environment !== "" });
  // A name is picked from a zone a connected DNS provider writes in for the project, and
  // Cloudflare is done for you; Another domain is typed whole and behaves as before (ADR 0104).
  const project = places.place(environment)?.scope ?? "";
  const { zones, providers } = useZones(start ? "" : project);
  const allProviders = useDnsProviders(!start && project !== "").data?.providers ?? [];
  const connectedZones = allProviders.flatMap((p) => p.zones.map((zone) => ({ zone, provider: p.name })));
  const [domain, setDomain] = useState(start?.domain ?? named ?? "");
  const [zonePicked, setZonePicked] = useState<string>();
  const matchedZone = zoneOfName(zones, nameIn(domain, ""));
  const zone =
    start || zones.length === 0
      ? ""
      : zonePicked !== undefined && (zonePicked === "" || zones.some((z) => z.zone === zonePicked))
        ? zonePicked
        : matchedZone || (domain.includes(".") ? "" : (zones[0]?.zone ?? ""));
  // A website's other domains are reached as its own is (ADR 0070): moving the site's
  // domain behind what's in front, or out, moves the redirects its list wrote with it.
  const websiteOf = useWebsiteOfEnvironment(start !== undefined);
  const site = start && environment ? websiteOf?.(environment) : undefined;
  const followers =
    site && site.domain === start?.domain
      ? (known.data?.domains ?? []).flatMap((d) =>
          site.spec?.redirectFrom.includes(d.name)
            ? d.routes
                .filter((r) => r.path === "" && sendsTo(r.redirect?.to, site.domain))
                .map((route) => ({ domain: d.name, route }))
            : [],
        )
      : [];
  const onServer = (service && appOf(service)?.hostNetwork) ?? false;
  const fleetServers = useFleet();
  const servers = redirecting
    ? fleetServers.status === "connecting"
      ? []
      : fleetServers.servers.map((s) => ({ serverId: s.id, serverName: s.name }))
    : (service?.deployments ?? []);
  const [serverPicked, setServer] = useState(servers[0]?.serverId ?? "");
  // What the Server field shows is what the plan sends: the one picked while it's a
  // choice there, or else the first, as the field shows it.
  const server = servers.some((s) => s.serverId === serverPicked) ? serverPicked : (servers[0]?.serverId ?? "");
  const [port, setPort] = useState(start?.port ?? portOf(service));
  // What you typed stays as you choose another app. What it started from follows.
  const portTyped = useRef(false);
  const [appError, setAppError] = useState<string>();
  const [noCertificate, setNoCertificate] = useState(start !== undefined);
  // Whether the new domain is watched by an uptime check, as Dockwell does unless told not to (#835).
  const [watchIt, setWatchIt] = useState(true);
  const [inFront, setInFront] = useState<InFront>("tunnel");
  const [proxies, setProxies] = useState("");
  const [proxiesError, setProxiesError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [domainError, setDomainError] = useState<string>();
  const [portError, setPortError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [plan, setPlan] = useState<Opening>();
  const field = useRef<HTMLInputElement>(null);
  // Review plan pressed while the check reads Cloudflare goes on once it has.
  const [waiting, setWaiting] = useState(false);
  const form = useRef<HTMLFormElement>(null);
  const switchName = useTunnelSwitchName();
  const fleet = fleetServers;
  const navigate = useNavigate();
  // Once it's on its way, the app's page is where the domain's state shows as it's served.
  const landOnApp = () => {
    if (service) void navigate({ to: "/apps/$serviceId", params: { serviceId: service.id } });
  };
  // The ports the app listens on, its own, to pick from, each labelled; any other is
  // typed. The port that serves its pages comes first, whether or not it's published.
  // What the app's server finds on each, asked as the form opens (#791).
  const servicePorts = useServicePorts(onServer || redirecting ? undefined : service);
  const appPorts = onServer ? [] : appPortsOf(service, servicePorts.checks);
  const listens = appPorts.map((p) => p.port);
  const portLabel = usePortLabel();
  // A port known for something else, such as SSH's 22, reaches no web page (#791).
  const portElsewhere = !onServer && validPort(port) ? knownFor(Number(port)) : "";
  // The port that serves its pages: the control plane's, unless that's one known for
  // something else, then the first its server found answering HTTP.
  const webPort =
    appPorts.find((p) => p.web && !knownFor(p.port))?.port ??
    appPorts.find((p) => p.check?.answer === PortAnswer.HTTP && !knownFor(p.port))?.port;
  // With no web port known, the first that answers HTTP is where it starts, until someone picks one.
  useEffect(() => {
    if (!portTyped.current && start === undefined && !service?.webPort && webPort !== undefined) {
      setPort(String(webPort));
    }
  }, [webPort, service?.webPort, start]);
  const [typingPort, setTypingPort] = useState(false);
  const [portOpen, setPortOpen] = useState(false);
  const picksPort = listens.length > 0 && !typingPort && (port === "" || listens.includes(Number(port)));
  // A domain's root may be with another app already, whatever its route is called.
  // Going on moves it, the same either way, so this says that before it does.
  const typed = nameIn(domain, zone);
  // What the zone and the server's tunnels say of the name, and the way in it picks,
  // unless the person picked another that can still be taken.
  const connected = !start && (zone !== "" || zoneOfName(connectedZones, typed) !== "");
  const cf = useCloudflareCheck(server, typed, project, !start && project !== "" && connected);
  const outsideProject = cf.check?.state === CloudflareName.OUTSIDE_PROJECT;
  const publishing = connected && !outsideProject;
  const manual = !connected;
  const [wayPicked, setWayPicked] = useState<{ way: RouteWay; tunnel: string }>();
  const way =
    wayPicked && wayOf(cf.check, wayPicked.way, wayPicked.tunnel)?.off === "" ? wayPicked : pickedOf(cf.check);
  const wayTakeable = wayOf(cf.check, way.way, way.tunnel)?.off === "";
  const [waysOpen, setWaysOpen] = useState(false);
  // The choice to leave a name's other half out, one step away in a connected zone.
  const [companionOpen, setCompanionOpen] = useState(false);
  // What an admin agreed to replace or take on, for the name it was agreed for alone.
  const [consent, setConsent] = useState<{ domain: string; items: TakeOverInit[] }>({ domain: "", items: [] });
  const takesOver = consent.domain === typed ? consent.items : [];
  const provider = cf.check?.provider || zones.find((z) => z.zone === zone)?.provider || "";
  const admin = useInstallAdmin();
  const providerInfo = (outsideProject ? allProviders : providers).find(
    (p) => p.name === provider && p.provider !== "",
  );
  // Published at Cloudflare: the way's route, and what it publishes. Anything else is
  // as it was, with what's in front said under Advanced.
  const publishes = publishing && cf.check !== undefined && wayTakeable && way.way !== RouteWay.FRONT;
  const behind = publishing ? way.way === RouteWay.FRONT : noCertificate;
  const serverName = serverOf(fleetServers, server)?.name ?? "";
  const held = known.data?.domains.find((d) => d.name === typed)?.routes.find((route) => route.path === "");
  const leaves = !start && !redirecting && held && held.service !== name ? held : undefined;
  // www for a bare domain, or the bare domain for www, redirecting to it, on by what DNS
  // says (ADR 0070). Not for a domain that redirects, or one moving behind what's in front.
  const companion = useCompanion(
    start || redirecting ? "" : (serverOf(fleet, server)?.id ?? ""),
    typed,
    places.place(environment)?.scope ?? "",
  );
  const [chosenCompanion, setAlsoCompanion] = useCompanionChoice(companion, environment, true, publishing);
  // www that redirects to this domain already needs nothing added.
  const companionAlready = (known.data?.domains ?? []).some(
    (d) => d.name === companion.companion && d.routes.some((r) => r.path === "" && sendsTo(r.redirect?.to, typed)),
  );
  const alsoCompanion = chosenCompanion && !companionAlready;
  const target = useRouteTarget();
  const leavesApp = leaves ? target(leaves) : "";
  // A catalog app builds its links from variables that follow its address (Entry.address_variables):
  // the control plane sets them in the plan that routes its first domain (ADR 0103), which the
  // summary says.
  const installs = useQuery(
    CatalogService.method.listCatalogInstalls,
    {},
    { enabled: service !== undefined && !start },
  );
  const entryId = (service && installs.data?.installs.find((i) => i.webServiceId === service.id)?.pin?.entry) || "";
  const entry = useQuery(CatalogService.method.getCatalogEntry, { id: entryId }, { enabled: entryId !== "" }).data
    ?.entry;
  const firstDomain =
    known.data !== undefined && !known.data.domains.some((d) => d.routes.some((r) => r.service === name));
  const follows = !start && firstDomain && entry && entry.addressVariables.length > 0 ? entry : undefined;
  const followsApp = follows?.name ?? "";
  const followsNames = follows?.addressVariables.map((a) => a.variable).join(", ") ?? "";

  /** Takes you to the first field that was refused, ready to change, as New app does. */
  const revealError = () =>
    requestAnimationFrame(() => {
      const refused = form.current?.querySelector<HTMLElement>('[aria-invalid="true"]');
      refused?.focus();
      if (refused instanceof HTMLInputElement) refused.select();
    });

  const choose = (id: string) => {
    const next = apps?.find((a) => a.service.id === id)?.service;
    setChosen(id);
    setAppError(undefined);
    setServer(next?.deployments[0]?.serverId ?? "");
    if (!portTyped.current) setPort(portOf(next));
  };

  // biome-ignore lint/correctness/useExhaustiveDependencies: it goes on once the check is in, and submit is new each time.
  useEffect(() => {
    if (!waiting || cf.checking) return;
    setWaiting(false);
    void submit();
  }, [waiting, cf.checking]);

  const accessPlan = useProviderAccessPlan();
  const useHere = () => {
    if (!providerInfo || !project) return;
    setPlan(
      accessPlan(providerInfo, project, (change) => {
        setPlan(undefined);
        if (change?.state !== ChangeState.APPLIED) return;
        setZonePicked(undefined);
        cf.again();
      }),
    );
  };

  /** Sets the zone to Full (strict) on its DNS provider's own plan, for an admin (ADR 0104, section 2). */
  const toStrict = () => {
    if (!providerInfo) return;
    const p = providerInfo;
    const checkedZone = cf.check?.zone || zone;
    setPlan({
      kind: "propose",
      summary: t`Set ${checkedZone} to Full (strict)`,
      edits: [dnsProviderEdit(p, { strictZones: [...new Set([...p.strictZones, checkedZone])] })],
      retryLabel: t`Edit and try again`,
      // Back here either way, with the check read again.
      onClose: () => {
        setPlan(undefined);
        cf.again();
      },
    });
  };

  // Back from a plan that failed, what it said is at the field, ready to change.
  const reveal = useRef(false);
  // biome-ignore lint/correctness/useExhaustiveDependencies: it runs when the form comes back, and revealError is new each time.
  useEffect(() => {
    if (plan || !reveal.current) return;
    reveal.current = false;
    revealError();
  }, [plan]);

  /** Puts what the server refused at the field it's about, or above them all if it's about none of them. */
  const land = (raw: string, wanted: string) => {
    const said = formRefusal(raw, wanted);
    if (redirecting && refusesTarget(said, wanted)) {
      setToError(said);
      return;
    }
    switch (refusedField(said, { domain: wanted, port, onServer })) {
      case "proxies":
        if (behind && inFront === "proxy") setProxiesError(said);
        else setFailure(said);
        break;
      case "port":
        setPortError(said);
        break;
      case "domain":
        setDomainError(said);
        break;
      default:
        setFailure(said);
    }
  };

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy) return;
    // A name in a connected zone waits for the check, which picks the way in.
    if (cf.checking && typed !== "") {
      setWaiting(true);
      return;
    }
    const wanted = nameIn(domain, zone);
    // What's sent is what the field shows: https:// and a domain's xn-- form are added there.
    const goesTo = redirecting ? redirectTarget(to) : to.trim();
    if (redirecting && goesTo !== to) setTo(goesTo);
    const domainOk = wanted !== "" && !/\s/.test(wanted);
    const portOk = redirecting || validPort(port);
    const toOk = !redirecting || /^https?:\/\/\S+$/i.test(goesTo);
    const named = proxyList(proxies);
    const proxiesOk = !behind || inFront !== "proxy" || named.length > 0;
    setAppError(service || redirecting ? undefined : t`Choose the app this domain reaches.`);
    setToError(toOk ? undefined : t`Give the address it sends to, such as https://example.org.`);
    setDomainError(domainOk ? undefined : t`Name the domain, such as app.example.com.`);
    setPortError(portOk ? undefined : t`Use a port from 1 to 65535.`);
    setProxiesError(proxiesOk ? undefined : t`Name what connects to this server, as an address or a range.`);
    setFailure(undefined);
    // A way in that can't be taken says why, with what fixes it, under the name; so
    // does a check Cloudflare couldn't answer, with Check again. Neither plans a plain
    // route in a connected zone: Another domain is a person's own choice (the review's B-M1).
    if (outsideProject || cf.failed || (publishing && !wayTakeable)) {
      setWaysOpen(false);
      return;
    }
    if ((!service && !redirecting) || !domainOk || !portOk || !proxiesOk || !toOk || environment === "") {
      revealError();
      return;
    }
    // The cards close as the plan opens, so a cancelled plan comes back to the way's line.
    setWaysOpen(false);
    const front = () => frontOf(behind, inFront, named);
    // How a new route is reached: the way's own fields and what it publishes, or what's in front.
    const reached = (takes: readonly TakeOverInit[]) => {
      if (!publishes) return behind ? front() : {};
      const publish = publishOf(way.way, way.tunnel, provider, takes);
      return { ...routeOfWay(way.way, cf.check?.sslMode ?? ""), ...(publish ? { publish } : {}) };
    };
    setBusy(true);
    let change: Change | undefined;
    try {
      change = (
        await client.proposeChange({
          summary: start
            ? t`Move ${typed} behind what's in front`
            : redirecting
              ? t`Redirect ${wanted} to ${goesTo}`
              : follows
                ? t`Send ${wanted} to ${name}, and point ${followsApp} at it`
                : t`Send ${wanted} to ${name}`,
          edits: start
            ? [
                ...start.routes.map((route) => ({
                  resource: { kind: "route", scope: environment, name: route.name },
                  spec: {
                    kind: {
                      case: "route" as const,
                      value: {
                        // Who gets in and whether it's held stay as they are, so moving it opens
                        // nothing: a front that's plain for anyone the plan refuses to a route with
                        // a password or a sign-in (#598's final check, M2).
                        ...routeAsItIs(wanted, route),
                        // A person's own port is for the route they asked from. The rest stay as they are.
                        ...(route.hostPort
                          ? { hostPort: route.name === start.route ? Number(port) : route.hostPort }
                          : { port: route.name === start.route ? Number(port) : route.port }),
                        // What's in front is the sheet's to say: a certificate, or what it names.
                        tls: "",
                        plainForAnyone: false,
                        ...(behind ? front() : {}),
                      },
                    },
                  },
                })),
                ...followers.map(({ domain: from, route }) => {
                  const kept = route.redirect;
                  // Its to follows how the site is reached: plain HTTP to anyone, or HTTPS.
                  const scheme = behind && inFront === "anyone" ? "http" : "https";
                  const after = kept?.to.replace(/^https?:\/\/[^/]+/i, "") ?? "";
                  return {
                    resource: { kind: "route", scope: environment, name: route.name },
                    spec: {
                      kind: {
                        case: "route" as const,
                        value: {
                          domain: from,
                          redirect: {
                            to: `${scheme}://${start.domain}${after}`,
                            temporary: kept?.temporary ?? false,
                            dropPath: kept?.dropPath ?? false,
                          },
                          tls: "",
                          trustedProxies: [],
                          plainForAnyone: false,
                          ...(route.publish ? { publish: route.publish } : {}),
                          ...(behind ? front() : {}),
                        },
                      },
                    },
                  };
                }),
              ]
            : [
                {
                  resource: {
                    kind: "route",
                    scope: environment,
                    name: held?.name ?? routeName(wanted, known.data?.domains),
                  },
                  spec: {
                    kind: {
                      case: "route",
                      value: redirecting
                        ? {
                            domain: wanted,
                            redirect: { to: goesTo, temporary, dropPath },
                            ...reached(takesOver),
                          }
                        : {
                            domain: wanted,
                            service: name,
                            ...(onServer ? { hostPort: Number(port) } : { port: Number(port) }),
                            ...reached(takesOver),
                            // Watched by an uptime check unless the person said not to (#835).
                            ...(watchIt ? {} : { unwatched: true }),
                          },
                    },
                  },
                  ...(server ? { servers: [server] } : {}),
                },
                // Its other half redirects to it, reached as it is, permanently, keeping the path.
                ...(alsoCompanion && !redirecting && companion.companion !== ""
                  ? [
                      {
                        resource: {
                          kind: "route",
                          scope: environment,
                          // A name no route has, so a claim the engine refuses is never an update of another's route.
                          name: freshRouteName(companion.companion, known.data?.domains),
                        },
                        spec: {
                          kind: {
                            case: "route" as const,
                            value: {
                              domain: companion.companion,
                              redirect: {
                                to: `${behind && inFront === "anyone" ? "http" : "https"}://${wanted}`,
                              },
                              // Published the same way, taking over nothing of its own.
                              ...reached([]),
                            },
                          },
                        },
                        ...(server ? { servers: [server] } : {}),
                      },
                    ]
                  : []),
              ],
        })
      ).change;
    } catch (err) {
      const said = sentence(err);
      // A server's agent refusing the route is about the route. What else fails isn't.
      if (reasonOf(err) === "invalid" || /^can't plan on /i.test(said)) land(said, wanted);
      else setFailure(said);
      setBusy(false);
      revealError();
      return;
    }
    setBusy(false);
    setPlan({
      kind: "plan",
      make: () => Promise.resolve({ change }),
      retryLabel: t`Edit and try again`,
      onDone: () => {
        onClose();
        landOnApp();
      },
      onClose: (after) => {
        // Cancelling comes back here, with what you typed, as failing does, saying why
        // at the field. Anything else is under way or over.
        if (after?.state === ChangeState.FAILED) {
          land(after.error, wanted);
          reveal.current = true;
          setPlan(undefined);
        } else if (!after || after.state === ChangeState.PLANNED) setPlan(undefined);
        else {
          onClose();
          landOnApp();
        }
      },
    });
  };

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      initialFocusRef={field}
      title={
        start
          ? t`Move ${typed} behind what's in front`
          : choosing || redirecting || (offersRedirect && !service)
            ? t`Add a domain`
            : t`Add a domain to ${name}`
      }
      subtitle={
        redirecting
          ? t`Visitors to this name are sent on to the address you give`
          : publishing
            ? t`People reach the app at this name`
            : !noCertificate
              ? t`People reach the app at this name, over HTTPS`
              : inFront === "anyone"
                ? t`People reach the app at this name, over plain HTTP`
                : t`People reach the app at this name, through what's in front of this server`
      }
      onClose={onClose}
      className={cx("app-sheet", phone && "is-phone")}
      footer={
        <>
          <span className="dw-sheet-note">
            <Icon name="commit" size={14} />
            <Trans>Nothing changes until you apply the plan</Trans>
          </span>
          <Button variant="ghost" size={phone ? "lg" : "md"} onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            size={phone ? "lg" : "md"}
            loading={busy || waiting}
            disabled={outsideProject}
            kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
            onClick={() => void submit()}
          >
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <form
        ref={form}
        className="app-form"
        onSubmit={(event) => void submit(event)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) void submit(event);
        }}
      >
        {/* Lets Enter in a field do what the button in the sheet's footer does. */}
        <button type="submit" hidden />
        {failure ? <Callout tone="critical">{failure}</Callout> : null}
        {zones.length > 0 && !start ? (
          <DomainName
            field={field}
            sub={subdomainOf(domain, zone)}
            onSub={(value) => {
              const low = nameIn(value, "");
              const into = zoneOfName(zones, low);
              if (into) setZonePicked(into);
              else if (zoneOfName(connectedZones, low)) setZonePicked("");
              else if (zone) setZonePicked(zone);
              setDomain(into ? subdomainOf(low, into) : value);
              setDomainError(undefined);
            }}
            zone={zone}
            onZone={(next) => {
              // Another domain is typed whole: what's typed so far becomes the start of it.
              if (next === "" && zone !== "") setDomain(domain.trim() === "" ? "" : typed);
              else if (next !== "") setDomain(subdomainOf(domain, zone || next));
              setZonePicked(next);
              setWayPicked(undefined);
              setDomainError(undefined);
            }}
            zones={zones}
            error={domainError}
          ></DomainName>
        ) : (
          <TextField
            ref={field}
            label={t`Domain`}
            mono
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
            value={domain}
            readOnly={start !== undefined}
            error={domainError}
            hint={
              manual
                ? !noCertificate
                  ? t`Its certificate comes by itself once its DNS points here.`
                  : inFront === "anyone"
                    ? t`Point its DNS at this server.`
                    : t`Point its DNS at your tunnel or proxy.`
                : undefined
            }
            onChange={(event) => {
              setDomain(event.target.value);
              setDomainError(undefined);
            }}
          />
        )}
        {outsideProject ? (
          <ProviderReach admin={admin} provider={providerInfo?.name} onUse={useHere} />
        ) : publishing || cf.checking || cf.failed ? (
          <NameState
            check={cf.check}
            checking={cf.checking}
            domain={typed}
            sub={subdomainOf(typed, cf.check?.zone || zone)}
            zone={cf.check?.zone || zone}
            companion={cf.companion}
            companionOn={alsoCompanion}
            onCompanion={() => setCompanionOpen(true)}
            failed={cf.failed}
            onAgain={cf.again}
            takesOver={takesOver}
            onTakesOver={(items) => setConsent({ domain: typed, items })}
            onSub={(value) => {
              setDomain(zone || !cf.check?.zone ? value : nameIn(value, cf.check.zone));
              setDomainError(undefined);
            }}
          />
        ) : null}
        {offersRedirect ? (
          <fieldset className="app-front">
            <legend className="dw-field-label">
              <Trans>It goes to</Trans>
            </legend>
            {[
              { value: false, label: t`An app`, hint: t`Visitors reach an app here.` },
              { value: true, label: t`A URL`, hint: t`Visitors are sent on, such as from an old domain.` },
            ].map((choice) => (
              <label
                key={String(choice.value)}
                className={cx("app-front-choice", redirecting === choice.value && "is-on")}
              >
                <input
                  type="radio"
                  name="add-domain-goes-to"
                  checked={redirecting === choice.value}
                  onChange={() => {
                    setRedirecting(choice.value);
                    setAppError(undefined);
                    setToError(undefined);
                    setPortError(undefined);
                  }}
                />
                <span className="app-front-text">
                  <span className="app-front-name">{choice.label}</span>
                  <span className="app-front-hint">{choice.hint}</span>
                </span>
              </label>
            ))}
          </fieldset>
        ) : null}
        {redirecting ? (
          <TextField
            label={t`Redirect to`}
            mono
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
            placeholder="https://example.org"
            value={to}
            error={toError}
            onChange={(event) => {
              setTo(event.target.value);
              setToError(undefined);
            }}
          />
        ) : null}
        {redirecting && places.environments.length > 1 ? (
          <SelectField
            label={t`Environment`}
            value={environment}
            options={places.environments.map((e) => ({ value: e.id, label: e.label }))}
            onChange={(event) => setPlaceChosen(event.target.value)}
          />
        ) : null}
        {choosing ? (
          <SelectField
            label={t`App`}
            error={appError}
            value={chosen}
            options={[
              { value: "", label: t`Choose an app` },
              ...(apps ?? []).map((a) => ({ value: a.service.id, label: a.label })),
            ]}
            onChange={(event) => choose(event.target.value)}
          />
        ) : null}
        {follows ? (
          <p className="app-domain-follows">
            <Icon name="globe" size={14} />
            <span>{t`Points ${followsApp} at it too: ${followsNames}`}</span>
          </p>
        ) : null}
        {publishing ? (
          <WayIn
            check={cf.check}
            way={way.way}
            tunnel={way.tunnel}
            onWay={(next, tunnel) => setWayPicked({ way: next, tunnel })}
            open={waysOpen}
            onOpen={setWaysOpen}
            server={serverOf(fleet, server)?.name ?? ""}
            address={publicAddressOf(serverOf(fleet, server))}
            admin={admin && providerInfo !== undefined}
            onStrict={toStrict}
            onLeave={onClose}
          />
        ) : manual ? (
          <DomainRecord
            server={serverOf(fleet, server)}
            domain={typed}
            tunnel={noCertificate}
            tunnelName={switchName}
            onLeave={onClose}
          />
        ) : null}
        {/* In a connected zone, the name's line says its other half, with Change one step away. */}
        {manual || companionOpen ? (
          <CompanionChoice
            server={serverOf(fleet, server)}
            read={companion}
            on={alsoCompanion}
            onChange={setAlsoCompanion}
            tunnel={behind || publishes}
            env={environment}
            strict
            already={companionAlready}
            onLeave={onClose}
          />
        ) : null}
        {leaves ? (
          <Callout tone="warning">
            <Trans>
              {typed} goes to {leavesApp} now. Going on moves it here, and it leaves {leavesApp}.
            </Trans>
          </Callout>
        ) : null}
        {redirecting ? null : publishing &&
          !onServer &&
          validPort(port) &&
          !portOpen &&
          service &&
          !choosing &&
          portError === undefined ? (
          // From an app's page both are set, so what it reaches is a fact with Change beside it.
          <dl className="domain-step-facts">
            <dt>
              <Trans>Reaches</Trans>
            </dt>
            <dd>{t`${name} on port ${port}`}</dd>
            <dd>
              <button
                type="button"
                className="domain-step-change"
                aria-label={t`Change the port it reaches`}
                onClick={() => setPortOpen(true)}
              >
                <Trans>Change</Trans>
              </button>
            </dd>
          </dl>
        ) : picksPort ? (
          <SelectField
            label={t`Port in the app`}
            value={port}
            error={portError}
            options={[
              ...appPorts.map((p) => ({ value: String(p.port), label: portLabel(p) })),
              { value: "other", label: t({ message: "Another port", context: "a port typed in" }) },
            ]}
            onChange={(event) => {
              portTyped.current = true;
              setPortError(undefined);
              if (event.target.value === "other") {
                setTypingPort(true);
                setPort("");
              } else setPort(event.target.value);
            }}
          />
        ) : (
          <TextField
            label={onServer ? t`Port on the server` : t`Port in the app`}
            mono
            inputMode="numeric"
            value={port}
            error={portError}
            hint={
              onServer
                ? t`What it listens on on the server, such as 8080; 80 is the proxy's.`
                : t`What the app listens on, such as 80.`
            }
            onChange={(event) => {
              portTyped.current = true;
              setPort(event.target.value);
              setPortError(undefined);
            }}
          />
        )}
        {!redirecting && portElsewhere && portError === undefined ? (
          <StateLine
            status="degraded"
            action={
              webPort !== undefined && webPort !== Number(port) ? (
                <button
                  type="button"
                  className="domain-step-act"
                  onClick={() => {
                    portTyped.current = true;
                    setTypingPort(false);
                    setPort(String(webPort));
                  }}
                >
                  {t`Use ${webPort}`}
                </button>
              ) : undefined
            }
          >
            {t`Port ${port} is ${portElsewhere}'s, which serves no web page`}
          </StateLine>
        ) : !redirecting && !onServer && portError === undefined ? (
          <PortState port={validPort(port) ? Number(port) : 0} ports={appPorts} read={servicePorts} />
        ) : null}
        {/* Dockwell watches a new domain with an uptime check, said in a line with one way out (#835). */}
        {!redirecting && !start ? (
          <dl className="domain-step-facts">
            <dt>
              <Trans>Uptime check</Trans>
            </dt>
            <dd>{watchIt ? t`Every minute, from the control server` : t`Not watched`}</dd>
            <dd>
              <button type="button" className="domain-step-change" onClick={() => setWatchIt((w) => !w)}>
                {watchIt ? t`Don't watch` : t`Watch it`}
              </button>
            </dd>
          </dl>
        ) : null}
        {servers.length > 1 ? (
          <SelectField
            label={t`Server`}
            hint={t`A route runs on the one server its DNS points at.`}
            value={server}
            options={servers.map((d) => ({ value: d.serverId, label: d.serverName }))}
            onChange={(event) => setServer(event.target.value)}
          />
        ) : null}
        {/* A permanent redirect that keeps the path is what most want: the rest is one step away. */}
        {redirecting ? (
          <Advanced
            label={t`Redirect`}
            summary={
              temporary
                ? dropPath
                  ? t`For a while, to the address as it is`
                  : t`For a while, keeping the path`
                : dropPath
                  ? t`Permanently, to the address as it is`
                  : t`Permanently, keeping the path`
            }
          >
            <label className="app-check">
              <input type="checkbox" checked={temporary} onChange={(event) => setTemporary(event.target.checked)} />
              <span>
                <Trans>For a while (302), so browsers ask again each time</Trans>
              </span>
            </label>
            <label className="app-check">
              <input type="checkbox" checked={dropPath} onChange={(event) => setDropPath(event.target.checked)} />
              <span>
                <Trans>Send every request to the address as it is, without its path</Trans>
              </span>
            </label>
          </Advanced>
        ) : null}
        {/* Something else in front: what it is, one step away, as the way says there's no certificate. */}
        {/* What the way sets, one step away: the path and who the route trusts (the review's B-L5). */}
        {publishes ? (
          <Advanced
            label={t`Advanced`}
            summary={
              way.way === RouteWay.PROXY
                ? t`Path / · Answers only Cloudflare`
                : way.way === RouteWay.OWN_HTTPS
                  ? t`Path / · Answers anyone`
                  : t`Path / · Answers only ${serverName}'s tunnel`
            }
          >
            <dl className="domain-step-facts">
              <dt>
                <Trans>Path</Trans>
              </dt>
              <dd>
                <code>/</code>
              </dd>
              <dd />
              <dt>
                <Trans>Believes</Trans>
              </dt>
              <dd>
                {way.way === RouteWay.PROXY ? (
                  <Trans>Cloudflare's addresses, for who a visitor is</Trans>
                ) : way.way === RouteWay.OWN_HTTPS ? (
                  <Trans>No proxy: each visitor reaches it as themselves</Trans>
                ) : (
                  <Trans>
                    The tunnel on <code>127.0.0.1</code>, for who a visitor is
                  </Trans>
                )}
              </dd>
              <dd />
            </dl>
          </Advanced>
        ) : null}
        {publishing && behind ? (
          <Advanced
            label={t`What's in front`}
            summary={
              inFront === "anyone"
                ? t`Nothing: plain HTTP to anyone`
                : inFront === "tunnel"
                  ? t`A tunnel on this server`
                  : inFront === "cloudflare"
                    ? t`Cloudflare's proxy`
                    : t`Another proxy`
            }
            defaultOpen={proxiesError !== undefined}
          >
            <FrontFields
              fixed
              noCertificate
              onNoCertificate={() => undefined}
              inFront={inFront}
              onInFront={(value) => {
                setInFront(value);
                setProxiesError(undefined);
              }}
              proxies={proxies}
              onProxies={(value) => {
                setProxies(value);
                setProxiesError(undefined);
              }}
              proxiesError={proxiesError}
            />
          </Advanced>
        ) : null}
        {/* HTTPS is on, with its certificate coming by itself: what's in front is one step away. */}
        {publishing ? null : (
          <Advanced
            label={t`HTTPS`}
            summary={
              !noCertificate
                ? t`On, with a certificate that comes by itself`
                : inFront === "anyone"
                  ? t`Off: plain HTTP to anyone`
                  : t`Handled in front of this server`
            }
            defaultOpen={noCertificate || proxiesError !== undefined}
          >
            <FrontFields
              noCertificate={noCertificate}
              onNoCertificate={setNoCertificate}
              inFront={inFront}
              onInFront={(value) => {
                setInFront(value);
                setProxiesError(undefined);
              }}
              proxies={proxies}
              onProxies={(value) => {
                setProxies(value);
                setProxiesError(undefined);
              }}
              proxiesError={proxiesError}
            />
          </Advanced>
        )}
      </form>
    </Sheet>
  );
}
