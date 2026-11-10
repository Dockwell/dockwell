// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import {
  ChangeService,
  type Fail2banBan,
  type Fail2banJail,
  Fail2banState,
  FirewallService,
  type HostListener,
  ListenerReach,
  type Server,
  type ServerSsh,
  SshAnswer,
  SshLoaded,
  type SshMatch,
  SshRootLogin,
  type SshSource,
} from "@dockwell/api";
import { Button, Callout, Metric, Panel, SelectField, Sheet, StatusBadge, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, type ReactNode, useRef, useState } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { useTimeWords } from "../../lib/time";
import { type Offer, useMay } from "../auth/may";
import { type Opening, useChangeSheet } from "../changes/ChangeSheet";
import { CopyButton } from "./CopyButton";
import { useControlPlaneNow } from "./useFleet";

/** How often the tab reads SSH again while it's open. */
const SSH_EVERY_MS = 30_000;

/** How often it reads fail2ban again: a few fail2ban-client runs for each jail. */
const FAIL2BAN_EVERY_MS = 60_000;

/** How many of the addresses banned now the card lists. */
const BANS_SHOWN = 8;

/** A file's name, without its folder, as the card says where a value is set. */
function fileName(path: string): string {
  return path.slice(path.lastIndexOf("/") + 1);
}

/** Where a value is set: a file and line, or sshd's default where no file sets it. */
function SetIn({ source, known = true }: { source: SshSource | undefined; known?: boolean }) {
  // A value Dockwell can't tell has no line to name, and isn't sshd's default either.
  if (!known) return null;
  if (source?.changed) {
    const file = fileName(source.file);
    const line = source.line;
    return (
      <span className="firewall-set-in is-waiting" title={source.file}>
        <Trans>
          {file}, line {line} · not in effect until sshd reloads
        </Trans>
      </span>
    );
  }
  if (!source) {
    return (
      <span className="firewall-set-in">
        <Trans>sshd's default</Trans>
      </span>
    );
  }
  const file = fileName(source.file);
  const line = source.line;
  return (
    <span className="firewall-set-in" title={source.file}>
      <Trans>
        {file}, line {line}
      </Trans>
    </span>
  );
}

/**
 * SSH on a server, as Server-Firewall.dc.html's SSH card has it, read-only for
 * now: whether a password and root sign in, its port and how far it reaches,
 * as sshd's files say, with the file that sets each, whether the sshd that
 * listens has read them, and what may set them otherwise (ADR 0052).
 */
export function SshPanel({
  server,
  connected,
  listening,
}: {
  server: Server;
  connected: boolean;
  /** What listens on the server, from the firewall's read, for how far SSH reaches. */
  listening: readonly HostListener[] | undefined;
}) {
  const { t } = useLingui();
  const read = useQuery(
    FirewallService.method.getServerSsh,
    { serverId: server.id },
    { enabled: connected, refetchInterval: SSH_EVERY_MS, retry: false },
  );
  const ssh = read.data?.ssh;
  const name = server.name;
  let body: ReactNode;
  if (read.error) {
    body =
      reasonOf(read.error) === "agent_outdated" ? (
        <p className="firewall-note">
          <Trans>
            The agent on {name} is older than the control plane, so it can't read SSH. Run the install script on {name}{" "}
            again to update it.
          </Trans>
        </p>
      ) : (
        <p className="firewall-note">{sentence(read.error)}</p>
      );
  } else if (!ssh) {
    body = (
      <p className="firewall-note" aria-busy="true">
        <Trans>Reading how SSH lets people in on {name}</Trans>
      </p>
    );
  } else if (ssh.unreadable) {
    body = <p className="firewall-note">{ssh.unreadable}</p>;
  } else {
    body = <SshValues ssh={ssh} listening={listening} />;
  }
  return (
    <Panel
      title={t`SSH`}
      actions={ssh?.version ? <span className="firewall-version">{ssh.version}</span> : undefined}
      className="firewall-ssh"
    >
      {body}
    </Panel>
  );
}

