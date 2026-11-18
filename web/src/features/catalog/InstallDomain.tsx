// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import type { RouteSchema, Server } from "@dockwell/api";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useId, useState } from "react";

import { nameIn } from "../apps/AddDomainSheet";
import { DomainRecord } from "../apps/DomainRecord";
import { DomainName, NameState, WayIn } from "../apps/DomainStep";
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
import type { Opening } from "../changes/ChangeSheet";
import "../apps/Apps.css";
import "./Catalog.css";

/** What an install's route says, as InstallCatalogEntryRequest.route takes it. */
export type RouteInit = MessageInitShape<typeof RouteSchema>;

/** What the install's Domain field comes to as the sheet plans it. */
export type DomainAsk =
  /** On the server alone, at its port: no route. */
  | { kind: "none" }
  /** A route to send with the install. */
  | { kind: "route"; route: RouteInit }
  /** Still read: wait for it. */
  | { kind: "reading" }
  /** Something to fix first, which the field says. */
  | { kind: "fix"; why: string };

/**
 * The install's Domain (#792), as #819's add-domain and new-website sheets ask it
 * (ADR 0104): a name under a zone a connected provider writes in, suggested from the
 * app, checked live at Cloudflare, with the way in its check picks and Change one
 * step away; or another domain typed whole, with the record it needs. "Only on the
 * server" leaves it at its port, the default where no zone is connected.
 */
