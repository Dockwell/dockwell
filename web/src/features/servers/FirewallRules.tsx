// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import {
  type DockwellFirewallRule,
  type EditSchema,
  FirewallAction,
  type FirewallPolicy,
  Protocol,
  type Server,
} from "@dockwell/api";
import { Button, Callout, Panel, SelectField, Sheet, Switch, Table, type TableColumn, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useRef, useState } from "react";

import { type Opening, useChangeSheet } from "../changes/ChangeSheet";

type Edit = MessageInitShape<typeof EditSchema>;

/** A rule's resource on its server. */
function ruleRef(server: Server, name: string) {
  return { kind: "firewall.rule", scope: server.id, name };
}

interface RuleRow {
  id: string;
  rule: DockwellFirewallRule;
}

/**
 * Dockwell's own rules for a server, and its policy, as ADR 0052 has them: what
 * Dockwell allows and denies, in its own table, and whether it drops what no
 * rule lets in. Each change is a plan, applied with a passkey, and kept only
 * once you confirm you can still reach the server.
 */
export function DockwellRules({
  server,
  rules,
  policy,
  onChanged,
}: {
  server: Server;
  rules: readonly DockwellFirewallRule[];
  policy: FirewallPolicy | undefined;
  /** Reads the firewall again, once a change it opened is over. */
  onChanged?: () => void;
}) {
  const { t } = useLingui();
  const changes = useChangeSheet();
  // What the sheet changed shows here at once, not at the next read.
  const sheet = { open: (opening: Opening) => changes.open({ ...opening, onClose: () => onChanged?.() }) };
  const [adding, setAdding] = useState(false);
  const name = server.name;
  const denies = policy?.denyEverythingElse ?? false;
  const setPolicy = (on: boolean) =>
    sheet.open({
      kind: "propose",
      summary: on ? t`Deny everything else on ${name}` : t`Let in what no rule drops on ${name}`,
      edits: [
        on
          ? {
              resource: { kind: "firewall.policy", scope: server.id, name: "default" },
              spec: { kind: { case: "firewallPolicy", value: { denyEverythingElse: true } } },
            }
          : { resource: { kind: "firewall.policy", scope: server.id, name: "default" }, delete: true },
      ],
    });
  const remove = (rule: DockwellFirewallRule) =>
    sheet.open({
      kind: "propose",
      summary: t`Remove the rule ${rule.name} from ${name}`,
      edits: [{ resource: ruleRef(server, rule.name), delete: true }],
    });
  const rows: RuleRow[] = rules.map((rule) => ({ id: rule.name, rule }));
  const columns: TableColumn<RuleRow>[] = [
    {
      key: "action",
      header: t`Action`,
      width: 96,
      render: ({ rule: r }) => <strong>{r.rule?.action === FirewallAction.DENY ? t`Deny` : t`Allow`}</strong>,
    },
    {
      key: "port",
      header: t`Port`,
      mono: true,
      render: ({ rule: r }) => `${r.rule?.port}/${r.rule?.protocol === Protocol.UDP ? "udp" : "tcp"}`,
    },
    {
      key: "from",
      header: t`From`,
      render: ({ rule: r }) => (r.rule?.sources.length ? <code>{r.rule.sources.join(", ")}</code> : t`Anywhere`),
    },
    { key: "note", header: t`Note`, muted: true, render: ({ rule: r }) => r.rule?.comment ?? "" },
    {
      key: "remove",
      header: <span className="dw-sr">{t`Remove`}</span>,
      width: 96,
      align: "right",
      render: ({ rule: r }) => (
        <Button variant="ghost" size="sm" onClick={() => remove(r)} aria-label={t`Remove the rule ${r.name}`}>
          <Trans>Remove</Trans>
        </Button>
      ),
    },
  ];
  return (
    <Panel
      title={t`Dockwell's rules`}
      actions={
        <Button variant="secondary" size="sm" icon="plus" onClick={() => setAdding(true)}>
          <Trans>Add a rule</Trans>
        </Button>
      }
      flush
      className="firewall-tool"
    >
      <div className="firewall-policy">
        <Switch
          checked={denies}
          onChange={setPolicy}
          label={t`Deny everything else`}
          description={t`Drops what no rule lets in. Connections already open, SSH from where people come in now, and on the control server its own ports, stay let in.`}
        />
      </div>
      {rows.length > 0 ? (
        <Table columns={columns} rows={rows} rowKey="id" caption={t`Dockwell's rules`} density="compact" />
      ) : (
        <p className="firewall-note">
          <Trans>Dockwell has no rules for {name} yet. What comes in is as the server's own firewall leaves it.</Trans>
        </p>
      )}
      {rows.length > 0 || denies ? (
        <p className="firewall-note">
          <Trans>
            As {name} starts, these are off until its agent is up, a few seconds in, which loads them again. Nothing is
            lost, and nobody is locked out, as they only narrow what comes in.
          </Trans>
        </p>
      ) : null}
      {adding ? (
        <AddRuleSheet
          server={server}
          taken={rules.map((r) => r.name)}
          onClose={() => setAdding(false)}
          open={sheet.open}
        />
      ) : null}
    </Panel>
  );
}

