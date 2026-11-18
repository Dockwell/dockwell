// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import { AuthService, type Database, DatabaseService, Protocol, ServiceService } from "@dockwell/api";
import { Button, CopyButton, Panel, Switch, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useState } from "react";
import { appOf } from "../apps/app";
import { StateLine } from "../apps/DomainStep";
import { ownEntries } from "../apps/PrivateSheet";
import { useServices } from "../apps/useServices";
import { useMay } from "../auth/may";
import { type Opening, useChangeSheet } from "../changes/ChangeSheet";
import { useFleetView } from "../servers/useFleet";

/** The addresses typed, one or more, split where a person would: commas, spaces or lines. */
function listOf(typed: string): string[] {
  return typed
    .split(/[\s,]+/)
    .map((a) => a.trim())
    .filter(Boolean);
}

/**
 * Who reaches a database from beyond its server (Database-Detail, #798): off, it's
 * its own apps alone; on, its port answers the addresses listed and nobody else, as
 * one plan of three edits (ADR 0038, "External access, as built"). What crosses the
 * network is unencrypted, so an SSH tunnel is one step away beside it.
 */
export function ExternalAccess({ db }: { db: Database }) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const may = useMay()(DatabaseService.method.setDatabaseExternalAccess, { environment: db.resource?.scope ?? "" });
  const open = db.externalAccess;
  const reading = db.readings[0];
  const serverName = reading?.serverName ?? "";
  // Your own address, as the control plane saw this request, is what's let in first.
  const sessions = useQuery(AuthService.method.listSessions, {});
  const own = ownEntries(sessions.data?.sessions.find((s) => s.current)?.address ?? "");
  const [editing, setEditing] = useState(false);
  const [typed, setTyped] = useState<string>();
  const [error, setError] = useState<string>();
  const [tunnel, setTunnel] = useState(false);
  const shown = typed ?? (open ? open.allowFrom.join(", ") : (own[0] ?? ""));
  const port = String(open?.port ?? db.port);
  const plan = (request: { allowFrom?: string[]; off?: boolean }): Opening => ({
    kind: "plan",
    make: () => createClient(DatabaseService, transport).setDatabaseExternalAccess({ id: db.id, ...request }),
    noteAction: { unencrypted: { label: t`Use an SSH tunnel`, go: () => setTunnel(true) } },
    onDone: () => {
      setEditing(false);
      setTyped(undefined);
    },
  });
  const review = (event?: FormEvent) => {
    event?.preventDefault();
    const allowFrom = listOf(shown);
    if (allowFrom.length === 0) {
      setError(t`Name an address or a range to let in, such as 203.0.113.7. It's never open to anywhere.`);
      return;
    }
    setError(undefined);
    sheet.open(plan({ allowFrom }));
  };
  const flip = (on: boolean) => {
    if (on) setEditing(true);
    else if (open) sheet.open(plan({ off: true }));
    else setEditing(false);
  };
  const where = serverName || t`its server`;
  const list = open?.allowFrom.join(", ") ?? "";
  return (
    <Panel title={t`External access`} className="database-external">
      <div className="database-external-body">
        <Switch
          label={t`External access`}
          checked={open !== undefined || editing}
          onChange={flip}
          disabled={!may.allowed}
          description={
            !may.allowed ? may.reason : open ? t`On port ${port}, from ${list}` : t`Off. Only apps on ${where} reach it`
          }
        />
        {open && !editing ? (
          <Button size="sm" variant="ghost" onClick={() => setEditing(true)} {...may.off}>
            <Trans>Change addresses</Trans>
          </Button>
        ) : null}
        {editing ? (
          <form className="database-external-form" onSubmit={review}>
            <TextField
              label={t`Allow from`}
              value={shown}
              mono
              placeholder="203.0.113.0/24"
              error={error}
              hint={t`Port ${port} then answers these addresses alone`}
              onChange={(event) => {
                setTyped(event.target.value);
                setError(undefined);
              }}
            />
            <div className="database-external-actions">
              <Button size="sm" variant="ghost" onClick={() => setEditing(false)}>
                <Trans>Cancel</Trans>
              </Button>
              <Button size="sm" variant="primary" type="submit" icon="commit">
                <Trans>Review plan</Trans>
              </Button>
            </div>
          </form>
        ) : null}
        <StateLine
          status={open ? "degraded" : "stopped"}
          action={
            <button type="button" className="domain-step-act" aria-expanded={tunnel} onClick={() => setTunnel(!tunnel)}>
              <Trans>Use an SSH tunnel</Trans>
            </button>
          }
        >
          {open ? t`Unencrypted over the network` : t`Nothing beyond ${where} reaches it`}
        </StateLine>
        {tunnel ? <Tunnel db={db} /> : null}
      </div>
    </Panel>
  );
}

/**
 * The SSH tunnel to a database: its port on the server alone, where it has one,
 * forwarded to yours, as a command to copy. Without one, a plan publishes it there.
 */
function Tunnel({ db }: { db: Database }) {
  const { t } = useLingui();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const may = useMay()(ServiceService.method.addServicePort, { environment: db.resource?.scope ?? "" });
  const service = useServices().data?.find((s) => s.id === db.id);
  const { servers } = useFleetView();
  // Its address where the server knows one, or its name, as an SSH config may call it.
  const address = servers?.find((s) => s.id === db.readings[0]?.serverId)?.address || db.readings[0]?.serverName || "";
  // A port on every address answers on the server's own too.
  const local =
    (service ? appOf(service)?.ports : [])?.find(
      (p) =>
        p.containerPort === db.port && p.protocol !== Protocol.UDP && ["127.0.0.1", "", "0.0.0.0"].includes(p.hostIp),
    )?.hostPort ?? db.externalAccess?.port;
  if (!local) {
    const port = db.port + 10000 <= 65535 ? db.port + 10000 : db.port;
    return (
      <div className="database-tunnel">
        <StateLine
          status="stopped"
          action={
            <button
              type="button"
              className="domain-step-act"
              onClick={() =>
                sheet.open({
                  kind: "plan",
                  make: () =>
                    createClient(ServiceService, transport).addServicePort({
                      id: db.id,
                      hostPort: port,
                      containerPort: db.port,
                      hostIp: "127.0.0.1",
                    }),
                })
              }
              {...may.off}
            >
              <Trans>Publish it on this server alone</Trans>
            </button>
          }
        >
          {t`A tunnel needs its port on the server first`}
        </StateLine>
      </div>
    );
  }
  const command = `ssh -N -L ${local}:127.0.0.1:${local} ${address}`;
  return (
    <div className="database-tunnel">
      <code className="database-tunnel-command">{command}</code>
      <CopyButton
        text={command}
        label={t`Copy the SSH tunnel command`}
        copiedLabel={t`SSH tunnel command copied`}
        variant="ghost"
      />
    </div>
  );
}