export function useInstallDomain({
  server,
  suggest,
  onPlan,
  enabled,
}: {
  /** The server it goes on, whose tunnels and address the check reads. */
  server: Server | undefined;
  /** The name it starts from, such as the project's. */
  suggest: string;
  /** Opens a plan over the sheet, as setting a zone to Full (strict). */
  onPlan: (opening: Opening) => void;
  /** Whether the app has a web page to send a domain to. */
  enabled: boolean;
}): { section: ReactNode; ask: () => DomainAsk; domain: string; reading: boolean } {
  const { t } = useLingui();
  const id = useId();
  const admin = useInstallAdmin();
  // A new project is in no provider yet: an admin picks from every provider's zones,
  // and the install's change has that provider serve its project (ADR 0104).
  const { zones, providers } = useZones("", admin);
  const [onServer, setOnServer] = useState<boolean>();
  const [sub, setSub] = useState<string>();
  const [zonePicked, setZonePicked] = useState<string>();
  const [error, setError] = useState<string>();
  const zone =
    zones.length === 0
      ? ""
      : zonePicked !== undefined && (zonePicked === "" || zones.some((z) => z.zone === zonePicked))
        ? zonePicked
        : (zones[0]?.zone ?? "");
  // The check runs wherever a domain is wanted, so a server with no way in is known
  // before the field leads with a domain it couldn't take (the review's M3).
  const wanted = enabled && onServer !== true && zones.length > 0 && zone !== "";
  const name = sub ?? (zone !== "" ? suggest : "");
  const typed = nameIn(name, zone);
  const cf = useCloudflareCheck(server?.id ?? "", typed, "", wanted);
  const picked = pickedOf(cf.check);
  const noWay =
    wanted && !cf.checking && !cf.failed && cf.check !== undefined && !cf.check.ways.some((w) => w.off === "");
  // With no way in on the server, and no domain asked for yet, it stays on the server
  // and says why; Add a domain is still one step away.
  const fellBack = noWay && onServer === undefined;
  const whyAlone = fellBack ? wayOf(cf.check, picked.way, picked.tunnel)?.off || cf.check?.ways[0]?.off || "" : "";
  const alone = !enabled || (onServer ?? zones.length === 0) || fellBack;
  const publishing = !alone && zone !== "";
  const [wayPicked, setWayPicked] = useState<{ way: number; tunnel: string }>();
  const way = wayPicked && wayOf(cf.check, wayPicked.way, wayPicked.tunnel)?.off === "" ? wayPicked : picked;
  const wayTakeable = wayOf(cf.check, way.way, way.tunnel)?.off === "";
  const [waysOpen, setWaysOpen] = useState(false);
  const [consent, setConsent] = useState<{ domain: string; items: TakeOverInit[] }>({ domain: "", items: [] });
  const takesOver = consent.domain === typed ? consent.items : [];
  const provider = cf.check?.provider || zones.find((z) => z.zone === zone)?.provider || "";
  const providerInfo = providers.find((p) => p.name === provider && p.provider !== "");

  const toStrict = () => {
    if (!providerInfo) return;
    const p = providerInfo;
    onPlan({
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
    });
  };

  const ask = (): DomainAsk => {
    if (alone) return { kind: "none" };
    if (typed.trim() === "") {
      const why = t`Type the app's domain, or install it on the server alone.`;
      setError(why);
      return { kind: "fix", why };
    }
    if (!publishing) return { kind: "route", route: { domain: typed.trim().toLowerCase() } };
    if (cf.checking) return { kind: "reading" };
    if (cf.failed || !cf.check || !wayTakeable) {
      // Said under the field, so Review plan never stops without a word (the review's M3).
      const why = cf.failed || wayOf(cf.check, way.way, way.tunnel)?.off || t`Choose a way in that the check can take.`;
      setWaysOpen(true);
      setError(why);
      return { kind: "fix", why };
    }
    const publish = publishOf(way.way, way.tunnel, provider, takesOver);
    return {
      kind: "route",
      route: { domain: typed, ...routeOfWay(way.way, cf.check.sslMode ?? ""), ...(publish ? { publish } : {}) },
    };
  };

  const section = !enabled ? null : alone ? (
    <fieldset className="catalog-chosen" aria-labelledby={id}>
      <span id={id} className="catalog-chosen-label">
        <Trans>Domain</Trans>
      </span>
      <span className="catalog-chosen-value">
        <span>
          <Trans>Only on the server, at its port</Trans>
        </span>
        <button type="button" className="catalog-change" onClick={() => setOnServer(false)}>
          <Trans>Add a domain</Trans>
        </button>
        {whyAlone ? <span className="catalog-domain-why">{whyAlone}</span> : null}
      </span>
    </fieldset>
  ) : (
    <div className="catalog-domain">
      <DomainName
        sub={name}
        onSub={(value) => {
          const low = value.trim().toLowerCase();
          const into = zone !== "" ? zones.find((z) => low.endsWith(`.${z.zone}`)) : undefined;
          if (into) setZonePicked(into.zone);
          setSub(into ? low.slice(0, -(into.zone.length + 1)) : value);
          setError(undefined);
        }}
        zone={zone}
        onZone={(next) => {
          const whole = next === "" && zone !== "" && name.trim() !== "" ? typed : name;
          setSub(whole);
          setZonePicked(next);
          setWayPicked(undefined);
          setError(undefined);
        }}
        zones={zones}
        error={error}
      >
        {publishing ? (
          <NameState
            check={cf.check}
            checking={cf.checking}
            domain={typed}
            sub={name.trim().toLowerCase()}
            zone={zone}
            companion=""
            failed={cf.failed}
            onAgain={cf.again}
            takesOver={takesOver}
            onTakesOver={(items) => setConsent({ domain: typed, items })}
            onSub={(value) => {
              setSub(value);
              setError(undefined);
            }}
          />
        ) : null}
      </DomainName>
      {publishing ? (
        <WayIn
          check={cf.check}
          way={way.way}
          tunnel={way.tunnel}
          onWay={(next, tunnel) => setWayPicked({ way: next, tunnel })}
          open={waysOpen}
          onOpen={setWaysOpen}
          server={server?.name ?? ""}
          address={publicAddressOf(server)}
          admin={admin && providerInfo !== undefined}
          onStrict={toStrict}
          onLeave={() => setOnServer(true)}
        />
      ) : typed.trim() !== "" ? (
        <DomainRecord server={server} domain={typed} tunnel={false} tunnelName="" onLeave={() => setOnServer(true)} />
      ) : null}
      <button type="button" className="catalog-change catalog-domain-alone" onClick={() => setOnServer(true)}>
        <Trans>Only on the server instead</Trans>
      </button>
    </div>
  );
  return { section, ask, domain: alone ? "" : typed, reading: publishing && cf.checking };
}