function SshValues({ ssh, listening }: { ssh: ServerSsh; listening: readonly HostListener[] | undefined }) {
  const { t } = useLingui();
  const source = (...keywords: string[]) => ssh.setIn.find((s) => keywords.includes(s.keyword));
  // What turns passwords on is whichever of the two says yes; what keeps them
  // off is PasswordAuthentication.
  const passwordSource =
    ssh.passwordLogin === SshAnswer.YES
      ? ssh.setIn.find(
          (s) =>
            s.value.toLowerCase() === "yes" &&
            ["PasswordAuthentication", "KbdInteractiveAuthentication", "ChallengeResponseAuthentication"].includes(
              s.keyword,
            ),
        )
      : source("PasswordAuthentication");
  const root = {
    [SshRootLogin.YES]: t`Yes`,
    [SshRootLogin.KEYS_ONLY]: t`Keys only`,
    [SshRootLogin.FORCED_COMMANDS_ONLY]: t`Forced commands only`,
    [SshRootLogin.NO]: t`No`,
    [SshRootLogin.UNSPECIFIED]: t`Can't tell`,
  }[ssh.rootLogin];
  const ports = ssh.ports.length > 0 ? ssh.ports.join(", ") : t`Can't tell`;
  const password = { [SshAnswer.YES]: t`On`, [SshAnswer.NO]: t`Off`, [SshAnswer.UNSPECIFIED]: t`Can't tell` }[
    ssh.passwordLogin
  ];
  const portSources = ssh.setIn.filter((s) => s.keyword === "Port");
  return (
    <div className="firewall-ssh-body">
      <SshLoadedNote ssh={ssh} />
      <dl className="firewall-facts">
        <div>
          <dt>
            <Trans>Password login</Trans>
          </dt>
          <dd>{password}</dd>
          <dd>
            <SetIn source={passwordSource} known={ssh.passwordLogin !== SshAnswer.UNSPECIFIED} />
          </dd>
        </div>
        <div>
          <dt>
            <Trans>Root login</Trans>
          </dt>
          <dd>{root}</dd>
          <dd>
            <SetIn source={source("PermitRootLogin")} known={ssh.rootLogin !== SshRootLogin.UNSPECIFIED} />
          </dd>
        </div>
        <div>
          <dt>{plural(ssh.ports.length, { one: "Port", other: "Ports" })}</dt>
          <dd className="is-mono">{ports}</dd>
          <dd>
            <SetIn source={portSources[0]} known={ssh.ports.length > 0} />
          </dd>
        </div>
        <div>
          <dt>
            <Trans>Reachable from</Trans>
          </dt>
          <dd>
            <ReachableFrom ports={ssh.ports} listening={listening} />
          </dd>
        </div>
      </dl>
      <SshNotes ssh={ssh} />
    </div>
  );
}

/**
 * What the card says of its own values, always: they're what sshd's files say,
 * and the sshd that listens uses what it read when it last started or
 * reloaded. It never says sshd has read them: nothing Dockwell reads of files
 * and times can prove that, so what it finds is only ever a warning. A file
 * that changed after sshd last wrote its PID file is one, with the files and
 * how to reload; a listener that isn't run from these files is another
 * (ADR 0052).
 */
function SshLoadedNote({ ssh }: { ssh: ServerSsh }) {
  const { t } = useLingui();
  const files = ssh.changedFiles.map(fileName).join(", ");
  const command = ssh.reloadCommand;
  return (
    <>
      <p className="firewall-note">
        <Trans>These are what sshd's files say; sshd uses what it read when it last started or reloaded.</Trans>
      </p>
      {ssh.loaded === SshLoaded.STALE ? (
        <Callout tone="warning" title={t`sshd hasn't read ${files} since it changed`}>
          <Trans>Until it reloads, it may still accept what this says is off.</Trans>
          {command ? (
            <span className="firewall-command">
              <code>{command}</code>
              <CopyButton text={command} label={t`Copy ${command}`} />
            </span>
          ) : null}
        </Callout>
      ) : null}
      {ssh.loaded === SshLoaded.APART ? (
        <Callout tone="warning" title={t`The sshd that listens may not use these files`}>
          {ssh.loadedWhy}
        </Callout>
      ) : null}
    </>
  );
}

