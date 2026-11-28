// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ChangeState, type ConnectionInfo, ConnectionService, LinkKind, type ServiceInfo } from "@dockwell/api";
import { Button, Callout, Loading, SelectField, Sheet, SkeletonField, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useId, useMemo, useState } from "react";

import { sentence } from "../../lib/errors";
import { appOf, environmentOf, nameOf } from "../apps/app";
import { usePlaces } from "../apps/useServices";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useConnectableApps } from "./ConnectStorageSheet";
import { CustomConnectionSheet, useCustomTypeLabels } from "./CustomConnectionSheet";
import { useConnections } from "./useConnections";

export function ConnectCustomSheet({
  service: fixedService,
  connection: fixedConnection,
  onClose,
}: {
  service?: ServiceInfo;
  connection?: ConnectionInfo;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const form = useId();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const client = useMemo(() => createClient(ConnectionService, transport), [transport]);
  const types = useCustomTypeLabels();
  const may = useMay();
  const mayCreate = may(ConnectionService.method.createCustomConnection).allowed;
  const [creating, setCreating] = useState(false);
  const places = usePlaces();
  const apps = useConnectableApps(fixedConnection).filter(
    (a) => !fixedConnection?.uses.some((u) => u.appIds.includes(a.id)),
  );
  const [pickedApp, setPickedApp] = useState("");
  const service = fixedService ?? apps.find((a) => a.id === pickedApp) ?? apps[0];
  const environment = service ? environmentOf(service) : "";
  const project = places.environments.find((e) => e.id === environment)?.scope ?? "";
  const query = useConnections(environment);
  const all = useConnections();
  const candidates = (query.data?.connections ?? []).filter(
    (c) =>
      c.kind === LinkKind.CUSTOM &&
      (c.shared || (!c.custom?.inUse && c.uses.length === 0)) &&
      !c.uses.some((u) => u.appIds.includes(service?.id ?? "")),
  );
  const [picked, setPicked] = useState("");
  const connection = fixedConnection ?? candidates.find((c) => c.name === picked) ?? candidates[0];
  const [aliases, setAliases] = useState<Record<string, string>>({});
  const fields = connection?.custom?.fields ?? [];
  const mappings = fields.map((f) => ({ field: f, alias: aliases[f.name] ?? f.defaultEnv }));
  const occupied = new Set([
    ...Object.keys(service?.spec?.variables ?? {}),
    ...Object.keys(service?.spec?.secrets ?? {}),
    ...Object.keys(service ? (appOf(service)?.env ?? {}) : {}),
    ...(service?.spec?.runtime ? ["PORT"] : []),
  ]);
  const duplicate = new Set(mappings.map((m) => m.alias)).size !== mappings.length;
  const conflict = mappings.some((m) => occupied.has(m.alias));
  const canReview =
    service &&
    project &&
    connection &&
    mappings.length > 0 &&
    !duplicate &&
    !conflict &&
    mappings.every((m) => /^[A-Za-z_][A-Za-z0-9_]{0,254}$/.test(m.alias));

  function review() {
    if (!canReview || !service || !connection) return;
    sheet.open({
      kind: "plan",
      make: () =>
        client.connectCustom({
          project,
          environment,
          app: nameOf(service),
          connection: connection.name,
          fields: Object.fromEntries(mappings.map((m) => [m.field.name, m.alias])),
        }),
      onClose: (change) => {
        if (change?.state !== ChangeState.APPLIED && change?.state !== ChangeState.APPLYING) return;
        void Promise.all([query.refetch(), all.refetch()]).then(onClose);
      },
    });
  }

  if (creating) {
    return (
      <CustomConnectionSheet
        existing={all.data?.connections ?? []}
        project={project}
        onClose={() => setCreating(false)}
        onSaved={(name) => {
          setPicked(name);
          setAliases({});
          void query.refetch();
        }}
      />
    );
  }

  return (
    <Sheet
      open
      title={t`Add from a connection`}
      onClose={onClose}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button type="submit" form={form} disabled={!canReview}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <form
        id={form}
        className="connection-sheet"
        onSubmit={(event) => {
          event.preventDefault();
          review();
        }}
      >
        {fixedService ? null : (
          <SelectField
            label={t`App`}
            value={service?.id ?? ""}
            options={apps.map((a) => ({
              value: a.id,
              label: `${nameOf(a)} · ${places.label(environmentOf(a))}`,
            }))}
            onChange={(e) => {
              setPickedApp(e.target.value);
              setAliases({});
            }}
          />
        )}
        {fixedConnection ? (
          <p className="connections-mono">{fixedConnection.displayName || fixedConnection.name}</p>
        ) : query.isPending ? (
          <Loading label={t`Loading connections`}>
            <SkeletonField />
          </Loading>
        ) : query.error ? (
          <Callout title={t`Dockwell can't read its connections`} tone="critical">
            {sentence(query.error)}
          </Callout>
        ) : candidates.length === 0 ? (
          <Callout
            title={t`No Custom connections available`}
            actions={
              mayCreate && project ? (
                <Button type="button" variant="secondary" size="sm" icon="plus" onClick={() => setCreating(true)}>
                  <Trans>Add a Custom connection</Trans>
                </Button>
              ) : undefined
            }
          >
            {mayCreate ? null : <Trans>An admin adds the connections your apps may use.</Trans>}
          </Callout>
        ) : (
          <SelectField
            label={t({ message: "Connection", context: "choose a saved connection" })}
            value={connection?.name ?? ""}
            options={candidates.map((c) => ({
              value: c.name,
              label: c.displayName || c.name,
            }))}
            onChange={(e) => {
              setPicked(e.target.value);
              setAliases({});
            }}
          />
        )}
        {mappings.length > 0 ? (
          <fieldset className="custom-connection-fields">
            <legend>
              <Trans>Environment variables</Trans>
            </legend>
            {mappings.map(({ field, alias }) => (
              <TextField
                key={field.name}
                label={field.name}
                mono
                value={alias}
                hint={types[field.type]}
                invalid={occupied.has(alias)}
                onChange={(e) => setAliases((was) => ({ ...was, [field.name]: e.target.value }))}
              />
            ))}
          </fieldset>
        ) : null}
        {duplicate || conflict ? (
          <Callout tone="warning" title={t`Use a different name for each environment variable`} />
        ) : null}
      </form>
    </Sheet>
  );
}
