// SPDX-License-Identifier: AGPL-3.0-only

import { equals, type MessageInitShape } from "@bufbuild/protobuf";
import {
  type DockwellFirewallRule,
  type DockwellTable,
  type EditSchema,
  FirewallAction,
  type FirewallRule,
  FirewallRuleSchema,
  Protocol,
  type Server,
  type ServerFirewall,
} from "@dockwell/api";
import {
  Button,
  Callout,
  Panel,
  PHONE_QUERY,
  SelectField,
  Sheet,
  Switch,
  Table,
  type TableColumn,
  Tag,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, useRef, useState } from "react";

import { type Opening, useChangeSheet } from "../changes/ChangeSheet";

type Edit = MessageInitShape<typeof EditSchema>;

/** A rule's resource on its server. */
function ruleRef(server: Server, name: string) {
  return { kind: "firewall.rule", scope: server.id, name };
}

/** Where a rule stands: kept, or waiting for Keep as a change adds, changes or removes it. */
type RuleState = "kept" | "adding" | "changing" | "removing";

interface RuleRow {
  id: string;
  rule: DockwellFirewallRule;
  state: RuleState;
}

/** The rules as confirmed, beside the ones a waiting change adds, changes or removes. */
function rowsOf(table: DockwellTable | undefined): RuleRow[] {
  const kept = new Map((table?.rules ?? []).map((r) => [r.name, r]));
  if (!table?.waitingChange) return [...kept.values()].map((rule) => ({ id: rule.name, rule, state: "kept" }));
  const waiting = new Map(table.waitingRules.map((r) => [r.name, r]));
  const rows: RuleRow[] = [];
  for (const rule of kept.values()) {
    const next = waiting.get(rule.name);
    if (!next) rows.push({ id: rule.name, rule, state: "removing" });
    else if (next.rule && rule.rule && !equals(FirewallRuleSchema, next.rule, rule.rule)) {
      rows.push({ id: rule.name, rule: next, state: "changing" });
    } else rows.push({ id: rule.name, rule, state: "kept" });
  }
  for (const rule of waiting.values()) {
    if (!kept.has(rule.name)) rows.push({ id: rule.name, rule, state: "adding" });
  }
  return rows.sort((a, b) => a.id.localeCompare(b.id));
}

/** What uses a port on the server: the containers Docker publishes it for, or what listens there. */
function useUsedBy(fw: ServerFirewall | undefined): (rule: FirewallRule | undefined) => string {
  const { t } = useLingui();
  return (rule) => {
    if (!rule || !fw) return "";
    const protocol = rule.protocol === Protocol.UDP ? "udp" : "tcp";
    const containers = [
      ...new Set(
        fw.published.filter((p) => p.hostPort === rule.port && p.protocol === protocol).map((p) => p.container),
      ),
    ];
    if (containers.length > 0) return containers.join(", ");
    if (fw.listening.some((l) => l.port === rule.port && l.protocol === protocol)) return t`The server itself`;
    return t`Nothing listens`;
  };
}

/**
 * Dockwell's own rules for a server, and its policy, as ADR 0052 has them: what
 * Dockwell allows and denies, in its own table, and whether it drops what no
 * rule lets in, as the server holds them. A change that waits for Keep shows
 * beside what's kept, and nothing else changes here until it's kept or put
 * back. Each change is a plan, applied with a passkey.
 */
