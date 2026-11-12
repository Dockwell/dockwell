// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { type Change, ChangeState, type ServiceInfo, ServiceService } from "@dockwell/api";
import { Button, Panel } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type ReactNode, useEffect, useState } from "react";

import { type Offer, useMay } from "../auth/may";
import { installIn, useInstalls } from "../catalog/useCatalog";
import { type Opening, type PlanChoice, useChangeSheet } from "../changes/ChangeSheet";
import { followChange } from "../changes/follow";
import { UpgradeLine, UpgradePhases } from "../changes/UpgradeLead";
import { movesDatabase, upgradeOf, upgradePhases, upgradeTo } from "../changes/upgrade";
import { environmentOf } from "./app";
import { useServices } from "./useServices";
import "../changes/Upgrade.css";

/** The volumes of a service it writes to, which an update backs up first (ADR 0080). */
export function writtenVolumes(service: ServiceInfo): string[] {
  const app = service.spec?.runs.case === "app" ? service.spec.runs.value : undefined;
  return (app?.mounts ?? []).flatMap((m) =>
    m.source.case === "volume" && !m.readOnly && !m.external ? [m.source.value] : [],
  );
}

export interface Upgrade {
  /** Opens the plan of the update, to image where it's given, or to the newer version Dockwell found. */
  open: (image?: string) => void;
  /** Whether the person may plan it (ADR 0059). */
  may: Offer;
}

/**
 * Updating an app, in one click: the plan sheet opens on the update's plan, with
 * its backup first, switched on, where it has data to back up (ADR 0080). A
 * service of a catalog install updates its install, whose services' volumes are
 * what it backs up.
 */
export function useUpgrade(service: ServiceInfo): Upgrade {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const may = useMay();
  const services = useServices().data ?? [];
  const environment = environmentOf(service);
  const install = installIn(useInstalls().data?.installs ?? [], environment);
  const kept = install
    ? services.filter((s) => environmentOf(s) === environment).flatMap(writtenVolumes)
    : writtenVolumes(service);
  const names = new Intl.ListFormat(i18n.locale, { type: "conjunction" }).format([...new Set(kept)]);
  const opening = (image: string | undefined, backup: boolean): Opening => {
    const choice: PlanChoice = {
      label: t`Back up first`,
      on: backup,
      description: backup
        ? t`Backs up ${names} before anything changes`
        : t`Undo won't bring back what it changes in ${names}`,
      flip: () => opening(image, !backup),
      moot: movesDatabase,
    };
    return {
      kind: "plan",
      make: () =>
        createClient(ServiceService, transport).upgradeService({
          id: service.id,
          image: image ?? "",
          skipBackup: !backup,
        }),
      ...(kept.length > 0 ? { choice } : {}),
    };
  };
  return {
    open: (image) => sheet.open(opening(image, true)),
    may: may(ServiceService.method.upgradeService, { environment }),
  };
}

/** How long an update's end stays said on the app's page once it's over. */
const SAID_FOR_MS = 15 * 60_000;

/**
 * An update of this app as it goes, on its page: the phases with the pull's bar
 * while it applies, then that it's updated, with Undo, for a while after (ADR
 * 0080). A failure is the page's notice of a rollback, as any deploy's is. Where
 * the app's latest change isn't an update, or isn't read yet, it's fallback.
 * Elsewhere says another UpgradeProgress on the page shows the update, in the
 * place of what's newer (ADR 0079): this one then says nothing while it would.
 */
export function UpgradeProgress({
  service,
  fallback = null,
  elsewhere = false,
}: {
  service: ServiceInfo;
  fallback?: ReactNode;
  elsewhere?: boolean;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const [change, setChange] = useState<Change>();
  // Only a change that's applying, or landed lately, has anything to say here, so no other is read.
  const lately =
    service.changeState === ChangeState.APPLYING ||
    (service.updateTime !== undefined && Date.now() - timestampMs(service.updateTime) < SAID_FOR_MS);
  const id = service.rolledBackChangeId || !lately ? "" : service.changeId;
  useEffect(() => {
    setChange(undefined);
    if (!id) return;
    const abort = new AbortController();
    void followChange(transport, id, abort.signal, setChange);
    return () => abort.abort();
  }, [transport, id]);
  const intent = change?.id === id ? upgradeOf(change?.plan) : undefined;
  if (!change || !intent) return fallback;
  const to = upgradeTo(intent);
  if (change.state === ChangeState.APPLYING) {
    if (elsewhere) return null;
    return (
      <Panel title={t`Updating to ${to}`} className="upgrade-card">
        <UpgradeLine intent={intent} />
        <UpgradePhases phases={upgradePhases(change.plan, change)} />
      </Panel>
    );
  }
  // When it landed: its last step's end, or when it was applied.
  const ended = Math.max(
    change.applyTime ? timestampMs(change.applyTime) : 0,
    ...change.steps.map((s) => (s.finishTime ? timestampMs(s.finishTime) : 0)),
  );
  if (change.state !== ChangeState.APPLIED || change.undoneBy || Date.now() - ended > SAID_FOR_MS) return fallback;
  if (elsewhere) return null;
  return (
    <Panel
      title={t`Updated to ${to}`}
      className="upgrade-card"
      actions={
        change.undoDeadline ? (
          <Button variant="ghost" size="sm" icon="undo" onClick={() => sheet.open({ kind: "undo", id: change.id })}>
            <Trans>Undo</Trans>
          </Button>
        ) : undefined
      }
    >
      <UpgradeLine intent={intent} />
    </Panel>
  );
}
