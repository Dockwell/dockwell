// SPDX-License-Identifier: AGPL-3.0-only

import { ChangeService, type Domain, type DomainPublished, PublishedState } from "@dockwell/api";
import { Trans, useLingui } from "@lingui/react/macro";

import { StateLine } from "../apps/DomainStep";
import { routeAsItIs } from "../apps/PrivateSheet";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";

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

/**
 * Where what Dockwell published for a domain at Cloudflare stands, as the drift timer
 * last read it, where it isn't as written (ADR 0104, section 4): a state each, and Put
 * it back where its routes, proposed as they are, write what's gone again.
 */
export function PublishedNote({ domain }: { domain: Domain }) {
  const { t } = useLingui();
  const word = useStateWord();
  const sheet = useChangeSheet();
  const may = useMay()(ChangeService.method.proposeChange).allowed;
  const off = domain.published.filter(
    (p) => p.state !== PublishedState.PUBLISHED && p.state !== PublishedState.UNSPECIFIED,
  );
  if (off.length === 0) return null;
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
  return (
    <span className="domains-published">
      {off.map((p) => (
        <StateLine
          key={`${p.name}-${p.state}`}
          status={
            p.state === PublishedState.CAN_TAKE_ON || p.state === PublishedState.CAN_MOVE ? "unknown" : "degraded"
          }
          action={
            putsBack(p) && may ? (
              <button
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
            ) : undefined
          }
        >
          {p.name !== name ? `${p.name}: ${word(p.state)}` : word(p.state)}
        </StateLine>
      ))}
    </span>
  );
}
