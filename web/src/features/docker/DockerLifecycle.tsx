// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeState,
  ContainerState,
  DockerContainerAction,
  type DockerObject,
  DockerObjectKind,
  DockerOwnership,
  DockerService,
} from "@dockwell/api";
import { Button, SelectField, Sheet, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";
import lifecycleSignals from "../../../../internal/docker/lifecycle-signals.json";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import type { MenuAction } from "../shell/ActionMenu";
import { combineBatchActions, useBatchAction } from "../shell/ListTools";

export function useLifecycleVerbs() {
  const { t } = useLingui();
  return [
    {
      action: DockerContainerAction.START,
      label: t`Start`,
      icon: "play",
      unavailable: t`Already running or unavailable`,
    },
    { action: DockerContainerAction.STOP, label: t`Stop`, icon: "stop", unavailable: t`Not running` },
    {
      action: DockerContainerAction.RESTART,
      label: t`Restart`,
      icon: "refresh",
      unavailable: t`Not running or paused`,
    },
    {
      action: DockerContainerAction.PAUSE,
      label: t`Pause`,
      icon: "pause",
      unavailable: t`Not running or already paused`,
    },
    { action: DockerContainerAction.UNPAUSE, label: t`Unpause`, icon: "play", unavailable: t`Not paused` },
    { action: DockerContainerAction.KILL, label: t`Kill`, icon: "stop", danger: true, unavailable: t`Not running` },
    {
      action: DockerContainerAction.REMOVE,
      label: t`Remove`,
      icon: "trash",
      danger: true,
      unavailable: t`Stop the container first`,
    },
    { action: DockerContainerAction.RENAME, label: t`Rename`, icon: "edit", unavailable: t`Being removed` },
  ] as const;
}

function accepts(row: DockerObject, action: DockerContainerAction) {
  if (!row.key || row.ownership !== DockerOwnership.EXTERNAL) return false;
  if (row.key.kind === DockerObjectKind.COMPOSE) return true;
  const running = row.state === ContainerState.RUNNING || row.state === ContainerState.RESTARTING;
  const stopped = row.state === ContainerState.CREATED || row.state === ContainerState.EXITED;
  switch (action) {
    case DockerContainerAction.START:
      return stopped;
    case DockerContainerAction.STOP:
    case DockerContainerAction.KILL:
      return running || row.state === ContainerState.PAUSED;
    case DockerContainerAction.RESTART:
      return running;
    case DockerContainerAction.PAUSE:
      return row.state === ContainerState.RUNNING;
    case DockerContainerAction.UNPAUSE:
      return row.state === ContainerState.PAUSED;
    case DockerContainerAction.REMOVE:
      return stopped || row.state === ContainerState.DEAD;
    case DockerContainerAction.RENAME:
      return row.state !== ContainerState.REMOVING;
    default:
      return false;
  }
}

type Pending = {
  rows: readonly DockerObject[];
  action: DockerContainerAction;
  label: string;
  onApplied?: (() => void) | undefined;
};
type Options = { newName?: string; signal?: string; timeoutSeconds?: number; removeAnonymousVolumes?: boolean };

export function useDockerLifecycle() {
  const { t } = useLingui();
  const permission = useMay()(DockerService.method.planDockerMaintenance);
  const transport = useTransport();
  const change = useChangeSheet();
  const verbs = useLifecycleVerbs();
  const batchAction = useBatchAction<DockerObject>();
  const [pending, setPending] = useState<Pending>();

  const review = ({ action, rows, onApplied }: Pending, options: Options = {}) => {
    setPending(undefined);
    change.open({
      kind: "plan",
      make: () =>
        createClient(DockerService, transport).planDockerMaintenance({
          operation: {
            case: "lifecycle",
            value: { keys: rows.flatMap((row) => (row.key ? [row.key] : [])), action, ...options },
          },
        }),
      onClose: (result) => {
        if (result?.state === ChangeState.APPLIED || result?.state === ChangeState.FAILED) onApplied?.();
      },
    });
  };
  const select = (request: Pending) => {
    if (
      [DockerContainerAction.KILL, DockerContainerAction.REMOVE, DockerContainerAction.RENAME].includes(request.action)
    ) {
      setPending(request);
    } else review(request);
  };
  const actionsFor = (row: DockerObject, onApplied?: () => void): MenuAction[] =>
    verbs.flatMap((verb) =>
      accepts(row, verb.action)
        ? [
            {
              id: `external-${verb.action}`,
              label: verb.label,
              icon: verb.icon,
              ...("danger" in verb ? { danger: true } : {}),
              ...permission.off,
              onSelect: () => select({ action: verb.action, label: verb.label, rows: [row], onApplied }),
            },
          ]
        : [],
    );
  const batch = (rows: readonly DockerObject[], onApplied: () => void) =>
    combineBatchActions(
      ...verbs.map((verb) =>
        batchAction(
          rows,
          (row) => {
            if (row.ownership !== DockerOwnership.EXTERNAL) return { reason: t`Managed or system container` };
            if (!accepts(row, verb.action)) return { reason: verb.unavailable };
            return permission.allowed ? undefined : { permission };
          },
          {
            id: `external-${verb.action}`,
            label: (count) => (count ? t`${verb.label} ${count}` : verb.label),
            icon: verb.icon,
            ...("danger" in verb ? { danger: true } : {}),
            onSelect: (targets) => select({ action: verb.action, label: verb.label, rows: targets, onApplied }),
          },
        ),
      ),
    );
  return {
    actionsFor,
    batch,
    editor: pending ? (
      <LifecycleOptions
        request={pending}
        review={(options) => review(pending, options)}
        onClose={() => setPending(undefined)}
      />
    ) : null,
  };
}

const customSignals = [
  ...lifecycleSignals.names,
  ...Array.from({ length: lifecycleSignals.maxNumber }, (_, index) => String(index + 1)),
].map((signal) => ({ value: signal, label: signal }));

function LifecycleOptions({
  request,
  review,
  onClose,
}: {
  request: Pending;
  review: (options: Options) => void;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const [value, setValue] = useState("");
  const [timeout, setTimeout] = useState("10");
  const [volumes, setVolumes] = useState(false);
  const rename = request.action === DockerContainerAction.RENAME;
  const kill = request.action === DockerContainerAction.KILL;
  const prefix = request.rows.length > 1 || request.rows[0]?.key?.kind === DockerObjectKind.COMPOSE;
  const valid = rename
    ? /^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$/.test(value)
    : !kill || value !== "" || (/^\d+$/.test(timeout) && Number(timeout) >= 1 && Number(timeout) <= 60);
  const submit = () =>
    review(
      rename
        ? { newName: value }
        : kill
          ? value
            ? { signal: value }
            : { timeoutSeconds: Number(timeout) }
          : { removeAnonymousVolumes: volumes },
    );
  return (
    <Sheet
      open
      title={request.label}
      subtitle={request.rows.length === 1 ? request.rows[0]?.name : undefined}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" disabled={!valid} onClick={submit}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="docker-image-fields">
        {rename ? (
          <TextField
            mono
            label={prefix ? t`Container name prefix` : t`New container name`}
            value={value}
            maxLength={255}
            onChange={(e) => setValue(e.target.value)}
          />
        ) : kill ? (
          <>
            <SelectField
              mono
              label={t`Signal`}
              value={value}
              options={[{ value: "", label: t`SIGTERM, then SIGKILL` }, ...customSignals]}
              onChange={(e) => setValue(e.target.value)}
            />
            {!value ? (
              <TextField
                mono
                label={t`Seconds before SIGKILL`}
                inputMode="numeric"
                value={timeout}
                maxLength={2}
                onChange={(e) => setTimeout(e.target.value)}
              />
            ) : null}
          </>
        ) : (
          <SelectField
            label={t`Anonymous volumes`}
            value={volumes ? "remove" : "keep"}
            options={[
              { value: "keep", label: t`Keep anonymous volumes` },
              { value: "remove", label: t`Remove container and its anonymous volumes` },
            ]}
            onChange={(e) => setVolumes(e.target.value === "remove")}
          />
        )}
      </div>
    </Sheet>
  );
}
