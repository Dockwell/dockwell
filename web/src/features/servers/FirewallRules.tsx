// SPDX-License-Identifier: AGPL-3.0-only

import { equals, fromJson, type JsonValue, type MessageInitShape, toJson } from "@bufbuild/protobuf";
import { useQuery } from "@connectrpc/connect-query";
import {
  ChangeService,
  Connection,
  type DockwellFirewallRule,
  type DockwellTable,
  type EditSchema,
  FirewallAction,
  type FirewallRule,
  FirewallRuleSchema,
  FirewallService,
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
import { ownEntries } from "../apps/PrivateSheet";
import { useMay } from "../auth/may";
import { type Opening, useChangeSheet } from "../changes/ChangeSheet";
import { useReadAgainOnceOver } from "../changes/follow";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { ListPager, type ListPlace, sortableColumns, useRowMenu } from "../shell/ListTools";
import { pagedRows, sortedRows } from "../shell/list";
import { sourceContainsAddress } from "./server";

type Edit = MessageInitShape<typeof EditSchema>;

/** A rule's resource on its server. */
function ruleRef(server: Server, name: string) {
  return { kind: "firewall.rule", scope: server.id, name };
}

export function policyEdit(
  server: Server,
  denyEverythingElse: boolean,
  guardDockerPorts: boolean,
  guardDockerUdp: boolean,
): Edit {
  const resource = { kind: "firewall.policy", scope: server.id, name: "default" };
  return denyEverythingElse || guardDockerPorts
    ? {
        resource,
        spec: { kind: { case: "firewallPolicy", value: { denyEverythingElse, guardDockerPorts, guardDockerUdp } } },
      }
    : { resource, delete: true };
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

/**
 * What uses a port on the server: the containers Docker publishes it for, SSH, or
 * what else listens there. Ports are by protocol; one with no protocol is either.
 */
export function usePortUser(
  server: Server,
  fw: ServerFirewall | undefined,
): (ports: readonly { port: number; protocol?: string }[]) => string {
  const { t } = useLingui();
  // SSH's ports, as the SSH card reads them, so its rule can name it.
  const ssh = useQuery(
    FirewallService.method.getServerSsh,
    { serverId: server.id },
    { enabled: server.connection === Connection.CONNECTED, refetchInterval: 30_000, retry: false },
  );
  const sshPorts = ssh.data?.ssh?.ports ?? [];
  return (ports) => {
    if (!fw || ports.length === 0) return "";
    const on = (p: { port: number; protocol?: string }, port: number, protocol: string) =>
      p.port === port && (!p.protocol || p.protocol === protocol);
    const users: string[] = [];
    let listens = false;
    for (const p of ports) {
      users.push(...fw.published.filter((pub) => on(p, pub.hostPort, pub.protocol)).map((pub) => pub.container));
      if (sshPorts.includes(p.port) && p.protocol !== "udp") users.push(t`SSH`);
      if (fw.listening.some((l) => on(p, l.port, l.protocol))) listens = true;
    }
    if (users.length > 0) return [...new Set(users)].join(", ");
    return listens ? t`The server itself` : t`Nothing listens`;
  };
}

function useUsedBy(server: Server, fw: ServerFirewall | undefined): (rule: FirewallRule | undefined) => string {
  const user = usePortUser(server, fw);
  return (rule) =>
    rule?.port ? user([{ port: rule.port, protocol: rule.protocol === Protocol.UDP ? "udp" : "tcp" }]) : "";
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
  yourAddress,
  onChanged,
  place,
}: {
  server: Server;
  place: ListPlace;
  /** The firewall as the agent read it, with Dockwell's own table. */
  fw: ServerFirewall | undefined;
  yourAddress: string;
  /** Reads the firewall again, once a change it opened is over. */
  onChanged?: () => void;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const changes = useChangeSheet();
  // What the sheet changed shows here once it's on the server, not at the next read.
  const readAgain = useReadAgainOnceOver(() => onChanged?.());
  const sheet = { open: (opening: Opening) => changes.open({ ...opening, onClose: readAgain }) };
  const [editing, setEditing] = useState<DockwellFirewallRule | "new">();
  const [selected, setSelected] = useState<ReadonlySet<string>>(new Set());
  const [fileError, setFileError] = useState<string>();
  const fileInput = useRef<HTMLInputElement>(null);
  const usedBy = useUsedBy(server, fw);
  const table = fw?.dockwell;
  const name = server.name;
  const denies = table?.policy?.denyEverythingElse ?? false;
  const waiting = table?.waitingChange ?? "";
  const policyWaits = waiting !== "" && (table?.waitingPolicy?.denyEverythingElse ?? false) !== denies;
  const rows = rowsOf(table);
  const exportable = (table?.rules ?? []).filter((r) => r.rule && !reservedRule(r.name));
  const picked = rows.filter(({ id, state }) => selected.has(id) && state === "kept");
  const locked = waiting !== "";
  const toggle = (id: string) =>
    setSelected((previous) => {
      const next = new Set(previous);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  const context = useRowMenu<RuleRow>();
  const mayPlan = useMay()(ChangeService.method.proposeChange, { install: true });
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
      edits: [policyEdit(server, on, table?.policy?.guardDockerPorts ?? false, table?.policy?.guardDockerUdp ?? false)],
    });
  const remove = (rule: DockwellFirewallRule) =>
    sheet.open({
      kind: "propose",
      summary: t`Remove the rule ${rule.name} from ${name}`,
      edits: [{ resource: ruleRef(server, rule.name), delete: true }],
    });
  const removePicked = () => {
    const count = picked.length;
    sheet.open({
      kind: "propose",
      summary: t`Remove ${count} firewall rules from ${name}`,
      edits: picked.map(({ id }) => ({ resource: ruleRef(server, id), delete: true })),
    });
    setSelected(new Set());
  };
  const importFile = async (file: File) => {
    setFileError(undefined);
    try {
      if (file.size > 256 * 1024) throw new Error();
      const rules = readRuleFile(await file.text(), yourAddress);
      const count = rules.length;
      const used = rows.map(({ id }) => id);
      const edits = rules.map(({ name: imported, rule }) => {
        let target = imported;
        for (let i = 2; used.includes(target); i++) target = `${imported.slice(0, 52)}-${i}`;
        used.push(target);
        const edit: Edit = {
          resource: ruleRef(server, target),
          mustCreate: true,
          spec: { kind: { case: "firewallRule", value: rule } },
        };
        return edit;
      });
      sheet.open({ kind: "propose", summary: t`Import ${count} firewall rules on ${name}`, edits });
    } catch (error) {
      if (error instanceof RuleFileError) {
        const rule = error.rule;
        setFileError(
          error.reason === "reserved"
            ? t`The name ${rule} is reserved for database external access. Rename the rule, or set access on the database page.`
            : t`The deny rule ${rule} covers your current address. Leave your address out of its sources and import again.`,
        );
        return;
      }
      setFileError(
        t`This file isn't a Dockwell firewall export. Choose a JSON export under 256 KB with up to 200 rules.`,
      );
    }
  };
  const stateWords: Record<RuleState, string> = {
    kept: "",
    adding: t`Waiting for Keep`,
    changing: t`Change waiting for Keep`,
    removing: t`Removal waiting for Keep`,
  };
  const action = (r: DockwellFirewallRule) => (r.rule?.action === FirewallAction.DENY ? t`Deny` : t`Allow`);
  const portOf = (r: DockwellFirewallRule) => {
    const protocol = r.rule?.protocol === Protocol.UDP ? "udp" : "tcp";
    if (r.rule?.port) return `${r.rule.port}/${protocol}`;
    return r.rule?.protocol ? t`All ${protocol} ports` : t`All ports`;
  };
  const from = (r: DockwellFirewallRule) =>
    r.rule?.sources.length ? <code>{r.rule.sources.join(", ")}</code> : t`Anywhere`;
  const keys = ["action", "port", "from", "note", "rule"];
  const page = pagedRows(
    sortedRows(
      rows.filter(({ rule }) =>
        `${rule.name} ${action(rule)} ${portOf(rule)} ${rule.rule?.sources.join(" ") ?? ""} ${rule.rule?.comment ?? ""}`
          .toLowerCase()
          .includes(place.query),
      ),
      place.sort,
      ({ rule }, key) => {
        if (key === "action") return action(rule);
        if (key === "port") return rule.rule?.port;
        if (key === "from") return rule.rule?.sources.join(", ");
        if (key === "note") return rule.rule?.comment;
        return rule.name;
      },
    ),
    place.page,
  );
  const hasMenu = (row: RuleRow) => row.state === "kept" && !waiting && mayPlan.allowed;
  const menuLabel = (row: RuleRow) => t`Actions for ${row.id}`;
  const actionsFor = (row: RuleRow): MenuAction[] => [
    ...(!reservedRule(row.id) ? [{ id: "edit", label: t`Edit`, onSelect: () => setEditing(row.rule) }] : []),
    { id: "remove", label: t`Remove`, icon: "trash", danger: true, onSelect: () => remove(row.rule) },
  ];
  const removeCell = (row: RuleRow) =>
    row.state === "kept" && !waiting ? (
      <span className="list-row-actions">
        {!reservedRule(row.id) ? (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setEditing(row.rule)}
            aria-label={t`Edit the rule ${row.id}`}
            {...mayPlan.off}
          >
            <Trans>Edit</Trans>
          </Button>
        ) : null}
        <Button
          variant="ghost"
          size="sm"
          onClick={() => remove(row.rule)}
          aria-label={t`Remove the rule ${row.id}`}
          {...mayPlan.off}
        >
          <Trans>Remove</Trans>
        </Button>
        {hasMenu(row) ? <ActionMenu label={menuLabel(row)} actions={actionsFor(row)} /> : null}
      </span>
    ) : row.state !== "kept" ? (
      <Tag tone="cobalt" words>
        {stateWords[row.state]}
      </Tag>
    ) : null;
  const dataColumns: TableColumn<RuleRow>[] = phone
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
          width: 200,
          align: "right",
          render: removeCell,
        },
      ];
  const selection: TableColumn<RuleRow> = {
    key: "selected",
    width: 40,
    header: <span className="dw-sr">{t`Select rules`}</span>,
    render: ({ id, state }) =>
      state === "kept" ? (
        <input
          type="checkbox"
          checked={selected.has(id)}
          disabled={locked}
          aria-label={t`Select the rule ${id}`}
          onChange={() => toggle(id)}
        />
      ) : null,
  };
  const columns = mayPlan.allowed ? [selection, ...dataColumns] : dataColumns;
  return (
    <Panel
      title={t`Dockwell's rules`}
      actions={
        <div className="firewall-rule-tools">
          <Button variant="ghost" size="sm" onClick={() => saveRuleFile(exportable)} disabled={exportable.length === 0}>
            <Trans>Export</Trans>
          </Button>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => fileInput.current?.click()}
            {...mayPlan.off}
            disabled={locked || !mayPlan.allowed}
          >
            <Trans>Import</Trans>
          </Button>
          <Button
            variant="secondary"
            size="sm"
            icon="plus"
            onClick={() => setEditing("new")}
            {...mayPlan.off}
            disabled={locked || !mayPlan.allowed}
          >
            <Trans>Add a rule</Trans>
          </Button>
        </div>
      }
      flush
      className="firewall-tool"
    >
      <input
        ref={fileInput}
        type="file"
        accept=".json,application/json"
        hidden
        onChange={(event) => {
          const file = event.currentTarget.files?.[0];
          event.currentTarget.value = "";
          if (file && !locked && mayPlan.allowed) void importFile(file);
        }}
      />
      {fileError ? <Callout tone="critical">{fileError}</Callout> : null}
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
        <>
          {picked.length > 0 ? (
            <div className="firewall-policy">
              <Button
                variant="danger"
                size="sm"
                onClick={removePicked}
                {...mayPlan.off}
                disabled={locked || !mayPlan.allowed}
              >
                {t`Remove selected (${picked.length})`}
              </Button>
            </div>
          ) : null}
          <Table
            columns={sortableColumns(columns, place, keys)}
            rows={page.rows}
            rowKey="id"
            caption={t`Dockwell's rules`}
            density="compact"
            rowHasContextMenu={hasMenu}
            onRowContextMenu={(rule, at, row) =>
              context.open(rule, at, row, { label: menuLabel(rule), actions: actionsFor(rule) })
            }
          />
          <ListPager page={page} onPage={place.setPage} />
          {context.element}
        </>
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
      {editing ? (
        <RuleSheet
          server={server}
          fw={fw}
          yourAddress={yourAddress}
          taken={rows.map((r) => r.id)}
          existing={editing === "new" ? undefined : editing}
          onClose={() => setEditing(undefined)}
          open={sheet.open}
        />
      ) : null}
    </Panel>
  );
}

