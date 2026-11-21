// SPDX-License-Identifier: AGPL-3.0-only

import { type DockerObject, DockerReadState, type DockerUnavailable, type Server } from "@dockwell/api";
import { Button, Callout } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";
import "./DiskMeasurements.css";

export function MeasuringSize() {
  const { t } = useLingui();
  return <span className="docker-pending-size" role="img" aria-label={t`Measuring`} />;
}

export function Fact({ label, value, mono }: { label: string; value: ReactNode; mono?: boolean }) {
  return (
    <div className="docker-fact">
      <dt>{label}</dt>
      <dd>{mono ? <code>{value}</code> : value}</dd>
    </div>
  );
}

export function Unread({
  values,
  servers,
  retry,
}: {
  values: readonly DockerUnavailable[];
  servers: readonly Pick<Server, "id" | "name">[];
  retry: () => void;
}) {
  const { t } = useLingui();
  if (values.length === 0) return null;
  const why = (state: DockerReadState) => {
    switch (state) {
      case DockerReadState.AGENT_OUTDATED:
        return t`Agent update needed`;
      case DockerReadState.OFFLINE:
        return t`Agent disconnected`;
      default:
        return t`Docker isn't answering`;
    }
  };
  return (
    <Callout
      tone="warning"
      title={values.length === 1 ? why(values[0]?.state ?? DockerReadState.UNSPECIFIED) : t`Some servers didn't answer`}
      actions={
        <Button size="sm" variant="secondary" onClick={retry}>
          <Trans>Try again</Trans>
        </Button>
      }
    >
      <span className="docker-unread">
        {values.map((row) => (
          <span key={row.serverId}>
            <Link className="dw-link" to="/servers/$serverId" params={{ serverId: row.serverId }}>
              {servers.find((server) => server.id === row.serverId)?.name ?? row.serverId}
            </Link>
            {values.length > 1 ? <> · {why(row.state)}</> : null}
          </span>
        ))}
      </span>
    </Callout>
  );
}

export function containerOf(row: DockerObject | undefined) {
  return row?.detail.case === "container" ? row.detail.value : undefined;
}

export function LiveUpdates({ paused, retry }: { paused: boolean; retry: () => void }) {
  if (!paused) return null;
  return (
    <div className="docker-live-note" role="status">
      <span>
        <Trans>Live updates paused</Trans>
      </span>
      <Button size="sm" variant="secondary" onClick={retry}>
        <Trans>Retry</Trans>
      </Button>
    </div>
  );
}