/** A name for a rule that no other on the server has, such as allow-443, or allow-443-udp. */
export function ruleName(action: "allow" | "deny", port: number, udp: boolean, taken: readonly string[]): string {
  const base = `${action}-${port}${udp ? "-udp" : ""}`;
  let name = base;
  for (let n = 2; taken.includes(name); n++) name = `${base}-${n}`;
  return name;
}

/** A new rule: what it does, to which port, from where, planned as a change. */
function AddRuleSheet({
  server,
  taken,
  onClose,
  open,
}: {
  server: Server;
  taken: readonly string[];
  onClose: () => void;
  open: (opening: Opening) => void;
}) {
  const { t } = useLingui();
  const field = useRef<HTMLInputElement>(null);
  const [action, setAction] = useState<"allow" | "deny">("allow");
  const [protocol, setProtocol] = useState<"tcp" | "udp">("tcp");
  const [port, setPort] = useState("");
  const [from, setFrom] = useState("");
  const [note, setNote] = useState("");
  const [error, setError] = useState<string>();
  const submit = (event?: FormEvent) => {
    event?.preventDefault();
    const n = Number(port.trim());
    if (!Number.isInteger(n) || n < 1 || n > 65535) {
      setError(t`Name a port from 1 to 65535, such as 443.`);
      field.current?.focus();
      return;
    }
    const sources = from
      .split(/[\s,]+/)
      .map((s) => s.trim())
      .filter(Boolean);
    const name = ruleName(action, n, protocol === "udp", taken);
    const edit: Edit = {
      resource: ruleRef(server, name),
      spec: {
        kind: {
          case: "firewallRule",
          value: {
            action: action === "deny" ? FirewallAction.DENY : FirewallAction.ALLOW,
            protocol: protocol === "udp" ? Protocol.UDP : Protocol.TCP,
            port: n,
            sources,
            comment: note.trim(),
          },
        },
      },
    };
    const serverName = server.name;
    onClose();
    open({
      kind: "propose",
      summary:
        action === "deny" ? t`Deny ${n}/${protocol} on ${serverName}` : t`Allow ${n}/${protocol} on ${serverName}`,
      edits: [edit],
    });
  };
  return (
    <Sheet
      open
      initialFocusRef={field}
      title={t`Add a rule to ${server.name}`}
      subtitle={t`It applies as a plan, and stays only once you confirm you can still reach the server`}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" onClick={() => submit()}>
            <Trans>Plan it</Trans>
          </Button>
        </>
      }
    >
      <form onSubmit={submit} className="firewall-add">
        {error ? <Callout tone="critical">{error}</Callout> : null}
        <SelectField
          label={t`Action`}
          value={action}
          onChange={(e) => setAction(e.target.value as "allow" | "deny")}
          options={[
            { value: "allow", label: t`Allow` },
            { value: "deny", label: t`Deny` },
          ]}
        />
        <TextField
          ref={field}
          label={t`Port`}
          mono
          inputMode="numeric"
          value={port}
          onChange={(e) => {
            setPort(e.target.value);
            setError(undefined);
          }}
        />
        <SelectField
          label={t`Protocol`}
          value={protocol}
          onChange={(e) => setProtocol(e.target.value as "tcp" | "udp")}
          options={[
            { value: "tcp", label: "TCP" },
            { value: "udp", label: "UDP" },
          ]}
        />
        <TextField
          label={t`From`}
          mono
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          value={from}
          hint={t`Addresses or ranges, such as 203.0.113.0/24, apart by commas. Empty is anywhere.`}
          onChange={(e) => setFrom(e.target.value)}
        />
        <TextField
          label={t`Note`}
          value={note}
          hint={t`Why the rule is there, shown beside it.`}
          onChange={(e) => setNote(e.target.value)}
        />
      </form>
    </Sheet>
  );
}