export function DockwellRules({
  server,
  fw,
  onChanged,
}: {
  server: Server;
  /** The firewall as the agent read it, with Dockwell's own table. */
  fw: ServerFirewall | undefined;
  /** Reads the firewall again, once a change it opened is over. */
  onChanged?: () => void;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const changes = useChangeSheet();
  // What the sheet changed shows here at once, not at the next read.
  const sheet = { open: (opening: Opening) => changes.open({ ...opening, onClose: () => onChanged?.() }) };
  const [adding, setAdding] = useState(false);
  const usedBy = useUsedBy(fw);
  const table = fw?.dockwell;
  const name = server.name;
  const denies = table?.policy?.denyEverythingElse ?? false;
  const waiting = table?.waitingChange ?? "";
  const policyWaits = waiting !== "" && (table?.waitingPolicy?.denyEverythingElse ?? false) !== denies;
  const rows = rowsOf(table);
  if (table?.unusable) {
    const why = table.unusable;
    return (
      <Panel title={t`Dockwell's rules`} className="firewall-tool">
        <p className="firewall-note">
          <Trans>
            {name} can't hold Dockwell's firewall, so no rule or policy can be planned for it: {why}
          </Trans>
        </p>
      </Panel>
    );
  }
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
  const stateWords: Record<RuleState, string> = {
    kept: "",
    adding: t`Waiting for Keep`,
    changing: t`Change waiting for Keep`,
    removing: t`Removal waiting for Keep`,
  };
  const action = (r: DockwellFirewallRule) => (r.rule?.action === FirewallAction.DENY ? t`Deny` : t`Allow`);
  const portOf = (r: DockwellFirewallRule) => `${r.rule?.port}/${r.rule?.protocol === Protocol.UDP ? "udp" : "tcp"}`;
  const from = (r: DockwellFirewallRule) =>
    r.rule?.sources.length ? <code>{r.rule.sources.join(", ")}</code> : t`Anywhere`;
  const removeCell = (row: RuleRow) =>
    row.state === "kept" && !waiting ? (
      <Button variant="ghost" size="sm" onClick={() => remove(row.rule)} aria-label={t`Remove the rule ${row.id}`}>
        <Trans>Remove</Trans>
      </Button>
    ) : row.state !== "kept" ? (
      <Tag tone="cobalt" words>
        {stateWords[row.state]}
      </Tag>
    ) : null;
  const columns: TableColumn<RuleRow>[] = phone
    ? [
        {
          key: "rule",
          header: t`Rule`,
          render: ({ rule: r }) => (
            <span className="firewall-stack firewall-sub-lines">
              <span>
                <strong>{action(r)}</strong> <code>{portOf(r)}</code>
              </span>
              <span className="firewall-line firewall-sub">{from(r)}</span>
              {r.rule?.comment ? <span className="firewall-line firewall-sub">{r.rule.comment}</span> : null}
            </span>
          ),
        },
        { key: "remove", header: <span className="dw-sr">{t`Remove`}</span>, align: "right", render: removeCell },
      ]
    : [
        { key: "action", header: t`Action`, width: 96, render: ({ rule: r }) => <strong>{action(r)}</strong> },
        { key: "port", header: t`Port`, width: 112, mono: true, render: ({ rule: r }) => portOf(r) },
        { key: "from", header: t`From`, render: ({ rule: r }) => from(r) },
        { key: "used", header: t`Used by`, muted: true, render: ({ rule: r }) => usedBy(r.rule) },
        { key: "note", header: t`Note`, muted: true, render: ({ rule: r }) => r.rule?.comment ?? "" },
        {
          key: "remove",
          header: <span className="dw-sr">{t`Remove`}</span>,
          width: 168,
          align: "right",
          render: removeCell,
        },
      ];
  return (
    <Panel
      title={t`Dockwell's rules`}
      actions={
        <Button variant="secondary" size="sm" icon="plus" onClick={() => setAdding(true)} disabled={waiting !== ""}>
          <Trans>Add a rule</Trans>
        </Button>
      }
      flush
      className="firewall-tool"
    >
      <div className="firewall-policy">
        <Switch
          checked={policyWaits ? !denies : denies}
          onChange={setPolicy}
          disabled={waiting !== ""}
          label={policyWaits ? t`Deny everything else, waiting for Keep` : t`Deny everything else`}
          description={t`Drops what no rule lets in. Connections already open, SSH from where people come in now, and on the control server its own ports, stay let in.`}
        />
      </div>
      {waiting ? (
        <p className="firewall-note">
          <Trans>
            A change to this firewall waits for you to keep it, so nothing else here changes until it's kept or {name}{" "}
            puts it back.
          </Trans>
        </p>
      ) : null}
      {rows.length > 0 ? (
        <Table columns={columns} rows={rows} rowKey="id" caption={t`Dockwell's rules`} density="compact" />
      ) : denies ? (
        <p className="firewall-note">
          <Trans>
            Dockwell has no rules for {name}, and denies everything else: only connections already open, SSH from the
            addresses it keeps, and on the control server its own ports, get in.
          </Trans>
        </p>
      ) : (
        <p className="firewall-note">
          <Trans>Dockwell has no rules for {name} yet. What comes in is as the server's own firewall leaves it.</Trans>
        </p>
      )}
      {rows.length > 0 || denies ? (
        <p className="firewall-note">
          {denies ? (
            <Trans>
              As {name} starts, these are off until its agent is up, a few seconds in, and loads them again: for those
              seconds, everything they'd drop gets in. Nothing is lost, and nobody is locked out.
            </Trans>
          ) : (
            <Trans>
              As {name} starts, these are off until its agent is up, a few seconds in, and loads them again: for those
              seconds, what they deny gets in. Nothing is lost, and nobody is locked out.
            </Trans>
          )}
        </p>
      ) : null}
      {adding ? (
        <AddRuleSheet
          server={server}
          taken={rows.map((r) => r.id)}
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
          hint={t`Addresses or ranges, such as 203.0.113.0/24, separated by commas. Empty is anywhere.`}
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
