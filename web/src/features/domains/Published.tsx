// SPDX-License-Identifier: AGPL-3.0-only

import { ChangeService, type Domain, type DomainPublished, PublishedState, RouteWay } from "@dockwell/api";
import { StatusBadge } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { createContext, type ReactNode, useContext } from "react";

import { StateLine } from "../apps/DomainStep";
import { routeAsItIs } from "../apps/PrivateSheet";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";

/**
 * Opens Add a domain on a name, for an admin, where taking on or moving what was made
 * in Cloudflare happens with the live check (the review's B-M4). Unset where the
 * caller can't, or the name reaches no app.
 */
export const ManageNameContext = createContext<((domain: Domain) => void) | undefined>(undefined);

/** The zones the connected providers write in, to say which zone's rule publishes a name. */
export const ZonesContext = createContext<readonly string[]>([]);

/** What a name Dockwell published at Cloudflare has come to, in a few words. */
function useStateWord(): (state: PublishedState) => string {
  const { t } = useLingui();
  return (state) => {
    switch (state) {
      case PublishedState.GONE_FROM_TUNNEL:
        return t`Not in the tunnel any more`;
      case PublishedState.RECORD_GONE:
        return t`Its DNS record is gone at Cloudflare`;
      case PublishedState.CHANGED:
        return t`Changed in Cloudflare, so removing it leaves it`;
      case PublishedState.TUNNEL_SHARED:
        return t`Its tunnel runs on another server too`;
      case PublishedState.CAN_TAKE_ON:
        return t`Made in Cloudflare, pointing here`;
      case PublishedState.CAN_MOVE:
        return t`Its tunnel's rule points at the app's own port`;
      case PublishedState.TUNNEL_DELETED:
        return t`Its tunnel was deleted in Cloudflare`;
      case PublishedState.UNREADABLE:
        return t`Dockwell can't read it at Cloudflare`;
      case PublishedState.TUNNEL_UNPROVEN:
        return t`Dockwell can't prove its tunnel runs on this server`;
    }
    return "";
  };
}

/** Whether proposing the domain's routes as they are writes what's gone again. */
function putsBack(p: DomainPublished): boolean {
  return p.state === PublishedState.GONE_FROM_TUNNEL || p.state === PublishedState.RECORD_GONE;
}

/** How a published name is published, in a word or two: "proxied", or the zone's rule. */
function useHowWords(): (p: DomainPublished, zones: readonly string[]) => string {
  const { t } = useLingui();
  return (p, zones) => {
    switch (p.way) {
      case RouteWay.SERVER_TUNNEL: {
        // The zone's own rule in the tunnel publishes it, as Dockwell writes one per zone.
        const zone = [...zones].sort((a, b) => b.length - a.length).find((z) => p.name.endsWith(`.${z}`));
        const tunnel = p.tunnel;
        return zone ? t`by *.${zone}` : tunnel ? t`through ${tunnel}` : "";
      }
      case RouteWay.OWN_TUNNEL:
        return t`through Dockwell's tunnel`;
      case RouteWay.PROXY:
        return t`proxied`;
    }
    return "";
  };
}

/**
 * Where what Dockwell published for a domain at Cloudflare stands, as the drift timer
 * last read it (ADR 0104, section 4, and #820), and the action each state has: Put it
 * back where its routes, proposed as they are, write what's gone again; Manage in, or
 * Move it to, Dockwell where it was made in Cloudflare; the state alone where nothing
 * here fixes it. A domain published by hand has none.
 */
