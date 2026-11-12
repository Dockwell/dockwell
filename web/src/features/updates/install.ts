// SPDX-License-Identifier: AGPL-3.0-only

import type { Timestamp } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type Change,
  ChangeService,
  ChangeState,
  type PackageInstall,
  PackageManager,
  type PackageUpdate,
  type Server,
  type ServerUpdates,
  StepState,
} from "@dockwell/api";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";

import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { followChange } from "../changes/follow";
import { isFinal } from "../changes/plan";

/**
 * What an install asks for (ADR 0064): packages by name with what they need, the
 * security fixes, or every update.
 */
export type InstallAsk = { names: string[] } | { all: boolean };

/** How long a row says what it updated to, at least, before it leaves. */
export const UPDATED_FOR_MS = 4_000;

/** Where one package is in an install, as its row shows it. */
export type RowState =
  | { kind: "idle" }
  /** Its plan is being made: the server's agent simulates the install. */
  | { kind: "planning" }
  /** In a plan the sheet shows, not applied yet. */
  | { kind: "reviewing" }
  /** In an install whose step hasn't started, as another change runs on the server. */
  | { kind: "waiting" }
  | { kind: "downloading" }
  /** dpkg is unpacking it, or the install runs and hasn't reached it yet. */
  | { kind: "installing" }
  /** Set up, while the install goes on with the rest. */
  | { kind: "installed" }
  /** The install that took it applied: it says what it went to, then leaves. */
  | { kind: "updated"; to: string }
  /** The install that took it failed. */
  | { kind: "failed" };

/** The packages a change installs on a server, as its plan says, or none. */
export function installOf(change: Change | undefined, serverId: string): PackageInstall | undefined {
  for (const r of change?.plan?.resources ?? []) {
    if (r.install && r.resource?.kind === "server.host" && r.resource.scope === serverId) return r.install;
  }
  return undefined;
}

