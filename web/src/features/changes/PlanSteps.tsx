// SPDX-License-Identifier: AGPL-3.0-only

import { timestampMs } from "@bufbuild/protobuf/wkt";
import {
  type BackupFirst,
  BackupMethod,
  HostAccessKind,
  type HostAgentUpgrade,
  type Intent,
  type PackageInstall,
  type Plan,
  type PlanRollout,
  type PlanStep,
  PlanWarning,
  type ServerPlan,
  type StepProgress,
  StepState,
} from "@dockwell/api";
import { Icon, StatusBadge, Tag } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";

import { useTimeWords } from "../../lib/time";
import { splitVersion } from "../../lib/versions";
import { useControlPlaneNow } from "../servers/useFleet";
import { DockerStops } from "./DockerStops";
import { MachineWords } from "./FailureNotice";
import { PanelLine, PanelPlan } from "./PanelPlan";
import {
  intentOf,
  isFleet,
  keepsData,
  longestDowntime,
  longestUntilHealthy,
  progressOf,
  type StepGroup,
  stepGroups,
  waveProgress,
} from "./plan";
import { RebootPlan } from "./RebootPlan";
import { UnitLine, UnitPlan } from "./UnitPlan";
import { type ChangeWords, type Tense, useChangeWords } from "./words";

/** How a step's verb reads, from how it is going. */
function tenseOf(state: StepState | undefined): Tense {
  switch (state) {
    case StepState.RUNNING:
      return "doing";
    case StepState.DONE:
      return "done";
    case StepState.FAILED:
      return "failed";
  }
  return "plan";
}

/**
 * A plan's steps, with how each is going once the change applies. A fleet's
 * come server by server; one server's are a plain list, with nothing to tell
 * apart. A change that rolls out has them wave by wave (ADR 0094), each wave
 * with how it stands, after what the rollout does between them.
 */
export function PlanSteps({ plan, progress }: { plan: Plan | undefined; progress: readonly StepProgress[] }) {
  const { t } = useLingui();
  const words = useChangeWords();
  const fleet = isFleet(plan);
  const groups = stepGroups(plan);
  if (groups.length === 0) return null;
  const rollout = plan?.rollout;
  return (
    <section className="plan-section" aria-label={fleet ? t`Steps by server` : t`Steps`}>
      {fleet ? (
        <h3 className="dw-text-label plan-heading">
          {rollout ? <Trans>Steps by wave</Trans> : <Trans>Steps by server</Trans>}
        </h3>
      ) : null}
      {rollout ? <RolloutLine rollout={rollout} /> : null}
      {groups.map((group) => (
        <div key={group.key} className="plan-group">
          {group.wave !== undefined ? <WaveHead group={group} progress={progress} /> : null}
          {group.servers.map((server) => (
            <ServerSteps
              key={server.serverId}
              server={server}
              progress={progress}
              words={words}
              fleet={fleet}
              intent={intentOf(plan)}
              noUndo={plan?.removes !== undefined}
            />
          ))}
        </div>
      ))}
    </section>
  );
}

/** What a rollout does between its waves, and when it stops, in a line. */
function RolloutLine({ rollout }: { rollout: PlanRollout }) {
  const { t } = useLingui();
  const time = useTimeWords();
  const count = rollout.waves.length;
  const waves = plural(count, { one: "# wave", other: "# waves" });
  const pauseMs = rollout.pause ? Number(rollout.pause.seconds) * 1000 + rollout.pause.nanos / 1e6 : 0;
  const pause = time.span(pauseMs);
  const parts = [
    pauseMs > 0 ? t`${waves}, ${pause} apart` : t`${waves}, one after another`,
    rollout.keepGoing
      ? t`a step that fails doesn't stop the rest, and the change fails at the end`
      : t`the first step that fails stops the change, and no server after it is touched`,
  ];
  return <p className="plan-rollout">{parts.join(" · ")}</p>;
}