/** How far SSH's port reaches past the firewalls here, as the firewall's read says. */
function ReachableFrom({
  ports,
  listening,
}: {
  ports: readonly number[];
  listening: readonly HostListener[] | undefined;
}) {
  const { t } = useLingui();
  if (!listening) return <span className="firewall-set-in">…</span>;
  const found = listening.filter((l) => l.protocol === "tcp" && ports.includes(l.port));
  // The narrowest reach of SSH's sockets, so it never reads as more open than one is.
  const order = [ListenerReach.UNKNOWN, ListenerReach.CLOSED, ListenerReach.SOME, ListenerReach.OPEN];
  const narrowest = found.sort((a, b) => order.indexOf(a.reach) - order.indexOf(b.reach))[0];
  if (!narrowest || narrowest.reach === ListenerReach.UNKNOWN || narrowest.reach === ListenerReach.UNSPECIFIED) {
    return <>{t`Can't tell`}</>;
  }
  if (narrowest.reach === ListenerReach.OPEN) return <>{t`Anywhere`}</>;
  if (narrowest.reach === ListenerReach.CLOSED) return <>{t`Nowhere`}</>;
  const where = [...narrowest.sources, ...narrowest.interfaces.map((iface) => t`on ${iface}`)].join(", ");
  return <span className="is-mono">{where}</span>;
}

/** A Match block: what it matches, where it is, and which of the card's values it sets. */
function MatchLine({ match }: { match: SshMatch }) {
  const criteria = match.criteria;
  const file = fileName(match.file);
  const line = match.line;
  const sets = match.sets.join(", ");
  return sets ? (
    <Trans>
      <code>Match {criteria}</code> · {file}, line {line} · sets {sets}
    </Trans>
  ) : (
    <Trans>
      <code>Match {criteria}</code> · {file}, line {line}
    </Trans>
  );
}