/** What one line of an install says of a package: dpkg unpacking it, or setting it up. */
export function packageLine(line: string): { name: string; phase: "installing" | "installed" } | undefined {
  const set = /^Setting up ([^\s:]+)(?::\S+)? \(/.exec(line);
  if (set?.[1]) return { name: set[1], phase: "installed" };
  const unpack =
    /^Unpacking ([^\s:]+)(?::\S+)? \(/.exec(line) ?? /^Preparing to unpack \.\.\.\/(?:\d+-)?([^_/]+)_/.exec(line);
  if (unpack?.[1]) return { name: unpack[1], phase: "installing" };
  return undefined;
}

/**
 * The line of a failure that says why: apt's last E: line, or else the last line, of
 * what the agent kept of the install's output, its lines joined by "; ".
 */
export function lastLine(text: string): string {
  const lines = text
    .split(/\n|; |: (?=E: |dpkg: )/)
    .map((l) => l.trim())
    .filter(Boolean);
  return lines.filter((l) => l.startsWith("E: ")).at(-1) ?? lines.at(-1) ?? "";
}

function seconds(t: Timestamp | undefined): bigint {
  return t?.seconds ?? 0n;
}

interface Recent {
  pkg: PackageUpdate;
  to: string;
  until: number;
  /** When the read was that came before the install ended: the row stays until a later one is back. */
  readBefore: bigint;
}

interface Failure {
  id: string;
  ask: InstallAsk;
  names: Set<string>;
  why: string;
}

/**
 * Installing a server's updates from its Updates tab (ADR 0064): a plan for the
 * packages asked, then the install followed live, so each row can say where its
 * package is. An install that someone else started, in another tab or from the
 * CLI, is followed as well, as ServerUpdates names it. Once one applies, the
 * page reads the updates again, and each package it took says what it went to
 * for a moment before it leaves.
 */
export function useInstall(server: Server, updates: ServerUpdates | undefined, reread: () => void) {
  const transport = useTransport();
  const sheet = useChangeSheet();
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const may = useMay()(ChangeService.method.planInstallUpdates, { install: true });
  const offered = updates?.manager === PackageManager.APT;
  const [planning, setPlanning] = useState<InstallAsk>();
  const [own, setOwn] = useState<{ id: string; ask: InstallAsk }>();
  const [change, setChange] = useState<Change>();
  const [seen, setSeen] = useState<ReadonlyMap<string, "installing" | "installed">>(new Map());
  // Whether the install has gone past its download, as its lines say.
  const [started, setStarted] = useState(false);
  const [recent, setRecent] = useState<ReadonlyMap<string, Recent>>(new Map());
  const [failure, setFailure] = useState<Failure>();
  const [now, setNow] = useState(() => Date.now());
  // What has been dealt with once it ended, so a change still named by a read from before isn't again.
  const handled = useRef(new Set<string>());
  const latest = useRef({ updates, own, reread });
  latest.current = { updates, own, reread };

  const serverInstall = updates?.installChangeId ?? "";
  const followId = own?.id ?? (handled.current.has(serverInstall) ? "" : serverInstall);

  const ended = useCallback(
    (c: Change) => {
      if (handled.current.has(c.id)) return;
      handled.current.add(c.id);
      const { updates: u, own: mine, reread: again } = latest.current;
      const install = installOf(c, server.id);
      const packages = install?.packages ?? [];
      const ask: InstallAsk = mine?.id === c.id ? mine.ask : askOf(install);
      if (mine?.id === c.id) setOwn(undefined);
      setChange(undefined);
      setSeen(new Map());
      setStarted(false);
      if (c.state === ChangeState.APPLIED) {
        const until = Date.now() + UPDATED_FOR_MS;
        const readBefore = seconds(u?.readTime);
        setRecent((was) => {
          const next = new Map(was);
          for (const p of packages) next.set(p.name, { pkg: p, to: p.available, until, readBefore });
          return next;
        });
        setFailure(undefined);
        again();
      } else if (c.state === ChangeState.FAILED) {
        const step = c.steps.find((s) => s.state === StepState.FAILED);
        setFailure({
          id: c.id,
          ask,
          names: new Set(packages.map((p) => p.name)),
          why: lastLine(step?.detail || step?.message || c.error) || c.error,
        });
        again();
      }
    },
    [server.id],
  );

  // Follows the install named, live, until it ends.
  useEffect(() => {
    if (!followId) return;
    const abort = new AbortController();
    void followChange(transport, followId, abort.signal, (c) => {
      if (abort.signal.aborted) return;
      if (isFinal(c)) {
        ended(c);
        return;
      }
      setChange(c);
      const line = c.steps.find((s) => s.state === StepState.RUNNING)?.activity ?? "";
      const said = packageLine(line);
      if (said || /^(Installing \d|Following|Finishing)/.test(line)) setStarted(true);
      if (said) {
        setSeen((was) => {
          if (was.get(said.name) === said.phase || was.get(said.name) === "installed") return was;
          const next = new Map(was);
          next.set(said.name, said.phase);
          return next;
        });
      }
    });
    return () => {
      abort.abort();
      setChange(undefined);
      setSeen(new Map());
      setStarted(false);
    };
  }, [followId, transport, ended]);

  // A row that said what it went to leaves once its time is up and a read from after is back.
  useEffect(() => {
    if (recent.size === 0) return;
    const next = Math.min(...[...recent.values()].map((r) => r.until));
    const wait = Math.max(next - Date.now(), 0) + 50;
    const timer = setTimeout(() => setNow(Date.now()), wait);
    return () => clearTimeout(timer);
  }, [recent]);
  useEffect(() => {
    if (recent.size === 0) return;
    const read = seconds(updates?.readTime);
    const fresh = !updates?.checking;
    let changed = false;
    const next = new Map(recent);
    for (const [name, r] of recent) {
      if (now >= r.until && fresh && read > r.readBefore) {
        next.delete(name);
        changed = true;
      }
    }
    if (changed) setRecent(next);
  }, [now, recent, updates?.readTime, updates?.checking]);

  const plan = useCallback(
    (ask: InstallAsk) => {
      setPlanning(ask);
      setFailure(undefined);
      sheet.open({
        kind: "plan",
        make: async () => {
          try {
            const res = await client.planInstallUpdates({
              serverIds: [server.id],
              ...("names" in ask ? { packages: [...ask.names].sort() } : { all: ask.all }),
            });
            if (res.change) setOwn({ id: res.change.id, ask });
            return res;
          } finally {
            setPlanning(undefined);
          }
        },
        onClose: (c) => {
          // A plan closed unapplied is discarded; one applying goes on, followed.
          if (!c || c.state === ChangeState.PLANNED || c.state === ChangeState.DISCARDED) {
            setOwn((was) => (was && (!c || was.id === c.id) ? undefined : was));
          }
        },
      });
    },
    [client, server.id, sheet],
  );

  const open = useCallback((id: string) => sheet.open({ kind: "change", id }), [sheet]);

  const install = installOf(change, server.id);
  const inInstall = new Set((install?.packages ?? []).map((p) => p.name));
  const applying = change?.state === ChangeState.APPLYING;
  const step = change?.steps.find((s) => s.resource?.kind === "server.host" && s.serverId === server.id);
  const activity = step?.state === StepState.RUNNING ? step.activity : "";
  const downloading = step?.state === StepState.RUNNING && !started;
  // Another install runs here: one followed that applies, or one the server names that isn't followed yet.
  const busy = applying || (serverInstall !== "" && !handled.current.has(serverInstall) && !change);
  const planningNames = planning && "names" in planning ? new Set(planning.names) : undefined;

  const stateOf = (name: string): RowState => {
    const r = recent.get(name);
    if (r) {
      const read = seconds(updates?.readTime);
      if (now < r.until || updates?.checking || read <= r.readBefore) return { kind: "updated", to: r.to };
    }
    if (planningNames?.has(name)) return { kind: "planning" };
    if (change && inInstall.has(name)) {
      if (change.state === ChangeState.PLANNED) return { kind: "reviewing" };
      if (!step || step.state === StepState.QUEUED || step.state === StepState.WAITING) return { kind: "waiting" };
      const phase = seen.get(name);
      if (phase === "installed") return { kind: "installed" };
      if (phase === "installing") return { kind: "installing" };
      return downloading ? { kind: "downloading" } : { kind: "installing" };
    }
    if (failure?.names.has(name)) return { kind: "failed" };
    return { kind: "idle" };
  };

  /** Rows that left the read but still say what they went to, by name. */
  const leaving = [...recent.values()].filter((r) => !updates?.packages.some((p) => p.name === r.pkg.name));

  return {
    offered,
    may,
    plan,
    open,
    planning,
    /** The install followed, while it's planned or applies. */
    change,
    install,
    activity,
    downloading,
    /** True while an install runs on the server. */
    busy,
    failure,
    leaving,
    stateOf,
  };
}

/** What a change someone else made asked for, to try again: its packages by name, or all. */
function askOf(install: PackageInstall | undefined): InstallAsk {
  if (install?.asked.length) return { names: [...install.asked] };
  const upgrades = (install?.packages ?? []).filter((p) => p.installed !== "").map((p) => p.name);
  return upgrades.length > 0 ? { names: upgrades } : { names: (install?.packages ?? []).map((p) => p.name) };
}

export type Install = ReturnType<typeof useInstall>;