function usePublished(domain: Domain): {
  off: { p: DomainPublished; words: string; status: "degraded" | "unknown"; action: ReactNode }[];
  published: DomainPublished | undefined;
} {
  const manage = useContext(ManageNameContext);
  const onManage = manage ? () => manage(domain) : undefined;
  const { t } = useLingui();
  const word = useStateWord();
  const sheet = useChangeSheet();
  const may = useMay()(ChangeService.method.proposeChange).allowed;
  const name = domain.name;
  const putBack = () =>
    sheet.open({
      kind: "propose",
      summary: t`Put ${name} back at Cloudflare`,
      edits: domain.routes.map((r) => ({
        resource: { kind: "route", scope: domain.environmentId, name: r.name },
        spec: { kind: { case: "route" as const, value: routeAsItIs(domain.name, r) } },
        ...(domain.servers.length === 1 ? { servers: [domain.servers[0]?.serverId ?? ""] } : {}),
      })),
    });
  const off = domain.published
    .filter((p) => p.state !== PublishedState.PUBLISHED && p.state !== PublishedState.UNSPECIFIED)
    .map((p) => {
      const madeThere = p.state === PublishedState.CAN_TAKE_ON || p.state === PublishedState.CAN_MOVE;
      const action =
        madeThere && onManage ? (
          // Taken on, or moved, where the live check is, with an admin's consent (the review's B-M4).
          <button
            key="manage"
            type="button"
            className="domain-step-act"
            onClick={(event) => {
              event.stopPropagation();
              onManage();
            }}
          >
            {p.state === PublishedState.CAN_MOVE ? (
              <Trans>Move it to Dockwell</Trans>
            ) : (
              <Trans>Manage in Dockwell</Trans>
            )}
          </button>
        ) : putsBack(p) && may ? (
          <button
            key="put-back"
            type="button"
            className="domain-step-act"
            aria-label={t`Put ${name} back at Cloudflare`}
            onClick={(event) => {
              event.stopPropagation();
              putBack();
            }}
          >
            <Trans>Put it back</Trans>
          </button>
        ) : null;
      return {
        p,
        words: p.name !== name ? `${p.name}: ${word(p.state)}` : word(p.state),
        status: madeThere ? ("unknown" as const) : ("degraded" as const),
        action,
      };
    });
  const published =
    domain.published.find((p) => p.name === name && p.state === PublishedState.PUBLISHED) ??
    domain.published.find((p) => p.state === PublishedState.PUBLISHED);
  return { off, published };
}

/**
 * A domain's At Cloudflare cell on the list, as the States mock's All domains draws it:
 * Published, with how, or each state that isn't, with its action under it.
 */
export function PublishedCell({ domain }: { domain: Domain }) {
  const { t } = useLingui();
  const how = useHowWords();
  const zones = useContext(ZonesContext);
  const { off, published } = usePublished(domain);
  if (off.length > 0) {
    return (
      <div className="dw-cell domains-at-cloudflare">
        {off.map(({ p, words, status, action }) => (
          <span key={`${p.name}-${p.state}`} className="domains-at-cloudflare-state">
            <StatusBadge status={status} label={words} />
            {action ? <span className="domains-at-cloudflare-act">{action}</span> : null}
          </span>
        ))}
      </div>
    );
  }
  if (!published) return null;
  const meta = how(published, zones);
  return (
    <div className="dw-cell domains-at-cloudflare">
      <span className="domains-at-cloudflare-state">
        <StatusBadge status="healthy" label={t`Published`} meta={meta || undefined} />
      </span>
    </div>
  );
}

/**
 * The same on a phone, as lines in the row's card: each state that isn't published
 * with its action, or Published, with how, in one quiet line.
 */
export function PublishedNote({ domain }: { domain: Domain }) {
  const { t } = useLingui();
  const how = useHowWords();
  const zones = useContext(ZonesContext);
  const { off, published } = usePublished(domain);
  if (off.length === 0 && !published) return null;
  const way = published ? how(published, zones) : "";
  return (
    <span className="domains-published">
      {off.length > 0 ? (
        off.map(({ p, words, status, action }) => (
          <StateLine key={`${p.name}-${p.state}`} status={status} action={action ?? undefined}>
            {words}
          </StateLine>
        ))
      ) : published ? (
        <StateLine status="healthy">{way ? t`Published at Cloudflare, ${way}` : t`Published at Cloudflare`}</StateLine>
      ) : null}
    </span>
  );
}
