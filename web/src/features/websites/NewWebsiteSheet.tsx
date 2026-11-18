// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeState, RouteWay, type Server, WebsiteService } from "@dockwell/api";
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
import { type FormEvent, useEffect, useId, useMemo, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { nameIn } from "../apps/AddDomainSheet";
import { useTunnelSwitchName } from "../apps/DomainNote";
import { DomainRecord } from "../apps/DomainRecord";
import { DomainName, NameState, WayIn } from "../apps/DomainStep";
import { formRefusal, proxyList, refusedField } from "../apps/domains";
import { FrontFields, frontOf, type InFront } from "../apps/FrontFields";
import {
  pickedOf,
  publicAddressOf,
  publishOf,
  routeOfWay,
  type TakeOverInit,
  useCloudflareCheck,
  useInstallAdmin,
  useZones,
  wayOf,
} from "../apps/publish";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { defaultServer } from "../servers/pick";
import { type FleetState, useFleet, useFleetView } from "../servers/useFleet";
import { CompanionChoice, useCompanion, useCompanionChoice } from "./Companion";
import {
  BLANK,
  type Missing,
  missing,
  type Serves,
  type Settings,
  refusedField as servesField,
  specOf,
} from "./settings";
import { SplitWebsiteFields, useMissingWords, useServesWords } from "./WebsiteFields";
import "../apps/Apps.css";
import "./Websites.css";
import { useMay } from "../auth/may";

/** The server a website goes on: the one chosen, or the only one there is. */
function serverOf(fleet: FleetState, id: string): Server | undefined {
  if (fleet.status === "connecting") return undefined;
  return fleet.servers.find((s) => s.id === id) ?? (fleet.servers.length === 1 ? fleet.servers[0] : undefined);
}

/**
 * New website: its domain, what it serves and where from, then the plan. What has a good
 * default (index files, who PHP runs as, HTTPS and what's in front) waits under Advanced,
 * each a line that says what it is. The folder follows the domain, as /var/www/<domain>,
 * until one is picked, and is made with the site when it isn't there. Once it applies,
 * the website's own page opens, with its state and its address to visit.
 */
export function NewWebsiteSheet({
  onClose,
  onMade,
  start,
}: {
  onClose: () => void;
  /** Opens the website made, by its ID, once its change applies. */
  onMade?: (websiteId: string) => void;
  /** What it starts from, as a runtime's card asks for PHP of its version. */
  start?: { serves?: Serves; version?: string };
}) {
  const { t } = useLingui();
  // A website makes a project of its own, which is an admin's (ADR 0049): off, with why, for anyone else.
  const mayCreate = useMay()(WebsiteService.method.createWebsite);
  const transport = useTransport();
  const phone = useMediaQuery(PHONE_QUERY);
  const client = useMemo(() => createClient(WebsiteService, transport), [transport]);
  const fleet = useFleet();
  const servers = fleet.status === "connecting" ? [] : fleet.servers;
  const [picked, setServer] = useState("");
  // Until a person picks one, the connected, healthy server with the most free disk, as
  // a new app takes the one with the most free memory (#798).
  const view = useFleetView().servers ?? [];
  const freeDisk = (id: string) => {
    const disks = servers.find((s) => s.id === id)?.stats?.disks ?? [];
    const disk = disks.find((d) => d.mountPoint === "/") ?? disks[0];
    return disk ? Number(disk.sizeBytes - disk.usedBytes) : -1;
  };
  // Chosen once, as the fleet first reads, so it holds still as stats come in (#849's L7).
  const best = defaultServer(view, freeDisk)?.id ?? "";
  const [first, setFirst] = useState("");
  // Once the servers say how much disk they have, or there's one, as before that every
  // server reads as having none.
  const read = view.length === 1 || servers.some((s) => (s.stats?.disks.length ?? 0) > 0);
  useEffect(() => {
    if (first === "" && best !== "" && read) setFirst(best);
  }, [first, best, read]);
  const server = picked || first || best;
  const [domain, setDomain] = useState("");
  const [settings, setSettings] = useState<Settings>({
    ...BLANK,
    serves: start?.serves ?? BLANK.serves,
    version: start?.version ?? "",
  });
  const rootTyped = useRef(false);
  const [noCertificate, setNoCertificate] = useState(false);
  const [inFront, setInFront] = useState<InFront>("tunnel");
  const [proxies, setProxies] = useState("");
  const [proxiesError, setProxiesError] = useState<string>();
  const [domainError, setDomainError] = useState<string>();
  const [fieldError, setFieldError] = useState<{ field: Missing["field"]; said: string }>();
  const [serverError, setServerError] = useState<string>();
  const [failure, setFailure] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [plan, setPlan] = useState<Opening>();
  // Review plan pressed while the check reads Cloudflare goes on once it has.
  const [waiting, setWaiting] = useState(false);
  const [httpsOpen, setHttpsOpen] = useState(false);
  const field = useRef<HTMLInputElement>(null);
  const form = useRef<HTMLFormElement>(null);
  const choiceName = useId();
  const switchName = useTunnelSwitchName();
  const missingWords = useMissingWords();
  const servesWords = useServesWords();
  // A name in a zone a connected provider writes in, published for you: an admin makes a
  // website, and its plan adds the website's project to that provider (ADR 0104).
  const admin = useInstallAdmin();
  const { zones, providers } = useZones("", mayCreate.allowed && admin);
  const [zonePicked, setZonePicked] = useState<string>();
  const zone =
    zones.length === 0
      ? ""
      : zonePicked !== undefined && (zonePicked === "" || zones.some((z) => z.zone === zonePicked))
        ? zonePicked
        : (zones[0]?.zone ?? "");
  const publishing = zone !== "";
  const typed = nameIn(domain, zone);
  const several = servers.length > 1;
  const cf = useCloudflareCheck(serverOf(fleet, server)?.id ?? "", typed, "", publishing);
  const [wayPicked, setWayPicked] = useState<{ way: RouteWay; tunnel: string }>();
  const way =
    wayPicked && wayOf(cf.check, wayPicked.way, wayPicked.tunnel)?.off === "" ? wayPicked : pickedOf(cf.check);
  const wayTakeable = wayOf(cf.check, way.way, way.tunnel)?.off === "";
  const [waysOpen, setWaysOpen] = useState(false);
  // The choice to leave a name's other half out, one step away in a connected zone.
  const [companionOpen, setCompanionOpen] = useState(false);
  const [consent, setConsent] = useState<{ domain: string; items: TakeOverInit[] }>({ domain: "", items: [] });
  const takesOver = consent.domain === typed ? consent.items : [];
  const provider = cf.check?.provider || zones.find((z) => z.zone === zone)?.provider || "";
  const publishes = publishing && cf.check !== undefined && wayTakeable && way.way !== RouteWay.FRONT;
  const providerInfo = providers.find((p) => p.name === provider && p.provider !== "");
  /** Sets the zone to Full (strict) on its DNS provider's own plan (ADR 0104, section 2). */
  const toStrict = () => {
    if (!providerInfo) return;
    const p = providerInfo;
    setPlan({
      kind: "propose",
      summary: t`Set ${zone} to Full (strict)`,
      edits: [
        {
          resource: { kind: "dns.provider", name: p.name },
          spec: {
            kind: {
              case: "dnsProvider",
              value: {
                provider: "cloudflare",
                zones: p.zones,
                projects: p.projects,
                strictZones: [...new Set([...p.strictZones, zone])],
              },
            },
          },
        },
      ],
      retryLabel: t`Edit and try again`,
      onClose: () => {
        setPlan(undefined);
        cf.again();
      },
    });
  };
  const behind = publishing ? way.way === RouteWay.FRONT : noCertificate;
  // The folder follows the name until it's typed, as /var/www/<domain>.
  const follow = (name: string) => {
    if (rootTyped.current) return;
    setSettings((was) => ({ ...was, root: name ? `/var/www/${name}` : "" }));
    setFieldError((said) => (said?.field === "root" ? undefined : said));
  };
  // www for a bare domain, or the bare domain for www, on by what DNS says of it.
  const companion = useCompanion(serverOf(fleet, server)?.id ?? "", typed);
  const [alsoCompanion, setAlsoCompanion] = useCompanionChoice(companion, "", false, publishing);

  /** Takes you to the first field that was refused, ready to change, as New app does. */
  const revealError = () =>
    requestAnimationFrame(() => {
      const refused = form.current?.querySelector<HTMLElement>('[aria-invalid="true"]');
      refused?.focus();
      if (refused instanceof HTMLInputElement) refused.select();
    });

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
    const at = refusedField(said, { domain: wanted, port: settings.port, onServer: settings.serves === "port" });
    if (at === "domain") return setDomainError(said);
    if (at === "proxies" && behind && inFront === "proxy") return setProxiesError(said);
    const target = servesField(said, settings.serves) || (at === "port" && settings.serves === "port" ? "port" : "");
    if (target) return setFieldError({ field: target, said });
    setFailure(said);
  };

  // biome-ignore lint/correctness/useExhaustiveDependencies: it goes on once the check is in, and submit is new each time.
  useEffect(() => {
    if (!waiting || cf.checking) return;
    setWaiting(false);
    void submit();
  }, [waiting, cf.checking]);

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    // Enter and Ctrl+Enter follow the role, as the button does.
    if (busy || !mayCreate.allowed) return;
    const domainOk = typed !== "" && !/\s/.test(typed);
    const gap = missing(settings);
    const named = proxyList(proxies);
    const proxiesOk = !behind || inFront !== "proxy" || named.length > 0;
    const serverOk = !several || server !== "";
    setDomainError(domainOk ? undefined : t`Name the domain, such as blog.example.com.`);
    setFieldError(gap ? { field: gap.field, said: missingWords(gap) } : undefined);
    setProxiesError(proxiesOk ? undefined : t`Name what connects to this server, as an address or a range.`);
    setServerError(serverOk ? undefined : t`Choose the server it runs on.`);
    setFailure(undefined);
    if (!domainOk || gap || !proxiesOk || !serverOk) {
      revealError();
      return;
    }
    // A name in a connected zone waits for the check, and a way that can't be taken says why.
    // A name in a connected zone waits for the check, then plans the way it took; one
    // that can't be taken, or a check Cloudflare couldn't answer, says why under the name.
    if (publishing && cf.checking) {
      setWaiting(true);
      return;
    }
    if (publishing && (cf.failed || (cf.check && !wayTakeable))) {
      setWaysOpen(false);
      return;
    }
    // The cards close as the plan opens, so a cancelled plan comes back to the way's line.
    setWaysOpen(false);
    const publish = publishes ? publishOf(way.way, way.tunnel, provider, takesOver) : undefined;
    const { proof: _proof, ...wayFields } = routeOfWay(way.way, cf.check?.sslMode ?? "");
    const front = publishes ? { ...wayFields, ...(publish ? { publish } : {}) } : frontOf(behind, inFront, named);
    const website = { ...specOf(settings), redirectFrom: alsoCompanion ? [companion.companion] : [] };
    setBusy(true);
    let change: Change | undefined;
    try {
      change = (
        await client.createWebsite({
          domain: typed,
          website,
          ...front,
          ...(server ? { servers: [server] } : {}),
        })
      ).change;
    } catch (err) {
      const said = sentence(err);
      // A server's agent refusing the route is about the route. What else fails isn't.
      if (reasonOf(err) === "invalid" || /^can't plan on /i.test(said)) land(said, typed);
      else setFailure(said);
      setBusy(false);
      revealError();
      return;
    }
    setBusy(false);
    // The website is its project, whose ID is the website's.
    const made = change?.plan?.resources.find((r) => r.resource?.kind === "project")?.id ?? "";
    setPlan({
      kind: "plan",
      make: () => Promise.resolve({ change }),
      retryLabel: t`Edit and try again`,
      ...(made && onMade ? { doneLabel: t`Go to the website` } : {}),
      onDone: onClose,
      onClose: (after) => {
        // Cancelling comes back here, with what you typed, as failing does, saying why
        // at the field. Anything else is under way or over.
        if (after?.state === ChangeState.FAILED) {
          land(after.error, typed);
          reveal.current = true;
          setPlan(undefined);
        } else if (!after || after.state === ChangeState.PLANNED || after.state === ChangeState.DISCARDED) {
          setPlan(undefined);
          // Undone in the same sheet, it has no page to open.
        } else if (made && onMade && after.reverts === "") onMade(made);
        else onClose();
      },
    });
  };

  const choices: { value: Serves; hint: string }[] = [
    { value: "folder", hint: t`A static site, sent as its files are.` },
    { value: "php", hint: t`Such as WordPress, run by PHP-FPM of its own.` },
    { value: "url", hint: t`Another site, or an address on your network.` },
    { value: "port", hint: t`Something listening on this server, such as port 3000.` },
  ];
  const fieldErrors = fieldError ? { [fieldError.field]: fieldError.said } : {};
  // HTTPS opens on what's in front being refused, and stays open until it's closed by
  // hand, so fixing it doesn't fold it away under the cursor, as Advanced does.
  if (proxiesError !== undefined && !httpsOpen) setHttpsOpen(true);
  const onFields = (next: Settings) => {
    if (next.root !== settings.root) rootTyped.current = true;
    setSettings(next);
    setFieldError(undefined);
  };

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
      initialFocusRef={field}
      title={t`New website`}
      subtitle={
        publishing
          ? t`Static files or PHP, served by Caddy on a server`
          : !noCertificate
            ? t`People reach it at its domain, over HTTPS`
            : inFront === "anyone"
              ? t`People reach it at its domain, over plain HTTP`
              : t`People reach it at its domain, through what's in front of this server`
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
            kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
            onClick={() => void submit()}
            {...mayCreate.off}
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
        {zones.length > 0 ? (
          <DomainName
            field={field}
            sub={domain}
            onSub={(value) => {
              const low = value.trim().toLowerCase();
              const into = zone !== "" ? zones.find((z) => low.endsWith(`.${z.zone}`)) : undefined;
              const sub = into ? low.slice(0, -(into.zone.length + 1)) : value;
              if (into) setZonePicked(into.zone);
              setDomain(sub);
              setDomainError(undefined);
              follow(nameIn(sub, into?.zone ?? zone));
            }}
            zone={zone}
            onZone={(next) => {
              const whole = next === "" && zone !== "" && domain.trim() !== "" ? typed : domain;
              setDomain(whole);
              setZonePicked(next);
              setWayPicked(undefined);
              setDomainError(undefined);
              follow(nameIn(whole, next));
            }}
            zones={zones}
            error={domainError}
          >
            {publishing ? (
              <NameState
                check={cf.check}
                checking={cf.checking}
                domain={typed}
                sub={domain.trim().toLowerCase()}
                zone={zone}
                companion={cf.companion}
                companionOn={alsoCompanion}
                onCompanion={() => setCompanionOpen(true)}
                failed={cf.failed}
                onAgain={cf.again}
                takesOver={takesOver}
                onTakesOver={(items) => setConsent({ domain: typed, items })}
                onSub={(value) => {
                  setDomain(value);
                  setDomainError(undefined);
                  follow(nameIn(value, zone));
                }}
              />
            ) : null}
          </DomainName>
        ) : (
          <TextField
            ref={field}
            label={t`Domain`}
            mono
            autoCapitalize="none"
            autoCorrect="off"
            spellCheck={false}
            placeholder="blog.example.com"
            value={domain}
            error={domainError}
            onChange={(event) => {
              setDomain(event.target.value);
              setDomainError(undefined);
              // The folder follows the domain until it's typed, as /var/www/<domain>.
              const name = event.target.value.trim().toLowerCase();
              if (!rootTyped.current) {
                setSettings((was) => ({ ...was, root: name ? `/var/www/${name}` : "" }));
                setFieldError((said) => (said?.field === "root" ? undefined : said));
              }
            }}
          />
        )}
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
        ) : (
          <DomainRecord
            server={serverOf(fleet, server)}
            domain={typed}
            tunnel={noCertificate}
            tunnelName={switchName}
            onLeave={onClose}
          />
        )}
        {/* In a connected zone, the name's line says its other half, with Change one step away. */}
        {!publishing || companionOpen ? (
          <CompanionChoice
            server={serverOf(fleet, server)}
            read={companion}
            on={alsoCompanion}
            onChange={setAlsoCompanion}
            tunnel={behind || publishes}
            onLeave={onClose}
          />
        ) : null}
        {several ? (
          <SelectField
            label={t`Server`}
            error={serverError}
            value={server}
            options={[
              ...(server === "" ? [{ value: "", label: t`Choose a server` }] : []),
              ...servers.map((s) => ({ value: s.id, label: s.name })),
            ]}
            onChange={(event) => {
              setServer(event.target.value);
              setServerError(undefined);
            }}
          />
        ) : null}
        <fieldset className="app-front website-sends">
          <legend className="dw-field-label">
            <Trans>What it serves</Trans>
          </legend>
          {choices.map((choice) => (
            <label key={choice.value} className={cx("app-front-choice", settings.serves === choice.value && "is-on")}>
              <input
                type="radio"
                name={choiceName}
                checked={settings.serves === choice.value}
                onChange={() => {
                  setSettings({ ...settings, serves: choice.value });
                  setFieldError(undefined);
                  setFailure(undefined);
                }}
              />
              <span className="app-front-text">
                <span className="app-front-name">{servesWords[choice.value]}</span>
                <span className="app-front-hint">{choice.hint}</span>
              </span>
            </label>
          ))}
        </fieldset>
        <SplitWebsiteFields
          settings={settings}
          server={serverOf(fleet, server)?.id ?? ""}
          errors={fieldErrors}
          onChange={onFields}
        />
        {/* HTTPS is on, with its certificate coming by itself: what's in front is one step away. In a
            connected zone, the way in says it, and what's in front is asked only for Something else in front. */}
        {publishing && !behind ? null : (
          <Advanced
            label={t`HTTPS`}
            summary={
              !behind
                ? t`On, with a certificate that comes by itself`
                : inFront === "anyone"
                  ? t`Off: plain HTTP to anyone`
                  : t`Handled in front of this server`
            }
            open={httpsOpen}
            onOpenChange={setHttpsOpen}
          >
            <FrontFields
              behind="website"
              fixed={publishing}
              noCertificate={publishing || noCertificate}
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
