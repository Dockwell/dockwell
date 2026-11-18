// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { createQueryOptions, useTransport } from "@connectrpc/connect-query";
import {
  type CatalogInstall,
  CatalogService,
  ChangeState,
  type ConnectionInfo,
  ConnectionReach,
  type GetCatalogEntryResponse,
  Kind,
  type Need,
} from "@dockwell/api";
import { Button } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueries } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useMemo } from "react";

import { usePlaces } from "../apps/useServices";
import { useMayLink } from "../catalog/StorageNeed";
import { useInstalls } from "../catalog/useCatalog";
import { useChangeSheet } from "../changes/ChangeSheet";

/** An installed app that wants mail and doesn't send through a connection, with the need it'd meet. */
export interface MailWant {
  install: CatalogInstall;
  need: Need;
  /** Why this connection can't meet it, as the control plane says; "" where it can. */
  why: string;
}

/**
 * The apps installed from the catalog, in the projects a mail connection reaches,
 * whose entry wants a mail server that no connection meets yet: not set, or set by
 * hand (#799). Each says why this connection can't meet it, where the control plane
 * says it can't, as its security.
 */
export function useMailWants(connection: ConnectionInfo): { wants: MailWant[]; pending: boolean } {
  const transport = useTransport();
  const listed = useInstalls();
  const installs = listed.data?.installs ?? [];
  const reaches = (project: string) =>
    connection.reach === ConnectionReach.EVERY || connection.projects.includes(project);
  const open = installs.filter(
    (i) => reaches(i.projectId) && i.pin?.entry && Object.values(i.pin.needs).some((n) => n.connection === ""),
  );
  const ids = [...new Set(open.map((i) => i.pin?.entry ?? ""))].sort();
  const reads = useQueries({
    queries: ids.map((id) => ({
      ...createQueryOptions(CatalogService.method.getCatalogEntry, { id }, { transport }),
      staleTime: 60_000,
    })),
  });
  const key = reads.map((r) => r.dataUpdatedAt).join(",");
  // biome-ignore lint/correctness/useExhaustiveDependencies: the entries change when their reads do, as key says
  const wants = useMemo(() => {
    const entries = new Map<string, GetCatalogEntryResponse>();
    ids.forEach((id, i) => {
      const data = reads[i]?.data;
      if (data) entries.set(id, data);
    });
    const out: MailWant[] = [];
    for (const install of open) {
      const read = entries.get(install.pin?.entry ?? "");
      for (const need of read?.entry?.needs ?? []) {
        if (need.kind !== Kind.SMTP || install.pin?.needs[need.name]?.connection) continue;
        const said = read?.needs.find((o) => o.need === need.name && o.connection === connection.name);
        const serves = connection.uses.length > 0 && !connection.shared;
        out.push({ install, need, why: said?.why || (serves ? "serves" : "") });
      }
    }
    return out.sort((a, b) => a.install.name.localeCompare(b.install.name));
  }, [key, connection, installs]);
  return { wants, pending: listed.isPending || reads.some((r) => r.isPending) };
}

/**
 * A mail connection no app sends through yet: each installed app that wants mail,
 * with Use for it, which plans sending its mail through this connection, one plan
 * each, as its card's Set up does (#799).
 */
export function WantsMail({ connection, onUsed }: { connection: ConnectionInfo; onUsed: () => void }) {
  const { t } = useLingui();
  const { wants, pending } = useMailWants(connection);
  const places = usePlaces();
  const mayLink = useMayLink();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const client = useMemo(() => createClient(CatalogService, transport), [transport]);
  const use = (w: MailWant) =>
    sheet.open({
      kind: "plan",
      make: () =>
        client.setCatalogNeed({ id: w.install.projectId, need: w.need.name, choice: `connection:${connection.name}` }),
      onClose: (after) => {
        if (after && after.state !== ChangeState.PLANNED && after.state !== ChangeState.FAILED) onUsed();
      },
    });
  if (pending) return null;
  if (wants.length === 0) {
    return (
      <div className="connection-unused">
        <span className="connections-meta">
          <Trans>No app sends through it yet, and none installed wants mail.</Trans>
        </span>
      </div>
    );
  }
  return (
    <ul className="connection-wants" aria-label={t`Apps that want mail`}>
      {wants.map((w) => {
        const app = w.install.name;
        const why = w.why === "serves" ? t`It serves another app already, and isn't shared` : w.why;
        return (
          <li key={`${w.install.projectId}/${w.need.name}`} className="connection-want">
            <span className="dw-cell">
              {w.install.webServiceId ? (
                <Link
                  to="/apps/$serviceId"
                  params={{ serviceId: w.install.webServiceId }}
                  className="connection-want-app"
                >
                  {app}
                </Link>
              ) : (
                <span>{app}</span>
              )}
              <span className="connections-meta">
                {[
                  places.label(w.install.environmentId),
                  w.install.pin?.needs[w.need.name]?.own ? t`mail set by hand` : t`no mail yet`,
                ].join(" · ")}
              </span>
            </span>
            {why ? <span className="connections-meta connection-want-why">{why}</span> : null}
            <Button size="sm" variant="secondary" onClick={() => use(w)} {...(why ? { disabled: true } : mayLink.off)}>
              <Trans>Use for {app}</Trans>
            </Button>
          </li>
        );
      })}
    </ul>
  );
}
