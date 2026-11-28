// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  ChangeState,
  type ConnectionCustomCheck,
  ConnectionCustomCheckSchema,
  ConnectionCustomSchema,
  ConnectionCustomType,
  type ConnectionInfo,
  ConnectionReach,
  ConnectionService,
  LinkKind,
} from "@dockwell/api";
import { Advanced, Button, Callout, SelectField, Sheet, Switch, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useId, useMemo, useRef, useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useAllResources } from "../projects/useProjects";
import { useFleetView } from "../servers/useFleet";
import { freeConnectionName } from "./providers";
import { checkServer, useCheckWords, useConnections } from "./useConnections";

const fieldType = ConnectionCustomType;
type Field = {
  id: number;
  name: string;
  type: ConnectionCustomType;
  value: string;
  savedName?: string;
  set: boolean;
  clear?: boolean;
};

function renamedSecret(field: Field): boolean {
  return field.type === fieldType.SECRET && field.set && field.name.trim() !== field.savedName;
}

const presets = {
  empty: [{ name: "", type: fieldType.TEXT }],
  api: [{ name: "api_key", type: fieldType.SECRET }],
  database: [{ name: "database_url", type: fieldType.SECRET }],
  mail: [
    { name: "host", type: fieldType.TEXT },
    { name: "port", type: fieldType.PORT },
    { name: "user", type: fieldType.TEXT },
    { name: "password", type: fieldType.SECRET },
  ],
};

export function useCustomTypeLabels() {
  const { t } = useLingui();
  return {
    [fieldType.UNSPECIFIED]: t`Field type`,
    [fieldType.TEXT]: t`Text`,
    [fieldType.SECRET]: t`Secret`,
    [fieldType.URL]: t`URL`,
    [fieldType.PORT]: t`Port`,
    [fieldType.NUMBER]: t`Number`,
  };
}