/** A wave's heading: which wave, its servers, and, as the change applies, how it stands. */
function WaveHead({ group, progress }: { group: StepGroup; progress: readonly StepProgress[] }) {
  const { t } = useLingui();
  const wave = group.wave ?? 0;
  const live = progress.length > 0 ? waveProgress(group, progress) : undefined;
  const servers =
    group.servers.length === 1
      ? (group.servers[0]?.serverName ?? "")
      : plural(group.servers.length, { one: "# server", other: "# servers" });
  const { done = 0, total = 0 } = live ?? {};
  const said = live
    ? {
        waiting: t`Waiting`,
        running: t`${done} of ${total} done`,
        done: t`Done`,
        failed: t`Failed`,
        skipped: t`Not run`,
      }[live.state]
    : "";
  const status = live
    ? ({ waiting: "stopped", running: "deploying", done: "healthy", failed: "down", skipped: "unknown" } as const)[
        live.state
      ]
    : undefined;
  return (
    <div className={`plan-wave-head${live ? ` is-${live.state}` : ""}`}>
      <span className="plan-wave-name">{t`Wave ${wave}`}</span>
      <span className="plan-wave-servers">{servers}</span>
      {live && status ? <StatusBadge status={status} label={said} /> : null}
      {live && total > 0 ? (
        <span className="plan-wave-bar" aria-hidden="true">
          <span style={{ width: `${Math.round((done / total) * 100)}%` }} />
        </span>
      ) : null}
    </div>
  );
}

function ServerSteps({
  server,
  progress,
  words,
  fleet,
  intent,
  noUndo,
}: {
  server: ServerPlan;
  progress: readonly StepProgress[];
  words: ChangeWords;
  fleet: boolean;
  intent: Intent | undefined;
  /** The change has no undo, as a removal hasn't (ADR 0090). */
  noUndo: boolean;
}) {
  const { t } = useLingui();
  const time = useTimeWords();
  const down = longestDowntime([server]);
  const span = time.span(down);
  const healthy = longestUntilHealthy([server]);
  const bound = time.bound(healthy);
  return (
    <div className="plan-server">
      {fleet ? (
        <div className="plan-server-head">
          <Icon name="server" size={14} />
          <code className="plan-server-name">{server.serverName}</code>
          {server.offline ? <Tag>{t`Offline`}</Tag> : null}
          {server.removed ? <Tag>{t`Removed`}</Tag> : null}
          <span className="plan-server-meta">
            {server.removed ? (
              <Trans>No steps: it left the fleet</Trans>
            ) : down > 0 && healthy > down ? (
              <Trans>
                About {span} down, and up to {bound} until healthy
              </Trans>
            ) : down > 0 ? (
              <Trans>About {span} down</Trans>
            ) : (
              <Trans>No downtime</Trans>
            )}
          </span>
        </div>
      ) : null}
      {server.steps.length > 0 ? (
        <ol className="plan-steps">
          {server.steps.map((step) => (
            <StepRow
              key={step.resourceId}
              step={step}
              server={server.serverName}
              serverId={server.serverId}
              live={progressOf(progress, server.serverId, step.resourceId)}
              words={words}
              fleet={fleet}
              intent={intent?.resourceId === step.resourceId ? intent : undefined}
              noUndo={noUndo}
            />
          ))}
        </ol>
      ) : null}
    </div>
  );
}

