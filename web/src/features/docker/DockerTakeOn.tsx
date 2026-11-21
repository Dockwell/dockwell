// SPDX-License-Identifier: AGPL-3.0-only

import type { DockerObject } from "@dockwell/api";
import { Button, Icon, SelectField, Sheet, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { usePlaces } from "../apps/useServices";
import { serviceNameFor } from "../servers/containers";
import { useTakeOn } from "../servers/takeOn";
import { containerOf } from "./parts";

export function DockerTakeOn({
  row,
  onClose,
  onApplied,
}: {
  row: DockerObject;
  onClose: () => void;
  onApplied?: () => void;
}) {
  const { t } = useLingui();
  const places = usePlaces();
  const { take, environmentFor } = useTakeOn(row.key?.serverId ?? "", onApplied);
  const details = containerOf(row);
  const container = {
    engineId: row.key?.engineId ?? "",
    name: row.name,
    composeProject: details?.composeProject ?? "",
    composeService: details?.composeService ?? "",
  };
  const [name, setName] = useState(() => serviceNameFor(container));
  const [environment, setEnvironment] = useState("");
  const chosen = environment || environmentFor(container);
  const ready = !places.isPending && !!container.engineId && !!name.trim();

  return (
    <Sheet
      open
      className="docker-take-on-sheet"
      title={t`Take on ${row.name}`}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button
            variant="primary"
            disabled={!ready}
            onClick={() => {
              onClose();
              take(container, name.trim(), chosen);
            }}
          >
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="docker-take-on">
        <p className="docker-take-on-kept">
          <Icon name="check" size={16} />
          <Trans>Keeps the current container, mounts, network connections and environment.</Trans>
        </p>
        <TextField label={t`App name`} value={name} onChange={(event) => setName(event.target.value)} />
        {places.environments.length > 1 ? (
          <SelectField
            label={places.showsEnvironment ? t`Environment` : t`Project`}
            value={chosen}
            options={places.environments.map((place) => ({ value: place.id, label: place.label }))}
            onChange={(event) => setEnvironment(event.target.value)}
          />
        ) : null}
      </div>
    </Sheet>
  );
}
