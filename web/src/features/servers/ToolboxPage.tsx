// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  ChangeService,
  type HostToolboxOperationSchema,
  type HostToolboxStatus,
  ResourceService,
  ServerService,
  ToolboxCleanupKind,
} from "@dockwell/api";
import {
  Advanced,
  Button,
  Callout,
  CodeField,
  Loading,
  Panel,
  SelectField,
  Sheet,
  StatusBadge,
  Switch,
  TextField,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import { type FormEvent, useId, useState } from "react";

import { sentence } from "../../lib/errors";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useReadAgainOnceOver } from "../changes/follow";
import { useAllVariables } from "../variables/VariablesPage";
import { AgentOutdated, isAgentOutdated } from "./AgentOutdated";
import { ServerFrame } from "./ServerFrame";
import "./ToolboxPage.css";

type Operation = NonNullable<MessageInitShape<typeof HostToolboxOperationSchema>["operation"]>;
type Editor = "swap" | "swappiness" | "hostname" | "timeZone" | "dns" | "hosts" | "cleanup" | "service";
type PlanOperation = (operation: Operation) => void;

export function ToolboxPage({ serverId }: { serverId: string }) {
  const { t } = useLingui();
  const may = useMay()(ServerService.method.getHostToolbox);
  const status = useQuery(ServerService.method.getHostToolbox, { serverId }, { enabled: may.allowed, retry: false });
  return (
    <ServerFrame id={serverId}>
      {(server) =>
        !may.allowed ? (
          <Callout tone="warning">{may.reason ?? t`An admin can use the host toolbox.`}</Callout>
        ) : isAgentOutdated(status.error) ? (
          <AgentOutdated server={server}>{t`This agent needs an update to manage host settings.`}</AgentOutdated>
        ) : status.error ? (
          <Callout
            tone="warning"
            actions={
              <Button variant="secondary" onClick={() => void status.refetch()}>
                <Trans>Try again</Trans>
              </Button>
            }
          >
            {sentence(status.error)}
          </Callout>
        ) : status.data?.status ? (
          <Toolbox serverId={serverId} status={status.data.status} refresh={() => void status.refetch()} />
        ) : (
          <Loading label={t`Reading host settings`} />
        )
      }
    </ServerFrame>
  );
}