function StepRow({
  step,
  server,
  serverId,
  live,
  words,
  fleet,
  intent,
  noUndo,
}: {
  step: PlanStep;
  server: string;
  serverId: string;
  live: StepProgress | undefined;
  words: ChangeWords;
  fleet: boolean;
  /** What the change is for, when this step is what does it. */
  intent: Intent | undefined;
  noUndo: boolean;
}) {
  const { t } = useLingui();
  const time = useTimeWords();
  const name = step.resource?.name ?? step.resourceId;
  const look = live ? words.step(live.state) : undefined;
  const planning = !live;
  const stepDown = step.downtime ? Number(step.downtime.seconds) * 1000 + step.downtime.nanos / 1e6 : 0;
  const stepSpan = time.span(stepDown);
  const stepHealthy = step.untilHealthy ? Number(step.untilHealthy.seconds) * 1000 + step.untilHealthy.nanos / 1e6 : 0;
  const stepBound = time.bound(stepHealthy);
  const effect = planning ? words.effect(step.effect) : "";
  const kept = step.keptPaths.join(", ");
  // The keys a Redis user's delete leaves, by their prefix, such as "shop:".
  const keysUnder = step.keysLeft.replace(/\*$/, "");
  const reach = step.hostAccess
    .filter((a) => a.kind !== HostAccessKind.HTTPS_OFF && a.kind !== HostAccessKind.ACCESS_OPENED)
    .map((a) => words.access(a))
    .join("; ");
  const httpsOff = step.hostAccess.filter((a) => a.kind === HostAccessKind.HTTPS_OFF);
  const opens = step.hostAccess.filter((a) => a.kind === HostAccessKind.ACCESS_OPENED);
  const dns = words.domain(step.domainCheck);
  const returning = words.returning(step.returningContainers, server);
  const buildLogins = step.buildLogins.join(", ");
  const activity = live?.state === StepState.RUNNING ? live.activity : "";
  // Why it waits is the step's own to say; every other state its badge says, and a failure is said above.
  const said = live?.state === StepState.WAITING ? live.message : "";
  return (
    <li className={`plan-step${live?.state === StepState.RUNNING ? " is-running" : ""}`}>
      <span className="plan-step-what">
        {look ? <StatusBadge status={look.status} label={look.word} /> : null}
        <span className="plan-step-line">
          {activity !== "" ? (
            <span className="plan-step-activity">{activity}</span>
          ) : intent ? (
            <MachineWords>{words.intent(intent, tenseOf(live?.state))}</MachineWords>
          ) : step.backupFirst ? (
            <BackupFirstLine first={step.backupFirst} name={name} />
          ) : step.reboot ? (
            <Trans>
              Reboot <code>{server}</code>
            </Trans>
          ) : step.install ? (
            <InstallLine install={step.install} server={server} />
          ) : step.unit ? (
            <UnitLine unit={step.unit} server={server} />
          ) : step.panel ? (
            <PanelLine panel={step.panel} server={server} />
          ) : step.agentUpgrade ? (
            <AgentUpgradeLine upgrade={step.agentUpgrade} server={server} />
          ) : (
            <>
              {step.adopts
                ? words.takesOn(tenseOf(live?.state))
                : step.releases
                  ? words.letsGo(tenseOf(live?.state))
                  : words.verb(step.action, tenseOf(live?.state))}{" "}
              <code>{name}</code> <span className="plan-step-kind">{words.kind(step.resource?.kind ?? "")}</span>
            </>
          )}
        </span>
        {/* A reboot's step counts while it waits for its server, as that's the reboot; so does an agent's upgrade. */}
        {live ? <StepTime live={live} waits={Boolean(step.reboot ?? step.agentUpgrade)} /> : null}
      </span>
      {planning && fleet ? (
        <span className="plan-step-meta">
          {effect ? <span>{effect}</span> : null}
          {stepDown > 0 ? <span>{t`About ${stepSpan} down`}</span> : null}
          {stepHealthy > stepDown ? <span>{t`Up to ${stepBound} until healthy`}</span> : null}
          {step.keptPaths.length > 0 ? (
            <span className="plan-step-warn">{t`Takes out of use what its image keeps at ${kept}, which the server keeps for 7 days`}</span>
          ) : keepsData(step) ? (
            <span className="plan-step-warn">{t`Keeps its data for 7 days, then deletes it`}</span>
          ) : step.destroysData ? (
            <span className="plan-step-warn">{t`Destroys data`}</span>
          ) : null}
          {step.keysLeft ? (
            <span>
              {step.keysLeft === "*"
                ? t`Leaves the keys it wrote on the server`
                : t`Leaves the keys under ${keysUnder} on the server`}
            </span>
          ) : null}
          {step.restores ? <span>{t`Brings back the data the server kept`}</span> : null}
          {step.registryLogin ? <span>{t`Pulls with the registry login ${step.registryLogin}`}</span> : null}
          {buildLogins ? (
            <span className="plan-step-warn">{t`Its Dockerfile can read any image ${buildLogins} can read`}</span>
          ) : null}
          {step.hidesLogs ? (
            <span>
              {noUndo
                ? t`Its kept logs can't be read from then on, and go in 7 days`
                : t`Its kept logs can't be read from then on, and go in 7 days. Undoing this brings them back`}
            </span>
          ) : null}
          {reach ? <span className="plan-step-warn">{t`Reaches ${reach} on ${server}`}</span> : null}
          {httpsOff.map((a) => {
            const stopping = words.access(a);
            return (
              <span key={a.path} className="plan-step-warn">
                {t`Takes away ${stopping}`}
              </span>
            );
          })}
          {opens.map((a) => {
            const opening = words.access(a);
            return (
              <span key={`opens ${a.path}`} className="plan-step-warn">
                {t`Opens ${opening}`}
              </span>
            );
          })}
          {step.warnings.includes(PlanWarning.MAY_LOCK_OUT) ? (
            <span className="plan-step-warn">{t`May cut off access to ${server}`}</span>
          ) : null}
          {words.ingress(step.ingress, server).map((line) => (
            <span key={line}>{line}</span>
          ))}
          {returning ? <span className="plan-step-warn">{returning}</span> : null}
          {dns ? <span className="plan-step-warn">{dns}</span> : null}
          {step.unencrypted ? <span className="plan-step-notice">{words.unencrypted(server)}</span> : null}
          {step.goes ? <span className="plan-step-notice">{words.goes(step.goes)}</span> : null}
          {step.moves ? <span className="plan-step-notice">{words.moves(step.moves)}</span> : null}
          {words.answers(step.answers, step.serverPort, server).map((line) => (
            <span key={line} className="plan-step-notice">
              {line}
            </span>
          ))}
        </span>
      ) : null}
      {said ? <span className="plan-step-meta">{said}</span> : null}
      {planning && step.install ? <InstallList install={step.install} server={server} serverId={serverId} /> : null}
      {planning && step.reboot ? <RebootPlan reboot={step.reboot} server={server} serverId={serverId} /> : null}
      {planning && step.unit ? <UnitPlan unit={step.unit} server={server} serverId={serverId} /> : null}
      {planning && step.panel ? <PanelPlan panel={step.panel} server={server} /> : null}
      {planning && step.agentUpgrade ? <AgentUpgradePlan upgrade={step.agentUpgrade} /> : null}
    </li>
  );
}