function reservedRule(name: string): boolean {
  return name.startsWith("external-");
}

export function writeRuleFile(rules: readonly DockwellFirewallRule[]): string {
  const contents = rules.flatMap(({ name, rule }) =>
    rule && !reservedRule(name) ? [{ name, rule: toJson(FirewallRuleSchema, rule) }] : [],
  );
  return JSON.stringify({ version: 1, rules: contents }, null, 2);
}

function saveRuleFile(rules: readonly DockwellFirewallRule[]) {
  const url = URL.createObjectURL(new Blob([writeRuleFile(rules)], { type: "application/json" }));
  const link = document.createElement("a");
  link.href = url;
  link.download = "dockwell-firewall-rules.json";
  link.click();
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

class RuleFileError extends Error {
  constructor(
    readonly reason: "reserved" | "own_address",
    readonly rule: string,
  ) {
    super(`${reason}: ${rule}`);
  }
}

export function readRuleFile(text: string, yourAddress = ""): { name: string; rule: FirewallRule }[] {
  const data: unknown = JSON.parse(text);
  if (
    !isObject(data) ||
    data["version"] !== 1 ||
    !Array.isArray(data["rules"]) ||
    data["rules"].length === 0 ||
    data["rules"].length > 200
  ) {
    throw new Error("Expected a version 1 Dockwell firewall export");
  }
  const names = new Set<string>();
  return data["rules"].map((entry: unknown) => {
    if (
      !isObject(entry) ||
      typeof entry["name"] !== "string" ||
      !/^[A-Za-z0-9 _.:/@-]{1,95}$/.test(entry["name"]) ||
      !entry["name"].trim() ||
      names.has(entry["name"]) ||
      !isObject(entry["rule"])
    ) {
      throw new Error("Expected a unique rule name and a firewall rule");
    }
    const name = entry["name"];
    if (reservedRule(name)) throw new RuleFileError("reserved", name);
    names.add(name);
    const rule = fromJson(FirewallRuleSchema, entry["rule"] as JsonValue, { ignoreUnknownFields: false });
    if (
      rule.action === FirewallAction.DENY &&
      rule.sources.some((source) => sourceContainsAddress(source, yourAddress))
    ) {
      throw new RuleFileError("own_address", name);
    }
    return { name, rule };
  });
}

/** A name for a rule that no other on the server has, such as allow-443, or allow-443-udp. */
export function ruleName(action: "allow" | "deny", port: number, udp: boolean, taken: readonly string[]): string {
  const base = `${action}-${port || "address"}${udp ? "-udp" : ""}`;
  let name = base;
  for (let n = 2; taken.includes(name); n++) name = `${base}-${n}`;
  return name;
}

/** A port that answers beyond the server, as a rule could name it: what listens there, and the protocol. */
interface Suggested {
  port: number;
  protocol: "tcp" | "udp";
  user: string;
}

/**
 * The ports a new rule is likely for: what listens beyond loopback and what Docker
 * publishes, each once, without those Dockwell has a rule for already.
 */
export function suggestedPorts(
  fw: ServerFirewall | undefined,
  ruled: readonly { port: number; protocol: string }[],
  user: (ports: readonly { port: number; protocol?: string }[]) => string,
  generic: readonly string[] = [],
): Suggested[] {
  if (!fw) return [];
  const loopback = (address: string) => address.startsWith("127.") || address === "::1";
  const seen = new Set(ruled.map((r) => `${r.port}/${r.protocol}`));
  const out: Suggested[] = [];
  const add = (port: number, protocol: string) => {
    const key = `${port}/${protocol}`;
    if (seen.has(key) || (protocol !== "tcp" && protocol !== "udp")) return;
    seen.add(key);
    // What uses it, where that's an app or SSH; "the server itself" on every chip says nothing.
    const who = user([{ port, protocol }]);
    out.push({ port, protocol, user: generic.includes(who) ? "" : who });
  };
  for (const p of fw.published) if (!loopback(p.hostIp)) add(p.hostPort, p.protocol);
  for (const l of fw.listening) if (!loopback(l.address)) add(l.port, l.protocol);
  return out.sort((a, b) => a.port - b.port).slice(0, 8);
}

/** A new rule: what it does, to which port, from where, planned as a change. */
function RuleSheet({
  server,
  fw,
  yourAddress,
  taken,
  existing,
  onClose,
  open,
}: {
  server: Server;
  fw: ServerFirewall | undefined;
  yourAddress: string;
  taken: readonly string[];
  existing: DockwellFirewallRule | undefined;
  onClose: () => void;
  open: (opening: Opening) => void;
}) {
  const { t } = useLingui();
  const field = useRef<HTMLInputElement>(null);
  const rule = existing?.rule;
  const [action, setAction] = useState<"allow" | "deny">(rule?.action === FirewallAction.DENY ? "deny" : "allow");
  const [protocol, setProtocol] = useState<"tcp" | "udp" | "all">(
    rule?.protocol === Protocol.UDP ? "udp" : rule?.protocol === Protocol.TCP || rule?.port ? "tcp" : "all",
  );
  const [port, setPort] = useState(rule?.port ? String(rule.port) : "");
  const [from, setFrom] = useState(rule?.sources.join(", ") ?? "");
  const [note, setNote] = useState(rule?.comment ?? "");
  const [error, setError] = useState<string>();
  const user = usePortUser(server, fw);
  const ruled = (fw?.dockwell?.rules ?? []).flatMap((r) =>
    r.rule ? [{ port: r.rule.port, protocol: r.rule.protocol === Protocol.UDP ? "udp" : "tcp" }] : [],
  );
  const suggested = suggestedPorts(fw, ruled, user, [t`The server itself`, t`Nothing listens`]);
  // Your own address, as the control plane saw this request, to let in.
  const mine = ownEntries(yourAddress)[0];
  const listed = from
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean);
  const pick = (s: Suggested) => {
    setPort(String(s.port));
    setProtocol(s.protocol);
    setError(undefined);
  };
  const submit = (event?: FormEvent) => {
    event?.preventDefault();
    const n = Number(port.trim());
    if (port.trim() && (!Number.isInteger(n) || n < 1 || n > 65535)) {
      setError(t`Name a port from 1 to 65535, such as 443.`);
      field.current?.focus();
      return;
    }
    const sources = listed;
    if (!n && sources.length === 0) {
      setError(t`Enter an address or range, or choose a port.`);
      return;
    }
    if (action === "deny" && sources.some((source) => sourceContainsAddress(source, yourAddress))) {
      setError(t`This range covers your current address. Choose a source that leaves yours out.`);
      return;
    }
    const name = existing?.name ?? ruleName(action, n, protocol === "udp", taken);
    const portProtocol = protocol === "all" && n ? "tcp" : protocol;
    const edit: Edit = {
      resource: ruleRef(server, name),
      mustCreate: !existing,
      spec: {
        kind: {
          case: "firewallRule",
          value: {
            action: action === "deny" ? FirewallAction.DENY : FirewallAction.ALLOW,
            protocol:
              portProtocol === "udp" ? Protocol.UDP : portProtocol === "tcp" ? Protocol.TCP : Protocol.UNSPECIFIED,
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
      summary: existing
        ? t`Edit the rule ${name} on ${serverName}`
        : n
          ? action === "deny"
            ? t`Deny ${n}/${portProtocol} on ${serverName}`
            : t`Allow ${n}/${portProtocol} on ${serverName}`
          : action === "deny"
            ? t`Deny addresses on ${serverName}`
            : t`Allow addresses on ${serverName}`,
      edits: [edit],
    });
  };
  return (
    <Sheet
      open
      initialFocusRef={field}
      title={existing ? t`Edit the rule ${existing.name}` : t`Add a rule to ${server.name}`}
      subtitle={t`It stays only once you confirm you can still reach the server`}
      onClose={onClose}
      footer={
        <>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" icon="commit" onClick={() => submit()}>
            <Trans>Review plan</Trans>
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
        <div className="firewall-add-port">
          <TextField
            ref={field}
            label={t`Port`}
            hint={t`Leave empty to cover every port from the addresses below.`}
            mono
            inputMode="numeric"
            placeholder="443"
            value={port}
            onChange={(e) => {
              setPort(e.target.value);
              if (e.target.value.trim() && protocol === "all") setProtocol("tcp");
              setError(undefined);
            }}
          />
          {suggested.length > 0 ? (
            <ul className="firewall-suggest" aria-label={t`What answers on ${server.name}`}>
              {suggested.map((s) => {
                const on = port.trim() === String(s.port) && protocol === s.protocol;
                const what = `${s.port}/${s.protocol}`;
                return (
                  <li key={what}>
                    <button type="button" className="firewall-suggest-chip" aria-pressed={on} onClick={() => pick(s)}>
                      <code>{what}</code>
                      {s.user ? <span>{s.user}</span> : null}
                    </button>
                  </li>
                );
              })}
            </ul>
          ) : null}
        </div>
        <SelectField
          label={t`Protocol`}
          value={protocol}
          onChange={(e) => setProtocol(e.target.value as "tcp" | "udp" | "all")}
          options={[
            { value: "tcp", label: "TCP" },
            { value: "udp", label: "UDP" },
            ...(!port.trim() ? [{ value: "all", label: t`All protocols` }] : []),
          ]}
        />
        <TextField
          label={t`From`}
          mono
          autoCapitalize="none"
          autoCorrect="off"
          spellCheck={false}
          placeholder="203.0.113.0/24"
          value={from}
          hint={
            port.trim()
              ? t`Addresses or ranges, separated by commas. Empty lets anyone in.`
              : t`Addresses or ranges, separated by commas.`
          }
          onChange={(e) => setFrom(e.target.value)}
          action={
            action === "allow" && mine && !listed.includes(mine) ? (
              <Button size="sm" variant="ghost" onClick={() => setFrom([...listed, mine].join(", "))}>
                <Trans>Use my address</Trans>
              </Button>
            ) : undefined
          }
        />
        <TextField
          label={t`Note`}
          value={note}
          optional
          placeholder={t`Office VPN`}
          onChange={(e) => setNote(e.target.value)}
        />
      </form>
    </Sheet>
  );
}
