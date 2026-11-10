// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import {
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
import { Callout, Metric, Panel, StatusBadge } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { useTimeWords } from "../../lib/time";
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
  const read = useQuery(
    FirewallService.method.getServerFail2ban,
    { serverId: server.id },
    { enabled: connected, refetchInterval: FAIL2BAN_EVERY_MS, retry: false },
  );
  const f2b = read.data?.fail2ban;
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
    body = <Jails jails={f2b.jails} />;
  }
  return (
    <Panel title={t`Fail2ban`} actions={badge} className="firewall-fail2ban">
      {body}
    </Panel>
  );
}

function Jails({ jails }: { jails: readonly Fail2banJail[] }) {
  const { t } = useLingui();
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