/** What upgrading a server's agent does, as its step's line: from the version it runs to the control plane's. */
function AgentUpgradeLine({ upgrade, server }: { upgrade: HostAgentUpgrade; server: string }) {
  const { from, to } = upgrade;
  return (
    <Trans>
      Upgrade the agent on <code>{server}</code> from {from} to {to}
    </Trans>
  );
}

/**
 * Under an agent upgrade's step: what runs, named by its SHA-256 as the plan holds it,
 * and that it can't be undone (ADR 0094).
 */
function AgentUpgradePlan({ upgrade }: { upgrade: HostAgentUpgrade }) {
  const { t } = useLingui();
  const sha = upgrade.scriptSha256.slice(0, 12);
  return (
    <div className="plan-install">
      <span className="plan-step-meta">
        <span>{t`Runs the control plane's install script, SHA-256 ${sha}, which checks the release's signature before it installs it`}</span>
        <span>{t`The step passes once the agent is back at the new version`}</span>
        <span className="plan-step-warn">{t`An upgrade can't be undone`}</span>
      </span>
    </div>
  );
}

/** How long a step has taken, counting up in tabular figures while it runs. */
/** What a step that backs up a volume does: its own dump, or a copy of what stops for it. */
function BackupFirstLine({ first, name }: { first: BackupFirst; name: string }) {
  const { i18n } = useLingui();
  const list = new Intl.ListFormat(i18n.locale, { style: "long", type: "conjunction" });
  const stops = list.format(first.stops);
  if (first.databaseName && first.also.length > 0) {
    // Several apps' databases on one server, each dumped alone (ADR 0084).
    const databases = list.format([first.databaseName, ...first.also.map((d) => d.databaseName)]);
    return (
      <Trans>
        Dump {databases} alone from <code>{name}</code>, each as a backup of its own, with nothing stopped
      </Trans>
    );
  }
  if (first.databaseName) {
    return (
      <Trans>
        Dump <code>{first.databaseName}</code> alone from <code>{name}</code>, with nothing stopped
      </Trans>
    );
  }
  if (first.method === BackupMethod.DUMP) {
    return (
      <Trans>
        Back up <code>{name}</code> with {first.database}'s own dump, with nothing stopped
      </Trans>
    );
  }
  if (first.stops.length > 0) {
    return (
      <Trans>
        Back up <code>{name}</code>, with {stops} stopped for an exact copy and started again after
      </Trans>
    );
  }
  return (
    <Trans>
      Back up <code>{name}</code>
    </Trans>
  );
}