export function CustomConnectionSheet({
  editing,
  existing,
  onClose,
  onSaved,
  project,
}: {
  editing?: ConnectionInfo | undefined;
  existing: readonly ConnectionInfo[];
  onClose: () => void;
  onSaved?: (name: string) => void;
  project?: string | undefined;
}) {
  const { t } = useLingui();
  const form = useId();
  const types = useCustomTypeLabels();
  const query = useConnections();
  const sheet = useChangeSheet();
  const transport = useTransport();
  const client = useMemo(() => createClient(ConnectionService, transport), [transport]);
  const may = useMay();
  const maySave = editing
    ? may(ChangeService.method.proposeChange, { install: true })
    : may(ConnectionService.method.createCustomConnection);
  const mayCheck = may(ConnectionService.method.checkConnection).allowed;
  const words = useCheckWords();
  const server = checkServer(useFleetView().servers ?? []);
  const resources = useAllResources();
  const projects = (resources.data ?? []).filter((r) => r.resource?.kind === "project");
  const [label, setLabel] = useState(editing?.displayName || editing?.name || "");
  const [initial, setInitial] = useState(editing?.custom?.initial ?? "");
  const [template, setTemplate] = useState<keyof typeof presets>("empty");
  const [fields, setFields] = useState<Field[]>(
    () =>
      editing?.custom?.fields.map((f, id) => ({
        id,
        name: f.name,
        savedName: f.name,
        type: f.type,
        value: f.value,
        set: f.set,
      })) ?? [{ id: 0, name: "", type: fieldType.TEXT, value: "", set: false }],
  );
  const nextID = useRef(fields.length);
  const [reach, setReach] = useState(editing?.reach ?? (project ? ConnectionReach.PROJECTS : ConnectionReach.EVERY));
  const [picked, setPicked] = useState(editing?.projects ?? (project ? [project] : []));
  const [shared, setShared] = useState(editing?.shared ?? false);
  const target = editing?.custom?.check?.target;
  const [checkType, setCheckType] = useState<string>(target?.case ?? "");
  const [url, setUrl] = useState<string>(target?.case === "httpsUrl" ? target.value : "");
  const [host, setHost] = useState<string>(target?.case === "tcp" ? target.value.host : "");
  const [port, setPort] = useState<string>(target?.case === "tcp" ? String(target.value.port) : "");
  const [checking, setChecking] = useState(false);
  const [answer, setAnswer] = useState<{ key: string; line: string; passed: boolean }>();

  const name =
    editing?.name ??
    freeConnectionName(
      label || "custom",
      existing.map((c) => c.name),
    );
  const names = fields.map((f) => f.name.trim());
  const check: ConnectionCustomCheck | undefined =
    checkType === "httpsUrl"
      ? create(ConnectionCustomCheckSchema, { target: { case: "httpsUrl", value: url.trim() } })
      : checkType === "tcp"
        ? create(ConnectionCustomCheckSchema, {
            target: { case: "tcp", value: { host: host.trim(), port: Number(port) } },
          })
        : undefined;
  const checkKey = JSON.stringify([checkType, url, host, port]);
  const checkReady =
    checkType === "httpsUrl"
      ? url.startsWith("https://")
      : host.trim() !== "" && /^\d+$/.test(port) && Number(port) > 0 && Number(port) <= 65535;
  const ready =
    label.trim() !== "" &&
    fields.length > 0 &&
    fields.every((f) => !renamedSecret(f) || f.value !== "") &&
    names.every((name) => /^[A-Za-z][A-Za-z0-9_-]{0,63}$/.test(name)) &&
    new Set(
      names.map((name) =>
        name
          .replace(/([A-Z]+)([A-Z][a-z])/g, "$1_$2")
          .replace(/([a-z0-9])([A-Z])/g, "$1_$2")
          .toUpperCase()
          .replace(/[-_]+/g, "_")
          .replace(/_+$/, ""),
      ),
    ).size === names.length &&
    (reach !== ConnectionReach.PROJECTS || picked.length > 0) &&
    (!check || checkReady);

  function updateField(id: number, edit: Partial<Field>) {
    setFields((was) => was.map((f) => (f.id === id ? { ...f, ...edit } : f)));
  }

  async function runCheck() {
    if (!check || !checkReady) return;
    setChecking(true);
    try {
      const result = words(await client.checkConnection({ custom: check, server }));
      setAnswer({ key: checkKey, line: result.line, passed: result.passed });
    } catch (err) {
      setAnswer({ key: checkKey, line: sentence(err), passed: false });
    } finally {
      setChecking(false);
    }
  }

  function review() {
    if (!ready) return;
    const custom = create(ConnectionCustomSchema, {
      initial: initial.trim(),
      check,
      fields: fields.map((f) => ({
        name: f.name.trim(),
        type: f.type,
        ...(f.type === fieldType.SECRET ? { secretValue: f.clear ? "" : f.value || undefined } : { value: f.value }),
      })),
    });
    const spec = {
      displayName: label.trim(),
      shared,
      reach,
      projects: reach === ConnectionReach.PROJECTS ? picked : [],
      custom,
    };
    const onApplied = (change: { state: ChangeState } | undefined) => {
      if (change?.state !== ChangeState.APPLIED && change?.state !== ChangeState.APPLYING) return;
      void query.refetch().then(() => {
        onClose();
        onSaved?.(name);
      });
    };
    if (editing) {
      sheet.open({
        kind: "propose",
        summary: t`Change the connection ${name}`,
        edits: [
          {
            resource: { kind: "connection", name },
            baseRevision: editing.revision,
            spec: { kind: { case: "connection", value: { ...spec, kind: LinkKind.CUSTOM } } },
          },
        ],
        onClose: onApplied,
      });
      return;
    }
    sheet.open({ kind: "plan", make: () => client.createCustomConnection({ name, ...spec }), onClose: onApplied });
  }

  return (
    <Sheet
      open
      onClose={onClose}
      title={editing ? t`Connection ${name}` : t`Add a Custom connection`}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button type="submit" form={form} disabled={!ready} {...maySave.off}>
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
        <TextField label={t`Name`} value={label} onChange={(e) => setLabel(e.target.value)} maxLength={60} />
        {!editing ? (
          <SelectField
            label={t`Start with`}
            value={template}
            options={[
              { value: "empty", label: t`Empty form` },
              { value: "api", label: t`API key` },
              { value: "database", label: t`Database URL` },
              { value: "mail", label: t`SMTP-like service` },
            ]}
            onChange={(e) => {
              const next = e.target.value as keyof typeof presets;
              setTemplate(next);
              setFields(presets[next].map((f) => ({ ...f, id: nextID.current++, value: "", set: false })));
            }}
          />
        ) : null}
        <fieldset className="custom-connection-fields">
          <legend>
            <Trans>Fields</Trans>
          </legend>
          {fields.map((f, i) => (
            <div key={f.id} className="custom-connection-field">
              <TextField
                label={t`Field ${i + 1} name`}
                mono
                value={f.name}
                onChange={(e) => updateField(f.id, { name: e.target.value, clear: false })}
              />
              <SelectField
                label={t`Field ${i + 1} type`}
                value={String(f.type)}
                options={Object.entries(types)
                  .slice(1)
                  .map(([value, label]) => ({ value, label }))}
                onChange={(e) => updateField(f.id, { type: Number(e.target.value), value: "", clear: false })}
              />
              <TextField
                label={t`Field ${i + 1} value`}
                type={f.type === fieldType.SECRET ? "password" : "text"}
                autoComplete={f.type === fieldType.SECRET ? "new-password" : "off"}
                placeholder={
                  f.type === fieldType.SECRET && f.set && !renamedSecret(f) && !f.clear ? t`Secret is set` : undefined
                }
                hint={renamedSecret(f) ? t`Enter the secret again to save the field under its new name.` : undefined}
                mono
                value={f.value}
                onChange={(e) => updateField(f.id, { value: e.target.value, clear: false })}
              />
              <div className="custom-connection-actions">
                {f.type === fieldType.SECRET && f.set && !renamedSecret(f) ? (
                  <Button
                    type="button"
                    variant="ghost"
                    size="sm"
                    onClick={() => updateField(f.id, { clear: !f.clear, value: "" })}
                  >
                    {f.clear ? t`Keep secret` : t`Clear secret`}
                  </Button>
                ) : null}
                <Button
                  type="button"
                  variant="ghost"
                  size="sm"
                  icon="trash"
                  disabled={fields.length === 1}
                  aria-label={t`Remove field ${i + 1}`}
                  onClick={() => setFields((was) => was.filter((x) => x.id !== f.id))}
                />
              </div>
            </div>
          ))}
          <Button
            type="button"
            variant="secondary"
            size="sm"
            icon="plus"
            disabled={fields.length >= 32}
            onClick={() => {
              const id = nextID.current++;
              setFields((was) => [...was, { id, name: "", type: fieldType.TEXT, value: "", set: false }]);
            }}
          >
            <Trans>Add field</Trans>
          </Button>
        </fieldset>
        <Advanced>
          <TextField label={t`Initials`} value={initial} onChange={(e) => setInitial(e.target.value)} maxLength={3} />
          <SelectField
            label={t`Who may use it`}
            value={String(reach)}
            options={[
              { value: String(ConnectionReach.EVERY), label: t`Every project` },
              { value: String(ConnectionReach.PROJECTS), label: t`Selected projects` },
              { value: String(ConnectionReach.UNSPECIFIED), label: t`No projects` },
            ]}
            onChange={(e) => setReach(Number(e.target.value))}
          />
          {reach === ConnectionReach.PROJECTS ? (
            <fieldset className="custom-connection-fields">
              <legend>
                <Trans>Projects</Trans>
              </legend>
              {projects.map((p) => (
                <Switch
                  key={p.id}
                  label={p.displayName || p.resource?.name || p.id}
                  checked={picked.includes(p.id)}
                  onChange={(on) => setPicked((was) => (on ? [...was, p.id] : was.filter((id) => id !== p.id)))}
                />
              ))}
            </fieldset>
          ) : null}
          <Switch label={t`Allow multiple apps`} checked={shared} onChange={setShared} />
          <SelectField
            label={t`Check`}
            value={checkType}
            options={[
              { value: "", label: t`No check` },
              { value: "httpsUrl", label: t`HTTPS GET` },
              { value: "tcp", label: t`TCP connection` },
            ]}
            onChange={(e) => setCheckType(e.target.value)}
          />
          {checkType === "httpsUrl" ? (
            <TextField label={t`HTTPS URL`} value={url} onChange={(e) => setUrl(e.target.value)} mono />
          ) : checkType === "tcp" ? (
            <>
              <TextField label={t`TCP host`} value={host} onChange={(e) => setHost(e.target.value)} mono />
              <TextField
                label={t`TCP port`}
                value={port}
                onChange={(e) => setPort(e.target.value)}
                mono
                inputMode="numeric"
              />
            </>
          ) : null}
          {check && mayCheck ? (
            <Button
              type="button"
              variant="secondary"
              icon="refresh"
              disabled={!checkReady || !server}
              loading={checking}
              onClick={() => void runCheck()}
            >
              <Trans>Run check</Trans>
            </Button>
          ) : null}
          {check && answer?.key === checkKey ? (
            <Callout tone={answer.passed ? "healthy" : "critical"} title={answer.line} />
          ) : null}
        </Advanced>
      </form>
    </Sheet>
  );
}