function Toolbox({ serverId, status, refresh }: { serverId: string; status: HostToolboxStatus; refresh: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const changeSheet = useChangeSheet();
  const readAgain = useReadAgainOnceOver(refresh);
  const [editor, setEditor] = useState<Editor>();
  const onPlan: PlanOperation = (operation) => {
    setEditor(undefined);
    const client = createClient(ChangeService, transport);
    changeSheet.open({
      kind: "plan",
      planningLabel: operation.case === "cleanup" ? t`Scanning host files` : t`Checking the server`,
      make: async () => {
        const res = await client.planHostToolbox({ serverId, operation: { operation } });
        const cleanup = res.change?.plan?.resources[0]?.toolbox;
        if (res.change && operation.case === "cleanup" && !cleanup?.files.length) {
          await client.discardChange({ id: res.change.id });
          return { notes: [t`No eligible files found.`] };
        }
        return res;
      },
      onClose: readAgain,
    });
  };

  const active = status.activeSwap;
  return (
    <div className="toolbox">
      <h2 className="dw-text-heading">
        <Trans>Toolbox</Trans>
      </h2>
      <div className="toolbox-grid">
        <Panel
          title={t`Swap`}
          actions={
            !status.swapUnavailable || status.managedSwapMib > 0n ? (
              <Button variant="secondary" size="sm" onClick={() => setEditor("swap")}>
                {status.managedSwapMib ? t`Resize swap` : t`Add swap`}
              </Button>
            ) : undefined
          }
        >
          {active.length ? (
            <ul className="toolbox-list">
              {active.map((item) => (
                <li key={item.path}>
                  <code>{item.path}</code>
                  <span>{t`${String(item.usedMib)} of ${String(item.sizeMib)} MiB used`}</span>
                </li>
              ))}
            </ul>
          ) : (
            <p className="ink-muted">
              <Trans>No active swap</Trans>
            </p>
          )}
          {status.swapUnavailable ? <p className="ink-muted">{status.swapUnavailable}</p> : null}
          <div className="toolbox-line">
            <span>
              <Trans>Swappiness</Trans> <code>{status.swappiness}</code>
            </span>
            <Button variant="ghost" size="sm" onClick={() => setEditor("swappiness")}>
              <Trans>Change swappiness</Trans>
            </Button>
          </div>
          {status.managedSwapMib > 0n && (
            <Button variant="ghost" size="sm" onClick={() => onPlan({ case: "swap", value: { sizeMib: 0n } })}>
              <Trans>Remove swap</Trans>
            </Button>
          )}
        </Panel>
        <Panel title={t`Identity and time`}>
          <div className="toolbox-line">
            <div className="toolbox-fact">
              <span>
                <Trans>Hostname</Trans>
              </span>
              <code>{status.hostname || t`Unknown`}</code>
            </div>
            <Button variant="ghost" size="sm" onClick={() => setEditor("hostname")}>
              <Trans>Change hostname</Trans>
            </Button>
          </div>
          <div className="toolbox-line">
            <div className="toolbox-fact">
              <span>
                <Trans>Time zone</Trans>
              </span>
              <code>{status.timeZone || t`Unknown`}</code>
            </div>
            <Button variant="ghost" size="sm" onClick={() => setEditor("timeZone")}>
              <Trans>Change time zone</Trans>
            </Button>
          </div>
          <div className="toolbox-line">
            <div className="toolbox-fact">
              <span>
                <Trans>Time synchronization</Trans>
              </span>
              <StatusBadge
                labelIsState
                status={
                  status.ntpEnabled && status.ntpSynced
                    ? "healthy"
                    : status.ntpEnabled
                      ? "running"
                      : status.ntpAvailable
                        ? "stopped"
                        : "unknown"
                }
                label={
                  !status.ntpAvailable
                    ? t`Unavailable`
                    : status.ntpSynced && status.ntpEnabled
                      ? t`Synchronized`
                      : status.ntpEnabled
                        ? t`Waiting for sync`
                        : t`Off`
                }
              />
            </div>
            {status.ntpAvailable ? (
              <Button variant="ghost" size="sm" onClick={() => onPlan({ case: "ntp", value: !status.ntpEnabled })}>
                {status.ntpEnabled ? t`Turn NTP off` : t`Turn NTP on`}
              </Button>
            ) : null}
          </div>
        </Panel>
        <Panel title={t`DNS and hosts`}>
          <div className="toolbox-line">
            <div className="toolbox-fact">
              <span>
                <Trans>Resolver owner</Trans>
              </span>
              <code>{status.dnsOwner}</code>
            </div>
            {status.canEditDns ? (
              <Button variant="ghost" size="sm" onClick={() => setEditor("dns")}>
                <Trans>Change DNS</Trans>
              </Button>
            ) : null}
          </div>
          <div className="toolbox-fact">
            <span>
              {status.dnsOwner === "systemd-resolved" || status.dnsOwner === "netplan" ? (
                <Trans>Global DNS servers</Trans>
              ) : (
                <Trans>DNS servers</Trans>
              )}
            </span>
            <code>{status.dnsServers.join(", ") || t`Unknown`}</code>
          </div>
          {status.linkDnsServers.length ? (
            <div className="toolbox-fact">
              <span>
                <Trans>Link DNS servers</Trans>
              </span>
              <code>{status.linkDnsServers.join(", ")}</code>
            </div>
          ) : null}
          <div className="toolbox-line">
            <span>
              <Trans>Dockwell hosts entries</Trans>
            </span>
            <Button variant="ghost" size="sm" onClick={() => setEditor("hosts")}>
              <Trans>Add hosts entry</Trans>
            </Button>
          </div>
          {status.hosts.length > 0 && (
            <ul className="toolbox-list">
              {status.hosts.map(({ hostname, address }) => (
                <li key={hostname}>
                  <div className="toolbox-fact">
                    <code>{hostname}</code>
                    <code>{address}</code>
                  </div>
                  <Button
                    variant="ghost"
                    size="sm"
                    aria-label={t`Remove hosts entry for ${hostname}`}
                    onClick={() => onPlan({ case: "hosts", value: { hostname } })}
                  >
                    <Trans>Remove</Trans>
                  </Button>
                </li>
              ))}
            </ul>
          )}
        </Panel>
        <Panel
          title={t`Host cleanup`}
          actions={
            <Button variant="secondary" size="sm" onClick={() => setEditor("cleanup")}>
              <Trans>Scan files</Trans>
            </Button>
          }
        >
          <p className="ink-muted">
            <Trans>Package cache, archived journals, rotated logs and old temporary files.</Trans>
          </p>
          <Link to="/servers/$serverId/docker/cleanup" params={{ serverId }}>
            <Trans>Docker cleanup</Trans>
          </Link>
        </Panel>
        <Panel
          title={t`Background services`}
          actions={
            status.systemd ? (
              <Button variant="secondary" size="sm" onClick={() => setEditor("service")}>
                <Trans>New service</Trans>
              </Button>
            ) : undefined
          }
        >
          {!status.systemd ? (
            <p className="ink-muted">
              <Trans>Creating background services requires systemd.</Trans>
            </p>
          ) : status.services.length > 0 ? (
            <ul className="toolbox-list">
              {status.services.map((unit) => (
                <li key={unit}>
                  <Link to="/servers/$serverId/services" params={{ serverId }} search={{ unit }}>
                    <code>{unit}</code>
                  </Link>
                  <Button
                    variant="ghost"
                    size="sm"
                    aria-label={t`Remove ${unit}`}
                    onClick={() =>
                      onPlan({
                        case: "service",
                        value: { name: unit.slice("dockwell-toolbox-".length, -".service".length), remove: true },
                      })
                    }
                  >
                    <Trans>Remove</Trans>
                  </Button>
                </li>
              ))}
            </ul>
          ) : (
            <p className="ink-muted">
              <Trans>No background services created in Dockwell.</Trans>
            </p>
          )}
          <Link to="/servers/$serverId/services" params={{ serverId }} search={{}}>
            <Trans>System services</Trans>
          </Link>
        </Panel>
      </div>
      {editor === "service" ? (
        <ServiceEditor close={() => setEditor(undefined)} plan={onPlan} />
      ) : editor ? (
        <SettingsEditor key={editor} editor={editor} status={status} close={() => setEditor(undefined)} plan={onPlan} />
      ) : null}
    </div>
  );
}

type FormProps = { close: () => void; plan: PlanOperation };

function SettingsEditor({
  editor,
  status,
  close,
  plan,
}: FormProps & { editor: Exclude<Editor, "service">; status: HostToolboxStatus }) {
  const { t } = useLingui();
  const form = useId();
  const [value, setValue] = useState(
    () =>
      ({
        swap: String(status.managedSwapMib || status.recommendedSwapMib || 256n),
        swappiness: String(status.swappiness),
        hostname: status.hostname,
        timeZone: status.timeZone || "UTC",
        dns: (status.dnsOwner === "netplan" ? status.linkDnsServers : status.dnsServers).join(", "),
        hosts: "",
        cleanup: "7",
      })[editor],
  );
  const [address, setAddress] = useState("");
  const [kind, setKind] = useState(ToolboxCleanupKind.APT);
  const [vacuum, setVacuum] = useState("age");
  const journal = kind === ToolboxCleanupKind.JOURNAL;
  const age = !journal || vacuum === "age";
  const fileKinds = [
    { value: String(ToolboxCleanupKind.APT), label: t`APT package cache` },
    { value: String(ToolboxCleanupKind.JOURNAL), label: t`Archived journals` },
    { value: String(ToolboxCleanupKind.ROTATED_LOGS), label: t`Rotated logs` },
    { value: String(ToolboxCleanupKind.TEMPORARY), label: t`Temporary files` },
  ];
  const title = {
    swap: t`Swap size`,
    swappiness: t`Swappiness`,
    hostname: t`Hostname`,
    timeZone: t`Time zone`,
    dns: t`DNS servers`,
    hosts: t`Add hosts entry`,
    cleanup: t`Scan host files`,
  }[editor];

  function submit(event: FormEvent) {
    event.preventDefault();
    switch (editor) {
      case "swap":
        return plan({ case: "swap", value: { sizeMib: BigInt(value) } });
      case "swappiness":
        return plan({ case: "swappiness", value: Number(value) });
      case "hostname":
        return plan({ case: "hostname", value: value.trim() });
      case "timeZone":
        return plan({ case: "timeZone", value });
      case "dns":
        return plan({ case: "dns", value: { servers: value.split(/[\s,]+/).filter(Boolean) } });
      case "hosts":
        return plan({ case: "hosts", value: { hostname: value.trim(), address: address.trim() } });
      case "cleanup":
        return plan({
          case: "cleanup",
          value: {
            kind,
            days: age && kind !== ToolboxCleanupKind.APT ? Number(value) : 0,
            keepMib: age ? 0n : BigInt(value),
          },
        });
    }
  }
  return (
    <Sheet
      open
      title={title}
      onClose={close}
      footer={<EditorFooter form={form} close={close} scan={editor === "cleanup"} />}
    >
      <form className="toolbox-form" id={form} onSubmit={submit}>
        {editor === "swap" ? (
          <TextField
            label={t`Size (MiB)`}
            type="number"
            min={128}
            max={Number(status.swapFreeMib)}
            required
            value={value}
            onChange={(e) => setValue(e.target.value)}
            hint={t`${String(status.swapFreeMib)} MiB available for a new swap file.`}
          />
        ) : editor === "swappiness" ? (
          <TextField
            label={title}
            type="number"
            min={0}
            max={200}
            required
            value={value}
            onChange={(e) => setValue(e.target.value)}
          />
        ) : editor === "timeZone" ? (
          <SelectField
            label={title}
            value={value}
            onChange={(e) => setValue(e.target.value)}
            options={status.timeZones.map((zone) => ({ value: zone, label: zone }))}
          />
        ) : editor === "cleanup" ? (
          <>
            <SelectField
              label={t`Files to scan`}
              value={String(kind)}
              options={fileKinds}
              onChange={(e) => {
                setKind(Number(e.target.value));
                setVacuum("age");
                setValue("7");
              }}
            />
            {journal ? (
              <SelectField
                label={t`Keep journals by`}
                value={vacuum}
                options={[
                  { value: "age", label: t`Age` },
                  { value: "size", label: t`Size` },
                ]}
                onChange={(e) => {
                  setVacuum(e.target.value);
                  setValue(e.target.value === "age" ? "7" : "500");
                }}
              />
            ) : null}
            {kind !== ToolboxCleanupKind.APT ? (
              <TextField
                label={age ? t`Older than (days)` : t`Keep up to (MiB)`}
                type="number"
                min={kind === ToolboxCleanupKind.TEMPORARY ? 3 : 1}
                max={age ? 3650 : 1_048_576}
                required
                value={value}
                onChange={(e) => setValue(e.target.value)}
              />
            ) : null}
          </>
        ) : (
          <>
            <TextField
              label={editor === "hosts" ? t`Hostname` : title}
              required
              value={value}
              onChange={(e) => setValue(e.target.value)}
              mono
              hint={editor === "dns" ? t`Enter one to three IP addresses, separated by commas.` : undefined}
            />
            {editor === "hosts" ? (
              <TextField
                label={t`IP address`}
                required
                value={address}
                onChange={(e) => setAddress(e.target.value)}
                mono
              />
            ) : null}
          </>
        )}
      </form>
    </Sheet>
  );
}

function EditorFooter({ form, close, scan = false }: { form: string; close: () => void; scan?: boolean }) {
  return (
    <>
      <Button variant="ghost" onClick={close}>
        <Trans>Cancel</Trans>
      </Button>
      <Button variant="primary" type="submit" form={form}>
        {scan ? <Trans>Scan files</Trans> : <Trans>Review plan</Trans>}
      </Button>
    </>
  );
}

function ServiceEditor({ close, plan }: FormProps) {
  const { t } = useLingui();
  const form = useId();
  const [name, setName] = useState("");
  const [executable, setExecutable] = useState("");
  const [args, setArgs] = useState("");
  const [account, setAccount] = useState("");
  const [dir, setDir] = useState("/");
  const [restart, setRestart] = useState("on-failure");
  const [boot, setBoot] = useState(true);
  const [environmentId, setEnvironment] = useState("");
  const [selected, setSelected] = useState("");
  const [bindings, setBindings] = useState<
    (
      | { name: string; source: string; secret: boolean; environmentId: string }
      | { name: string; literalValue: string }
    )[]
  >([]);
  const [literalName, setLiteralName] = useState("");
  const [literalValue, setLiteralValue] = useState("");
  const canAddLiteral =
    /^[A-Za-z_][A-Za-z0-9_]{0,127}$/.test(literalName) && !bindings.some((binding) => binding.name === literalName);
  const environments = useQuery(ResourceService.method.listResources, { kind: "environment" });
  const variables = useAllVariables(environmentId || undefined);
  const add = () => {
    const variable = variables.data?.variables.find((v) => v.name === selected);
    if (variable && !bindings.some((b) => b.name === variable.name)) {
      setBindings([
        ...bindings,
        { name: variable.name, source: variable.name, secret: variable.secret, environmentId },
      ]);
      setSelected("");
    }
  };
  function submit(event: FormEvent) {
    event.preventDefault();
    plan({
      case: "service",
      value: {
        name,
        user: account,
        workingDirectory: dir,
        argv: [executable, ...args.split("\n").filter((arg) => arg !== "")],
        restart,
        startOnBoot: boot,
        environment: bindings,
      },
    });
  }
  return (
    <Sheet open title={t`New background service`} onClose={close} footer={<EditorFooter form={form} close={close} />}>
      <form className="toolbox-form" id={form} onSubmit={submit}>
        <TextField
          label={t`Name`}
          value={name}
          onChange={(e) => setName(e.target.value)}
          required
          pattern={String.raw`[a-z0-9][a-z0-9\-]{0,62}`}
          mono
        />
        <TextField
          label={t`Executable`}
          value={executable}
          onChange={(e) => setExecutable(e.target.value)}
          required
          placeholder="/usr/local/bin/worker"
          mono
        />
        <CodeField
          label={t`Arguments`}
          value={args}
          onChange={(e) => setArgs(e.target.value)}
          hint={t`Enter one argument per line.`}
        />
        <TextField label={t`Run as user`} value={account} onChange={(e) => setAccount(e.target.value)} required mono />
        <Advanced label={t`Service settings`} summary={<code>{dir}</code>}>
          <div className="toolbox-form">
            <TextField label={t`Working folder`} value={dir} onChange={(e) => setDir(e.target.value)} required mono />
            <SelectField
              label={t`Restart policy`}
              value={restart}
              onChange={(e) => setRestart(e.target.value)}
              options={[
                { value: "on-failure", label: t`On failure` },
                { value: "always", label: t`Always` },
                { value: "no", label: t`Never` },
              ]}
            />
            <Switch label={t`Start at boot`} checked={boot} onChange={setBoot} />
            <SelectField
              label={t`Environment`}
              value={environmentId}
              onChange={(e) => {
                setEnvironment(e.target.value);
                setSelected("");
              }}
              options={[
                { value: "", label: t`Choose an environment` },
                ...(environments.data?.resources ?? []).map((res) => ({
                  value: res.id,
                  label: res.displayName || res.resource?.name || res.id,
                })),
              ]}
            />
            {environmentId && (
              <>
                <SelectField
                  label={t`Dockwell value`}
                  value={selected}
                  onChange={(e) => setSelected(e.target.value)}
                  options={[
                    { value: "", label: t`Choose a variable or secret` },
                    ...(variables.data?.variables ?? [])
                      .filter((v) => !bindings.some((b) => b.name === v.name))
                      .map((v) => ({
                        value: v.name,
                        label: v.secret ? t`${v.name} (secret)` : v.name,
                      })),
                  ]}
                />
                <Button type="button" variant="secondary" disabled={!selected} onClick={add}>
                  <Trans>Add value</Trans>
                </Button>
              </>
            )}
            <TextField
              label={t`Direct environment name`}
              value={literalName}
              onChange={(e) => setLiteralName(e.target.value)}
              mono
            />
            <CodeField
              label={t`Direct environment value`}
              value={literalValue}
              onChange={(e) => setLiteralValue(e.target.value)}
            />
            <Button
              type="button"
              variant="secondary"
              disabled={!canAddLiteral}
              onClick={() => {
                setBindings([...bindings, { name: literalName, literalValue }]);
                setLiteralName("");
                setLiteralValue("");
              }}
            >
              <Trans>Add direct value</Trans>
            </Button>
            {bindings.length > 0 && (
              <ul className="toolbox-list">
                {bindings.map((binding) => (
                  <li key={binding.name}>
                    <code>{binding.name}</code>
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      aria-label={t`Remove ${binding.name}`}
                      onClick={() => setBindings(bindings.filter((b) => b !== binding))}
                    >
                      <Trans>Remove</Trans>
                    </Button>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </Advanced>
      </form>
    </Sheet>
  );
}