/** What installing a server's updates does, in a line (ADR 0064). */
function InstallLine({ install, server }: { install: PackageInstall; server: string }) {
  const n = install.packages.length;
  return (
    <>
      {plural(n, { one: "Install # package on", other: "Install # packages on" })} <code>{server}</code>
    </>
  );
}

/**
 * Each package the install installs, from the version there to the one it gets, the
 * part that changes marked, with those the others pull in too: for packages picked by
 * name, every one not picked; otherwise each new to the server. Then what follows: a
 * reboot, Docker restarting, and that it can't be undone.
 */
function InstallList({ install, server, serverId }: { install: PackageInstall; server: string; serverId: string }) {
  const { t } = useLingui();
  const asked = new Set(install.asked);
  // What was picked comes first, then what it needs.
  const packages =
    asked.size > 0
      ? [...install.packages].sort((a, b) => Number(!asked.has(a.name)) - Number(!asked.has(b.name)))
      : install.packages;
  return (
    <div className="plan-install">
      <ul className="plan-install-packages" aria-label={t`Packages it installs on ${server}`}>
        {packages.map((p) => {
          const [same, changed] = splitVersion(p.installed, p.available);
          const pulled = asked.size > 0 ? !asked.has(p.name) : !p.installed;
          return (
            <li key={p.name}>
              <code>{p.name}</code>{" "}
              <span className="plan-install-versions">
                {p.installed ? `${p.installed} → ` : null}
                {same}
                <mark className="plan-install-changed">{changed}</mark>
              </span>
              {pulled ? <Tag words>{t`Pulled in`}</Tag> : null}
              {p.security ? <Tag words>{t({ message: "Security fix", context: "a package update" })}</Tag> : null}
            </li>
          );
        })}
      </ul>
      <span className="plan-step-meta">
        {install.dockerRestarts ? (
          install.keepsRunning ? (
            <span>{t`Docker restarts, and keeps containers running while it does: it's a patch release, and live-restore is on`}</span>
          ) : (
            <span className="plan-step-warn">
              {t`Docker restarts: every app and website on ${server} stops until it's back, the domains Caddy serves there too`}
            </span>
          )
        ) : null}
        {install.dockerRestarts && !install.keepsRunning ? <DockerStops serverId={serverId} server={server} /> : null}
        {install.reboot ? (
          <span className="plan-step-warn">{t`${server} needs a reboot for this to take effect, which is a change of its own`}</span>
        ) : null}
        <span>
          {t`Services these packages run may restart as they're installed. It can't be undone: what it installs stays installed`}
        </span>
      </span>
    </div>
  );
}

function StepTime({ live, waits = false }: { live: StepProgress; waits?: boolean }) {
  if (!live.startTime) return null;
  const start = timestampMs(live.startTime);
  if (live.finishTime) return <Took ms={timestampMs(live.finishTime) - start} />;
  return live.state === StepState.RUNNING || (waits && live.state === StepState.WAITING) ? (
    <Counting start={start} />
  ) : null;
}

function Counting({ start }: { start: number }) {
  const now = useControlPlaneNow(1_000);
  return <Took ms={now - start} />;
}

function Took({ ms }: { ms: number }) {
  const time = useTimeWords();
  return <span className="plan-step-time">{time.bound(Math.max(ms, 0))}</span>;
}