/** What else decides who signs in: Match blocks, who may, ways in Dockwell can't check, and where it would write. */
function SshNotes({ ssh }: { ssh: ServerSsh }) {
  const notes: ReactNode[] = [];
  if (ssh.accessLists.length > 0) {
    const lists = ssh.accessLists.join("; ");
    notes.push(<Trans key="access">sshd lets in only who these allow: {lists}.</Trans>);
  }
  if (ssh.unchecked.length > 0) {
    const ways = ssh.unchecked.join(", ");
    notes.push(<Trans key="unchecked">{ways} also let people in, which Dockwell can't check.</Trans>);
  }
  if (ssh.socketUnit) {
    const unit = ssh.socketUnit;
    notes.push(<Trans key="socket">systemd holds SSH's port, through {unit}.</Trans>);
  }
  if (!ssh.dropInFolder) {
    notes.push(
      <Trans key="drop-ins">sshd_config includes no drop-in folder, so Dockwell won't change SSH here.</Trans>,
    );
  }
  if (notes.length === 0 && ssh.matches.length === 0) return null;
  const count = ssh.matches.length;
  return (
    <div className="firewall-ssh-notes">
      {/* The values are every connection's defaults: each Match block may set them otherwise. */}
      {count > 0 ? (
        <details className="firewall-matches">
          <summary>
            {plural(count, {
              one: "Defaults for every connection: # Match block can set them otherwise",
              other: "Defaults for every connection: # Match blocks can set them otherwise",
            })}
          </summary>
          <ul>
            {ssh.matches.map((m) => (
              <li key={`${m.file}:${m.line}`}>
                <MatchLine match={m} />
              </li>
            ))}
          </ul>
        </details>
      ) : null}
      {notes.length > 0 ? (
        <ul>
          {notes.map((note, i) => (
            // biome-ignore lint/suspicious/noArrayIndexKey: the notes keep their order
            <li key={i}>{note}</li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}

/** A jail's rule in words: how many failures, within how long, ban for how long. */
function useJailWords(): (jail: Fail2banJail) => string {
  const { t } = useLingui();
  const words = useTimeWords();
  return (jail) => {
    const tries = jail.maxRetry;
    const within = jail.findTime ? words.span(Number(jail.findTime.seconds) * 1000) : "";
    const failures = within
      ? plural(tries, { one: `# failed sign-in in ${within}`, other: `# failed sign-ins in ${within}` })
      : plural(tries, { one: "# failed sign-in", other: "# failed sign-ins" });
    if (jail.bansForGood) return t`${failures} · bans for good`;
    if (!jail.banTime) return failures;
    const span = words.span(Number(jail.banTime.seconds) * 1000);
    return t`${failures} · bans for ${span}`;
  };
}

/**
 * fail2ban on a server, as Server-Firewall.dc.html's card has it, read-only for
 * now: whether it runs, its jails as it runs them, and the addresses banned
 * now. fail2ban keeps no history of its bans, so the chart of them waits for
 * one Dockwell keeps (ADR 0052).
 */
export function Fail2banPanel({ server, connected }: { server: Server; connected: boolean }) {
  const { t } = useLingui();
  // A jail is the whole server's, so planning one is an admin's (ADR 0049).
  const mayPlan = useMay()(ChangeService.method.proposeChange, { install: true });
  const read = useQuery(
    FirewallService.method.getServerFail2ban,
    { serverId: server.id },
    { enabled: connected, refetchInterval: FAIL2BAN_EVERY_MS, retry: false },
  );
  const f2b = read.data?.fail2ban;
  // SSH's ports, for the jail that closes them; the SSH card reads the same.
  const ssh = useQuery(
    FirewallService.method.getServerSsh,
    { serverId: server.id },
    { enabled: connected, refetchInterval: SSH_EVERY_MS, retry: false },
  );
  const changes = useChangeSheet();
  const [editing, setEditing] = useState<Editing>();
  const name = server.name;
  const badge = (() => {
    switch (f2b?.state) {
      case Fail2banState.RUNNING:
        return <StatusBadge status="running" label={t`Running`} labelIsState />;
      case Fail2banState.STOPPED:
        return <StatusBadge status="stopped" label={t`Stopped`} labelIsState />;
      case Fail2banState.NOT_INSTALLED:
        return <StatusBadge status="unknown" label={t`Not installed`} labelIsState />;
      case Fail2banState.UNREADABLE:
        return <StatusBadge status="unknown" label={t`Can't tell`} labelIsState />;
      default:
        return undefined;
    }
  })();
  let body: ReactNode;
  if (read.error) {
    body =
      reasonOf(read.error) === "agent_outdated" ? (
        <p className="firewall-note">
          <Trans>
            The agent on {name} is older than the control plane, so it can't read fail2ban. Run the install script on{" "}
            {name} again to update it.
          </Trans>
        </p>
      ) : (
        <p className="firewall-note">{sentence(read.error)}</p>
      );
  } else if (!f2b) {
    body = (
      <p className="firewall-note" aria-busy="true">
        <Trans>Reading fail2ban on {name}</Trans>
      </p>
    );
  } else if (f2b.state === Fail2banState.NOT_INSTALLED) {
    body = (
      <p className="firewall-note">
        <Trans>fail2ban isn't installed on {name}, so nothing bans an address that keeps failing to sign in.</Trans>
      </p>
    );
  } else if (f2b.state === Fail2banState.STOPPED) {
    const why = f2b.why;
    body = (
      <p className="firewall-note">
        <Trans>
          fail2ban isn't running on {name}, so it bans nobody: {why}
        </Trans>
      </p>
    );
  } else if (f2b.state === Fail2banState.UNREADABLE) {
    body = <Callout tone="warning">{f2b.why}</Callout>;
  } else {
    body = <Jails server={server} jails={f2b.jails} onEdit={setEditing} mayPlan={mayPlan} />;
  }
  const running = f2b?.state === Fail2banState.RUNNING;
  const sshJail = f2b?.jails.find((j) => j.name === "sshd");
  return (
    <Panel
      title={t`Fail2ban`}
      actions={
        <>
          {running && !sshJail ? (
            <Button variant="secondary" size="sm" onClick={() => setEditing({ name: "sshd" })} {...mayPlan.off}>
              <Trans>Protect SSH</Trans>
            </Button>
          ) : null}
          {badge}
        </>
      }
      className="firewall-fail2ban"
    >
      {body}
      {editing ? (
        <JailSheet
          server={server}
          jail={editing.jail}
          sshPorts={ssh.data?.ssh?.ports ?? []}
          signedInFrom={f2b?.signedInFrom ?? []}
          yourAddress={read.data?.yourAddress ?? ""}
          onClose={() => setEditing(undefined)}
          open={(opening) => changes.open({ ...opening, onClose: () => void read.refetch() })}
        />
      ) : null}
    </Panel>
  );
}

/** A jail the sheet edits: the one running, or a new one of that name. */
interface Editing {
  name: string;
  jail?: Fail2banJail;
}

/** Lengths of time a jail offers, as fail2ban takes them. */
const SPANS = ["10m", "1h", "1d", "1w"] as const;

/** An address or range as a jail's list has it: one address alone, without its /32 or /128. */
function bare(address: string): string {
  return address.replace(/\/(32|128)$/, "");
}

/** What every jail ignores without being told: the server's own loopback. */
const LOOPBACK = ["127.0.0.0/8", "127.0.0.1/8", "127.0.0.1", "::1"];

/** Why an address is in a jail's list as the form opens. */
type Why = "kept" | "session" | "you";

/**
 * The SSH jail, planned as a change: how many failed sign-ins within how long
 * ban an address, for how long, and whom it never bans. An edit starts from
 * the jail as Dockwell holds it and changes only what the form shows, so its
 * ports, its filter and what it reads stay as they are. A new one reads the
 * journal, or the files the jail running now reads, and closes SSH's ports to
 * the address it bans. What it never bans starts with what the jail ignores
 * now, the addresses signed in over SSH, and your own, each of which you can
 * take out (ADR 0052).
 */
function JailSheet({
  server,
  jail,
  sshPorts,
  signedInFrom,
  yourAddress,
  onClose,
  open,
}: {
  server: Server;
  jail: Fail2banJail | undefined;
  sshPorts: readonly number[];
  /** The addresses signed in to the server over SSH now. */
  signedInFrom: readonly string[];
  /** The address this browser comes from, as the control plane sees it. */
  yourAddress: string;
  onClose: () => void;
  open: (opening: Opening) => void;
}) {
  const { t } = useLingui();
  const words = useTimeWords();
  const field = useRef<HTMLInputElement>(null);
  // The jail as Dockwell holds it, when it writes it: what the edit starts from.
  const spec = jail?.dockwell ? jail.spec : undefined;
  const span = (d: { seconds: bigint } | undefined, fallback: (typeof SPANS)[number]) => {
    const secs = d ? Number(d.seconds) : 0;
    return SPANS.find((s) => SPAN_SECONDS[s] === secs) ?? fallback;
  };
  const [tries, setTries] = useState(String(spec?.maxRetry || jail?.maxRetry || 5));
  // A time the spec holds that isn't one of the choices stays as it is, under its own option.
  const [within, setWithin] = useState<string>(spec?.findTime || span(jail?.findTime, "10m"));
  const [banFor, setBanFor] = useState<string>(
    spec
      ? spec.banForGood
        ? "forever"
        : spec.banTime || "1h"
      : jail?.bansForGood
        ? "forever"
        : span(jail?.banTime, "1h"),
  );
  const [ignore, setIgnore] = useState<{ address: string; why: Why }[]>(() => {
    const list: { address: string; why: Why }[] = [];
    const add = (address: string, why: Why) => {
      const a = bare(address.trim());
      if (a && !LOOPBACK.includes(a) && !list.some((entry) => entry.address === a)) list.push({ address: a, why });
    };
    // What the jail ignores now holds, whoever set it: a section's own list replaces fail2ban's default one.
    for (const a of spec ? spec.ignore : (jail?.ignore ?? [])) add(a, "kept");
    for (const a of signedInFrom) add(a, "session");
    if (yourAddress) add(yourAddress, "you");
    return list;
  });
  const [adding, setAdding] = useState("");
  // Addresses signed in, or where you are, that were taken out of what's never banned.
  const [takenOut, setTakenOut] = useState<string[]>([]);
  const takenOutList = takenOut.join(", ");
  const [error, setError] = useState<string>();
  const name = server.name;
  const spanLabel = (s: string) =>
    (SPANS as readonly string[]).includes(s)
      ? words.span(SPAN_SECONDS[s as (typeof SPANS)[number]] * 1000)
      : t`As it is: ${s}`;
  const spans = (current: string) => [
    ...SPANS.map((s) => ({ value: s as string, label: spanLabel(s) })),
    ...(current !== "forever" && !(SPANS as readonly string[]).includes(current)
      ? [{ value: current, label: spanLabel(current) }]
      : []),
  ];
  const addIgnored = () => {
    const a = bare(adding.trim());
    if (!a) return;
    // An address, a range, or a host name, which fail2ban looks up itself.
    if (!/^[0-9a-fA-F:.]+(\/\d{1,3})?$/.test(a) && !/^[A-Za-z0-9]([A-Za-z0-9-]*\.)+[A-Za-z0-9-]*[A-Za-z]$/.test(a)) {
      setError(t`Name an address or a range to never ban, such as 203.0.113.7 or 198.51.100.0/24.`);
      return;
    }
    setIgnore((list) => (list.some((entry) => entry.address === a) ? list : [...list, { address: a, why: "kept" }]));
    setAdding("");
    setError(undefined);
  };
  const submit = (event?: FormEvent) => {
    event?.preventDefault();
    const n = Number(tries.trim());
    if (!Number.isInteger(n) || n < 1 || n > 100) {
      setError(t`Name how many failed sign-ins ban an address, from 1 to 100, such as 5.`);
      field.current?.focus();
      return;
    }
    // A jail that reads files keeps reading them; one that reads the journal,
    // or a new one, reads the journal, as Debian 12 has no auth.log.
    const files = jail && jail.journalMatches.length === 0 ? [...jail.files] : [];
    // Dockwell's own jail keeps everything the form doesn't show: its filter, what
    // it reads, and its ports, which a failed read of SSH's must never empty.
    const base = spec
      ? { filter: spec.filter, backend: spec.backend, logPaths: [...spec.logPaths], ports: [...spec.ports] }
      : { filter: "sshd", backend: files.length > 0 ? "" : "systemd", logPaths: files, ports: [...sshPorts] };
    onClose();
    open({
      kind: "propose",
      summary: jail?.dockwell ? t`Change the sshd jail on ${name}` : t`Protect SSH on ${name} with fail2ban`,
      edits: [
        {
          resource: { kind: "fail2ban.jail", scope: server.id, name: "sshd" },
          spec: {
            kind: {
              case: "fail2banJail",
              value: {
                ...base,
                maxRetry: n,
                findTime: within,
                banTime: banFor === "forever" ? "" : banFor,
                banForGood: banFor === "forever",
                ignore: ignore.map((entry) => entry.address),
              },
            },
          },
        },
      ],
    });
  };
  const whyWords: Record<Why, string> = {
    kept: t`ignored now`,
    session: t`signed in over SSH now`,
    you: t`where you are now`,
  };
  return (
    <Sheet
      open
      initialFocusRef={field}
      title={jail?.dockwell ? t`The sshd jail on ${name}` : t`Protect SSH on ${name}`}
      subtitle={t`Bans an address that keeps failing to sign in over SSH.`}
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
        <TextField
          ref={field}
          label={t`Failed sign-ins that ban an address`}
          mono
          inputMode="numeric"
          value={tries}
          onChange={(e) => {
            setTries(e.target.value);
            setError(undefined);
          }}
        />
        <SelectField
          label={t`Counted within`}
          value={within}
          onChange={(e) => setWithin(e.target.value)}
          options={spans(within)}
        />
        <SelectField
          label={t`Banned for`}
          value={banFor}
          onChange={(e) => setBanFor(e.target.value)}
          options={[...spans(banFor), { value: "forever", label: t`For good` }]}
        />
        <fieldset className="firewall-never-bans">
          <legend>
            <Trans>Never banned</Trans>
          </legend>
          <ul>
            {ignore.map((entry) => {
              const address = entry.address;
              return (
                <li key={address}>
                  <code className="firewall-never-bans-address">{address}</code>
                  <span className="firewall-set-in">{whyWords[entry.why]}</span>
                  <Button
                    className="firewall-never-bans-out"
                    variant="ghost"
                    size="sm"
                    aria-label={t`Take ${address} out`}
                    onClick={() => {
                      setIgnore((list) => list.filter((e) => e.address !== address));
                      if (entry.why !== "kept") setTakenOut((out) => [...out, address]);
                    }}
                  >
                    <Trans>Take out</Trans>
                  </Button>
                </li>
              );
            })}
          </ul>
          <div className="firewall-never-bans-add">
            <TextField
              label={t`An address or a range to never ban`}
              mono
              value={adding}
              onChange={(e) => setAdding(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  addIgnored();
                }
              }}
            />
            <Button variant="secondary" onClick={addIgnored}>
              <Trans>Add</Trans>
            </Button>
          </div>
        </fieldset>
        <p className="firewall-note">
          <Trans>{name} itself and the control server are never banned either.</Trans>
        </p>
        {/* Said only once it applies: a ban can land on what was taken out as this applies. */}
        {takenOut.length > 0 ? (
          <Callout tone="warning" title={t`This can ban ${takenOutList} as it applies`}>
            <Trans>fail2ban counts the failures from just before.</Trans>
          </Callout>
        ) : null}
        {!spec && sshPorts.length === 0 ? (
          <p className="firewall-note">
            <Trans>SSH's ports couldn't be read: this closes fail2ban's own sshd port, 22 unless set otherwise.</Trans>
          </p>
        ) : null}
      </form>
    </Sheet>
  );
}

/** Seconds in each of SPANS. */
const SPAN_SECONDS: Record<(typeof SPANS)[number], number> = { "10m": 600, "1h": 3600, "1d": 86_400, "1w": 604_800 };

function Jails({
  server,
  jails,
  onEdit,
  mayPlan,
}: {
  server: Server;
  jails: readonly Fail2banJail[];
  onEdit: (editing: Editing) => void;
  /** Whether this reader may plan a change to a jail, and why not. */
  mayPlan: Offer;
}) {
  const { t } = useLingui();
  const changes = useChangeSheet();
  const name = server.name;
  const remove = (jail: string) =>
    changes.open({
      kind: "propose",
      summary: t`Stop writing the ${jail} jail on ${name}`,
      edits: [{ resource: { kind: "fail2ban.jail", scope: server.id, name: jail }, delete: true }],
    });
  const words = useJailWords();
  const time = useTimeWords();
  const now = useControlPlaneNow(SSH_EVERY_MS);
  const bannedNow = jails.reduce((sum, j) => sum + j.bannedNow, 0);
  const bannedTotal = jails.reduce((sum, j) => sum + j.bannedTotal, 0);
  const bans: { jail: string; ban: Fail2banBan }[] = jails
    .flatMap((j) => j.bans.map((ban) => ({ jail: j.name, ban })))
    .sort(
      (a, b) =>
        (b.ban.since ? timestampDate(b.ban.since).getTime() : 0) -
        (a.ban.since ? timestampDate(a.ban.since).getTime() : 0),
    );
  // fail2ban's own count, as the agent sends only the latest of a jail's bans.
  const more = Math.max(bans.length, bannedNow) - BANS_SHOWN;
  if (jails.length === 0) {
    return (
      <p className="firewall-note">
        <Trans>fail2ban runs with no jails, so it bans nobody.</Trans>
      </p>
    );
  }
  return (
    <div className="firewall-fail2ban-body">
      <Metric
        label={t`Banned now`}
        value={bannedNow}
        unit={plural(bannedNow, { one: "address", other: "addresses" })}
        caption={plural(bannedTotal, {
          one: "# ban since fail2ban started",
          other: "# bans since fail2ban started",
        })}
      />
      <section className="firewall-fail2ban-part" aria-label={t`Jails`}>
        <h3>
          <Trans>Jails</Trans>
        </h3>
        <ul className="firewall-jails">
          {jails.map((j) => (
            <li key={j.name} className="firewall-jail">
              <div>
                <code>{j.name}</code>
                <span>{words(j)}</span>
                {j.unreadable ? <span>{j.unreadable}</span> : null}
                {j.dockwell ? <span>{t`Dockwell writes this jail.`}</span> : null}
                {j.name === "sshd" || j.dockwell ? (
                  <span className="firewall-jail-actions">
                    {j.name === "sshd" ? (
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => onEdit({ name: j.name, jail: j })}
                        aria-label={t`Edit the ${j.name} jail`}
                        {...mayPlan.off}
                      >
                        <Trans>Edit</Trans>
                      </Button>
                    ) : null}
                    {j.dockwell ? (
                      <Button
                        variant="ghost"
                        size="sm"
                        onClick={() => remove(j.name)}
                        aria-label={t`Stop writing the ${j.name} jail`}
                        {...mayPlan.off}
                      >
                        <Trans>Remove</Trans>
                      </Button>
                    ) : null}
                  </span>
                ) : null}
              </div>
              <span className={j.bannedNow === 0 ? "firewall-count is-zero" : "firewall-count"}>{j.bannedNow}</span>
            </li>
          ))}
        </ul>
      </section>
      {bans.length > 0 ? (
        <section className="firewall-fail2ban-part" aria-label={t`Recent bans`}>
          <h3>
            <Trans>Recent bans</Trans>
          </h3>
          <ul className="firewall-bans">
            {bans.slice(0, BANS_SHOWN).map(({ jail, ban }) => (
              <li key={`${jail}-${ban.address}`} className="firewall-ban">
                <code>{ban.address}</code>
                <span>{ban.since ? t`${jail} · ${time.ago(timestampDate(ban.since), now)}` : jail}</span>
              </li>
            ))}
          </ul>
          {more > 0 ? (
            <p className="firewall-note">{plural(more, { one: "and # more", other: "and # more" })}</p>
          ) : null}
        </section>
      ) : null}
    </div>
  );
}
