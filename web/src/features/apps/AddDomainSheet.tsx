// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  CatalogService,
  type Change,
  ChangeService,
  ChangeState,
  type Domain,
  type DomainRoute,
  DomainService,
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
import { addressEdits } from "../catalog/access";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { type FleetState, useFleet } from "../servers/useFleet";
import { appOf, environmentOf, nameOf } from "./app";
import { useRouteTarget, useTunnelSwitchName } from "./DomainNote";
import { DomainRecord } from "./DomainRecord";
import { formRefusal, proxyList, refusedField, routeName } from "./domains";
import { FrontFields, frontOf, type InFront } from "./FrontFields";
import { validPort } from "./form";
import { useServices } from "./useServices";

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
 * as that's who the route answers and who the app believes on who a visitor is. An app
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
}: {
  /** The app it's for, when it's opened from one. */
  service?: ServiceInfo;
  /** The apps to choose from, when it isn't. */
  apps?: readonly AppChoice[];
  onClose: () => void;
  /** A domain that needs moving behind a tunnel, as it's refused a certificate. */
  start?: MoveBehind;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const [chosen, setChosen] = useState("");
  const choosing = given === undefined && (apps?.length ?? 0) > 1;
  const service = given ?? (choosing ? apps?.find((a) => a.service.id === chosen) : apps?.[0])?.service;
  const name = service ? nameOf(service) : "";
  const environment = service ? environmentOf(service) : "";
  const known = useQuery(DomainService.method.listDomains, { environment }, { enabled: service !== undefined });
  const onServer = (service && appOf(service)?.hostNetwork) ?? false;
  const servers = service?.deployments ?? [];
  const [server, setServer] = useState(servers[0]?.serverId ?? "");
  const [domain, setDomain] = useState(start?.domain ?? "");
  const [port, setPort] = useState(start?.port ?? portOf(service));
  // What you typed stays as you choose another app. What it started from follows.
  const portTyped = useRef(false);
  const [appError, setAppError] = useState<string>();
  const [noCertificate, setNoCertificate] = useState(start !== undefined);
  const [inFront, setInFront] = useState<InFront>("tunnel");
  const [proxies, setProxies] = useState("");
  const [proxiesError, setProxiesError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [domainError, setDomainError] = useState<string>();
  const [portError, setPortError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [plan, setPlan] = useState<Opening>();
  const field = useRef<HTMLInputElement>(null);
  const form = useRef<HTMLFormElement>(null);
  const switchName = useTunnelSwitchName();
  const fleet = useFleet();
  const navigate = useNavigate();
  // Once it's on its way, the app's page is where the domain's state shows as it's served.
  const landOnApp = () => {
    if (service) void navigate({ to: "/apps/$serviceId", params: { serviceId: service.id } });
  };
  // The ports the app listens on, its own, to pick from; any other is typed.
  const listens = onServer
    ? []
    : [...new Set((service ? (appOf(service)?.ports ?? []) : []).map((p) => p.containerPort).filter((p) => p > 0))];
  const [typingPort, setTypingPort] = useState(false);
  const picksPort = listens.length > 0 && !typingPort && (port === "" || listens.includes(Number(port)));
  // A domain's root may be with another app already, whatever its route is called.
  // Going on moves it, the same either way, so this says that before it does.
  const typed = domain.trim().toLowerCase();
  const held = known.data?.domains.find((d) => d.name === typed)?.routes.find((route) => route.path === "");
  const leaves = !start && held && held.service !== name ? held : undefined;
  const target = useRouteTarget();
  const leavesApp = leaves ? target(leaves) : "";
  // A catalog app builds its links from variables that follow its domain (Entry.address_variables):
  // its first domain sets them in the same plan, rather than leaving that for after.
  const services = useServices().data ?? [];
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
    switch (refusedField(said, { domain: wanted, port, onServer })) {
      case "proxies":
        if (noCertificate && inFront === "proxy") setProxiesError(said);
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
    const wanted = domain.trim().toLowerCase();
    const domainOk = wanted !== "" && !/\s/.test(wanted);
    const portOk = validPort(port);
    const named = proxyList(proxies);
    const proxiesOk = !noCertificate || inFront !== "proxy" || named.length > 0;
    setAppError(service ? undefined : t`Choose the app this domain reaches.`);
    setDomainError(domainOk ? undefined : t`Name the domain, such as app.example.com.`);
    setPortError(portOk ? undefined : t`Use a port from 1 to 65535.`);
    setProxiesError(proxiesOk ? undefined : t`Name what connects to this server, as an address or a range.`);
    setFailure(undefined);
    if (!service || !domainOk || !portOk || !proxiesOk) {
      revealError();
      return;
    }
    const front = () => frontOf(noCertificate, inFront, named);
    setBusy(true);
    let change: Change | undefined;
    try {
      change = (
        await client.proposeChange({
          summary: start
            ? t`Move ${typed} behind what's in front`
            : follows
              ? t`Send ${wanted} to ${name}, and point ${followsApp} at it`
              : t`Send ${wanted} to ${name}`,
          edits: start
            ? start.routes.map((route) => ({
                resource: { kind: "route", scope: environment, name: route.name },
                spec: {
                  kind: {
                    case: "route" as const,
                    value: {
                      domain: wanted,
                      path: route.path,
                      service: route.service,
                      // A person's own port is for the route they asked from. The rest stay as they are.
                      ...(route.hostPort
                        ? { hostPort: route.name === start.route ? Number(port) : route.hostPort }
                        : { port: route.name === start.route ? Number(port) : route.port }),
                      ...(noCertificate ? front() : {}),
                    },
                  },
                },
              }))
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
                      value: {
                        domain: wanted,
                        service: name,
                        ...(onServer ? { hostPort: Number(port) } : { port: Number(port) }),
                        ...(noCertificate ? front() : {}),
                      },
                    },
                  },
                  ...(server ? { servers: [server] } : {}),
                },
                ...(follows
                  ? addressEdits(follows, wanted, (n) =>
                      services.find((s) => environmentOf(s) === environment && nameOf(s) === n),
                    )
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
      title={start ? t`Move ${typed} behind what's in front` : choosing ? t`Add a domain` : t`Add a domain to ${name}`}
      subtitle={
        !noCertificate
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
            loading={busy}
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
            !noCertificate
              ? t`Its certificate comes by itself once its DNS points here.`
              : inFront === "anyone"
                ? t`Point its DNS at this server.`
                : t`Point its DNS at your tunnel or proxy.`
          }
          onChange={(event) => {
            setDomain(event.target.value);
            setDomainError(undefined);
          }}
        />
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
        <DomainRecord
          server={serverOf(fleet, server)}
          domain={typed}
          tunnel={noCertificate}
          tunnelName={switchName}
          onLeave={onClose}
        />
        {leaves ? (
          <Callout tone="warning">
            <Trans>
              {typed} goes to {leavesApp} now. Going on moves it here, and it leaves {leavesApp}.
            </Trans>
          </Callout>
        ) : null}
        {picksPort ? (
          <SelectField
            label={t`Port in the app`}
            value={port}
            error={portError}
            options={[
              ...listens.map((p) => ({ value: String(p), label: String(p) })),
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
        {servers.length > 1 ? (
          <SelectField
            label={t`Server`}
            hint={t`A route runs on the one server its DNS points at.`}
            value={server}
            options={servers.map((d) => ({ value: d.serverId, label: d.serverName }))}
            onChange={(event) => setServer(event.target.value)}
          />
        ) : null}
        {/* HTTPS is on, with its certificate coming by itself: what's in front is one step away. */}
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
      </form>
    </Sheet>
  );
}
