// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import {
  type Change,
  ChangeService,
  ChangeState,
  type FleetRollout,
  FleetRolloutSchema,
  type Server,
  SystemServiceAction,
} from "@dockwell/api";
import {
  Button,
  cx,
  FleetGrid,
  type FleetServer,
  Icon,
  type IconName,
  SegmentedControl,
  StatusBadge,
  Switch,
  TextField,
  useUIStrings,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQuery as useAnyQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useId, useMemo, useState } from "react";

import { formatDay } from "../../lib/format";
import { RouterButtonLink } from "../../lib/links";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { Connecting } from "../servers/FleetNotes";
import { LabelChips } from "../servers/LabelChips";
import {
  fleetLabels,
  groupBy,
  isProblem,
  labelSuggestions,
  matchesSelector,
  parseSelector,
  parseTerm,
  quickLabels,
  type Selector,
  selectorText,
  termText,
} from "../servers/labels";
import { useServerTiles } from "../servers/tiles";
import { useFleet } from "../servers/useFleet";
import { Page } from "../shell/Page";
import { agentRelease, connected, inOrder, runs, waveSizes } from "./rollout";
import "./FleetActionPage.css";

/** Which servers a fleet action targets, as its URL says: those picked on Servers, or a selector of their labels. */
export interface FleetActionSearch {
  /** The servers picked on Servers, by ID, with commas between. */
  servers?: string;
  /** A selector of their labels, such as "pool:web region:eu" (ADR 0091). */
  selector?: string;
}

/** The actions a fleet action takes, each a plan method of its own (ADR 0094). */
type ActionKind = "updates" | "unit" | "upgrade" | "reboot";

/** What a unit action does, of those a fleet runs. */
type UnitVerb = "restart" | "reload" | "start" | "stop";

const UNIT_ACTIONS: Record<UnitVerb, SystemServiceAction> = {
  restart: SystemServiceAction.RESTART,
  reload: SystemServiceAction.RELOAD,
  start: SystemServiceAction.START,
  stop: SystemServiceAction.STOP,
};

/** How many servers a target names one by one, before it draws them as tiles. */
const NAMED_UP_TO = 12;

/** What a system service's name may be, as the control plane takes it. */
const SERVICE_NAME = /^(?:[A-Za-z0-9:_.@\\-]+\.(?:service|socket)|[A-Za-z0-9_][A-Za-z0-9_.@-]*)$/;

/**
 * New fleet action, as Fleet-Action lays it out (ADR 0094): the servers, by a
 * selector or as picked on Servers, with their tiles; the action; the rollout, its
 * waves drawn as they'd run; and a summary beside them, with Review plan, which
 * opens the plan sheet with its waves. Nothing runs until the plan is applied.
 */
export function FleetActionPage({
  search,
  onSearch,
}: {
  search: FleetActionSearch;
  onSearch: (next: FleetActionSearch, how?: { replace?: boolean }) => void;
}) {
  const { t } = useLingui();
  const fleet = useFleet();
  const all = useMemo(() => (fleet.status === "connecting" ? [] : inOrder(fleet.servers)), [fleet]);
  const picked = useMemo(() => search.servers?.split(",").filter(Boolean), [search.servers]);
  const selector = useMemo(() => parseSelector(search.selector ?? ""), [search.selector]);
  const targets = useMemo(
    () => (picked ? all.filter((s) => picked.includes(s.id)) : all.filter((s) => matchesSelector(selector, s.labels))),
    [all, picked, selector],
  );
  const [action, setAction] = useState<ActionKind>("updates");
  const [everyUpdate, setEveryUpdate] = useState(false);
  const [unitName, setUnitName] = useState("");
  const [unitVerb, setUnitVerb] = useState<UnitVerb>("restart");
  const [canary, setCanary] = useState(true);
  const [batch, setBatch] = useState("");
  const [pause, setPause] = useState("");
  const [stopFirst, setStopFirst] = useState(true);
  const release = agentRelease(all);

  if (fleet.status === "connecting") {
    return (
      <Page width="wide" title={t`New fleet action`}>
        <Connecting error={fleet.error} />
      </Page>
    );
  }

  // What the plan will leave out, as the preview says it: a server that's offline, of a set of
  // more than one, and for an agent's upgrade, the control server and an agent at the version.
  const offline = targets.length > 1 ? targets.filter((s) => !connected(s)) : [];
  const control = action === "upgrade" ? targets.filter((s) => s.controlServer) : [];
  const current = action === "upgrade" && release ? targets.filter((s) => !s.controlServer && runs(s, release)) : [];
  const out = new Set([...offline, ...control, ...current].map((s) => s.id));
  const acting = targets.filter((s) => !out.has(s.id));
  const batchSize = Number.parseInt(batch, 10) > 0 ? Number.parseInt(batch, 10) : 0;
  const pauseMinutes = Number.parseFloat(pause) > 0 ? Number.parseFloat(pause) : 0;
  const waves = waveSizes(
    acting.length,
    canary,
    batchSize,
    acting.some((s) => s.controlServer),
  );
  const unitOk = action !== "unit" || SERVICE_NAME.test(unitName.trim());
  const pickedText = picked ? targets.map((s) => s.name).join(", ") : "";

  return (
    <Page
      width="wide"
      title={t`New fleet action`}
      lead={t`Pick servers, an action and a rollout. Nothing runs until you apply the plan.`}
      actions={
        <RouterButtonLink to="/servers" variant="ghost">
          <Trans>Cancel</Trans>
        </RouterButtonLink>
      }
    >
      <div className="fleet-action">
        <div className="fleet-action-main">
          <Step number={1} title={t`Target`} sub={picked ? t`The servers you picked` : t`Servers whose labels match`}>
            <Target
              all={all}
              targets={targets}
              picked={picked}
              selector={selector}
              onSelector={(next) => {
                const text = selectorText(next.terms, next.bad);
                onSearch(text === "" ? {} : { selector: text }, { replace: true });
              }}
              onUseSelector={() => onSearch({}, { replace: true })}
            />
            <Leaving offline={offline} control={control} current={current} release={release} />
          </Step>
          <Step number={2} title={t`Action`} sub={t`What runs on each server`}>
            <Actions
              value={action}
              onChange={setAction}
              release={release}
              targets={targets}
              current={current.length}
              // Which updates is the updates card's own choice, inside it (#800).
              updatesChoice={
                <SegmentedControl<"security" | "all">
                  label={t`Which updates`}
                  size="sm"
                  value={everyUpdate ? "all" : "security"}
                  onChange={(v) => setEveryUpdate(v === "all")}
                  options={[
                    { value: "security", label: t`Security updates` },
                    { value: "all", label: t`Every update` },
                  ]}
                />
              }
            />
            {action === "unit" ? (
              <div className="fleet-action-unit">
                <TextField
                  label={t`System service`}
                  mono
                  placeholder="nginx.service"
                  value={unitName}
                  onChange={(e) => setUnitName(e.target.value)}
                  spellCheck={false}
                  autoCapitalize="off"
                  error={
                    unitName.trim() !== "" && !unitOk
                      ? t`That isn't a system service's name, such as nginx.service`
                      : undefined
                  }
                />
                <SegmentedControl<UnitVerb>
                  label={t`What to do to it`}
                  size="sm"
                  value={unitVerb}
                  onChange={setUnitVerb}
                  options={[
                    { value: "restart", label: t`Restart` },
                    { value: "reload", label: t`Reload` },
                    { value: "start", label: t`Start` },
                    { value: "stop", label: t`Stop` },
                  ]}
                />
              </div>
            ) : null}
          </Step>
          <Step number={3} title={t`Rollout`} sub={t`How fast, and when to stop`}>
            <Rollout
              waves={waves}
              servers={acting.length}
              first={acting[0]?.name}
              canary={canary}
              onCanary={setCanary}
              batch={batch}
              onBatch={setBatch}
              auto={Math.ceil(acting.length / 4) || 1}
              pause={pause}
              onPause={setPause}
              stopFirst={stopFirst}
              onStopFirst={setStopFirst}
            />
          </Step>
        </div>
        <div className="fleet-side">
          <Summary
            targets={acting.length}
            waves={waves}
            target={picked ? pickedText : selectorText(selector.terms) || t`Every server`}
            action={action}
            everyUpdate={everyUpdate}
            unitVerb={unitVerb}
            unitName={unitName.trim()}
            release={release}
            pauseMinutes={pauseMinutes}
            stopFirst={stopFirst}
            ready={acting.length > 0 && unitOk && (action !== "upgrade" || release !== undefined)}
            plan={{
              servers: picked ?? (search.selector ? undefined : all.map((s) => s.id)),
              selector: picked ? "" : selectorText(selector.terms, selector.bad),
              rollout: create(FleetRolloutSchema, {
                noCanary: !canary,
                batchSize,
                keepGoing: !stopFirst,
                ...(pauseMinutes > 0 ? { pause: durationFromMs(Math.round(pauseMinutes * 60_000)) } : {}),
              }),
              action,
              everyUpdate,
              unitName: unitName.trim(),
              unitAction: UNIT_ACTIONS[unitVerb],
            }}
          />
          <RecentFleetActions />
        </div>
      </div>
    </Page>
  );
}

/** One numbered step of the form, as the mock numbers them: its title, what it's for, then what it holds. */
function Step({
  number,
  title,
  sub,
  children,
}: {
  number: number;
  title: string;
  sub: string;
  children: React.ReactNode;
}) {
  const id = useId();
  return (
    <section className="dw-panel fleet-step" aria-labelledby={id}>
      <div className="dw-panel-head fleet-step-head">
        <span className="fleet-step-number" aria-hidden="true">
          {number}
        </span>
        <h2 className="dw-panel-title" id={id}>
          {title}
        </h2>
        <span className="fleet-step-sub">{sub}</span>
      </div>
      <div className="dw-panel-body fleet-step-body">{children}</div>
    </section>
  );
}

/** The servers targeted: a selector over the fleet's labels, or those picked on Servers, then their tiles. */
function Target({
  all,
  targets,
  picked,
  selector,
  onSelector,
  onUseSelector,
}: {
  all: readonly Server[];
  targets: readonly Server[];
  picked: string[] | undefined;
  selector: Selector;
  onSelector: (next: Selector) => void;
  onUseSelector: () => void;
}) {
  const { t } = useLingui();
  const used = useMemo(() => fleetLabels(all), [all]);
  const tiles = useServerTiles(targets, false);
  // By region, as the mock groups them, once the servers have one; those without, last.
  const none = t`No region`;
  const groups = useMemo(() => {
    if (!used.has("region")) return undefined;
    const byId = new Map(tiles.map((tile) => [tile.id, tile]));
    const grouped = groupBy(targets, "region");
    if (grouped.length < 2 && grouped[0]?.value === undefined) return undefined;
    return grouped.map((g) => ({
      label: g.value ?? none,
      servers: g.servers.map((s) => byId.get(s.id)).filter((tile): tile is FleetServer => tile !== undefined),
    }));
  }, [used, tiles, targets, none]);
  const { terms, bad } = selector;
  const count = targets.length;
  const example = labelSuggestions(used)[0] ?? "region:eu";
  const matching = plural(count, { one: "# server matches", other: "# servers match" });
  return (
    <>
      {picked ? (
        <div className="fleet-target-picked">
          <span className="fleet-target-count">
            {plural(count, { one: "# server picked", other: "# servers picked" })}
          </span>
          <Button variant="ghost" size="sm" onClick={onUseSelector}>
            <Trans>Pick by labels instead</Trans>
          </Button>
        </div>
      ) : (
        <LabelChips
          label={t`Add a label to the selector`}
          icon="filter"
          chips={[
            ...terms.map((term) => ({
              text: termText(term),
              key: `${term.not ? "!" : ""}${term.key}`,
              value: term.hasValue ? term.value : undefined,
            })),
            ...bad.map((b) => ({ text: b.text, key: b.text, problem: b.problem })),
          ]}
          onAdd={(text) => {
            const term = parseTerm(text);
            if (isProblem(term)) return;
            onSelector({ terms: [...terms.filter((x) => termText(x) !== termText(term)), term], bad });
          }}
          onRemove={(i) =>
            onSelector(
              i < terms.length
                ? { terms: terms.filter((_, j) => j !== i), bad }
                : { terms, bad: bad.filter((_, j) => j !== i - terms.length) },
            )
          }
          check={(text) => {
            const term = parseTerm(text);
            return isProblem(term) ? term : undefined;
          }}
          suggestions={labelSuggestions(used)}
          // The labels the fleet uses, a click each (#800); a selector can only match what's there.
          quick={used.size > 0 ? quickLabels(used) : []}
          placeholder={used.size > 0 ? t`Every server, or a label such as ${example}` : t`Every server`}
          after={<span className="label-chips-after">{matching}</span>}
        />
      )}
      {count > 0 && !groups && count <= NAMED_UP_TO ? (
        // A handful of servers reads by name, each with its state (#800); a fleet reads as tiles.
        <ul className="fleet-target-names" aria-label={t`Servers targeted`}>
          {tiles.map((tile) => (
            <li key={tile.id}>
              <StatusBadge status={tile.status} label={tile.name} />
            </li>
          ))}
        </ul>
      ) : count > 0 ? (
        <section className="fleet-target-tiles" aria-label={t`Servers targeted`}>
          <FleetGrid servers={groups ? undefined : tiles} groups={groups} legend={false} readout={false} />
        </section>
      ) : (
        <p className="fleet-target-none">
          {bad.length > 0 ? (
            <Trans>No server matches while the selector has a term it can't read</Trans>
          ) : (
            <Trans>No server matches</Trans>
          )}
        </p>
      )}
    </>
  );
}

/** What the plan will leave out of the target, and why, in a line each. */
function Leaving({
  offline,
  control,
  current,
  release,
}: {
  offline: readonly Server[];
  control: readonly Server[];
  current: readonly Server[];
  release: string | undefined;
}) {
  const { t, i18n } = useLingui();
  const list = (servers: readonly Server[]) =>
    new Intl.ListFormat(i18n.locale, { style: "long", type: "conjunction" }).format(servers.map((s) => s.name));
  const lines: { icon: IconName; text: string }[] = [];
  if (offline.length > 0) {
    const names = list(offline);
    lines.push({
      icon: "clock",
      text:
        offline.length === 1
          ? t`${names} isn't connected, so it isn't in this change`
          : t`${names} aren't connected, so they aren't in this change`,
    });
  }
  if (control.length > 0) {
    const names = list(control);
    lines.push({ icon: "server", text: t`${names} is the control server, which upgrades by the install script on it` });
  }
  if (current.length > 0 && release) {
    const names = list(current);
    lines.push({
      icon: "check",
      text: current.length === 1 ? t`${names} runs ${release} already` : t`${names} run ${release} already`,
    });
  }
  if (lines.length === 0) return null;
  return (
    <ul className="fleet-target-leaving">
      {lines.map((line) => (
        <li key={line.text}>
          <Icon name={line.icon} size={14} />
          <span>{line.text}</span>
        </li>
      ))}
    </ul>
  );
}

/** The actions as cards, one picked, each saying what it would do to this set. */
function Actions({
  value,
  onChange,
  release,
  targets,
  current,
  updatesChoice,
}: {
  value: ActionKind;
  onChange: (next: ActionKind) => void;
  release: string | undefined;
  targets: readonly Server[];
  current: number;
  /** Which updates, shown inside the updates card while it's picked. */
  updatesChoice: React.ReactNode;
}) {
  const { t } = useLingui();
  const name = useId();
  const agents = targets.filter((s) => !s.controlServer).length;
  const upgradeSub = !release
    ? t`The control plane is a development build, so it has no release to upgrade agents to`
    : agents > 0 && current === agents
      ? plural(agents, { one: `It runs ${release} already`, other: `All # run ${release} already` })
      : t`To ${release}, the control plane's version, by its own install script`;
  const cards: { kind: ActionKind; title: string; sub: string; disabled?: boolean }[] = [
    {
      kind: "updates",
      title: t`Apply OS updates`,
      sub: t`The updates waiting on each server, as its Updates tab lists them`,
    },
    {
      kind: "unit",
      title: t`Act on a system service`,
      sub: t`Restart, reload, start or stop one service on each server`,
    },
    { kind: "upgrade", title: t`Upgrade the agent`, sub: upgradeSub, disabled: !release },
    {
      kind: "reboot",
      title: t`Reboot servers`,
      sub: t`Each wave's servers reboot together, and the next wave goes once they're back`,
    },
  ];
  return (
    <fieldset className="fleet-actions-cards">
      <legend className="dw-sr">{t`Action`}</legend>
      {cards.map((c) => (
        <div
          key={c.kind}
          className={cx("fleet-action-card", c.kind === value && "is-picked", c.disabled && "is-disabled")}
        >
          <label className="fleet-action-card-pick">
            <input
              type="radio"
              name={name}
              value={c.kind}
              checked={c.kind === value}
              disabled={c.disabled}
              onChange={() => onChange(c.kind)}
            />
            <span className="fleet-action-card-text">
              <span className="fleet-action-card-title">{c.title}</span>
              <span className="fleet-action-card-sub">{c.sub}</span>
            </span>
          </label>
          {c.kind === "updates" && value === "updates" ? (
            <div className="fleet-action-card-choice">{updatesChoice}</div>
          ) : null}
        </div>
      ))}
    </fieldset>
  );
}

/** The rollout: its waves drawn as they'd run, the canary, the batch, the pause, and when it stops. */
function Rollout({
  waves,
  servers,
  first,
  canary,
  onCanary,
  batch,
  onBatch,
  auto,
  pause,
  onPause,
  stopFirst,
  onStopFirst,
}: {
  waves: number[];
  /** How many servers take a step, as the preview has it. */
  servers: number;
  first: string | undefined;
  canary: boolean;
  onCanary: (on: boolean) => void;
  batch: string;
  onBatch: (v: string) => void;
  /** The batch the control plane picks when it's left to it: a quarter of the servers, rounded up. */
  auto: number;
  pause: string;
  onPause: (v: string) => void;
  stopFirst: boolean;
  onStopFirst: (on: boolean) => void;
}) {
  const { t } = useLingui();
  const strings = useUIStrings();
  return (
    <div className="fleet-rollout">
      <div className="fleet-rollout-row">
        <span className="fleet-rollout-label">{t`Waves`}</span>
        {waves.length > 0 ? (
          <ol className="fleet-waves" aria-label={t`Waves`}>
            {waves.map((n, i) => {
              const wave = i + 1;
              return (
                // biome-ignore lint/suspicious/noArrayIndexKey: a wave is its place in the order
                <li key={i} className="fleet-wave" style={{ flexGrow: n }}>
                  <span className="fleet-wave-rule">{t`Wave ${wave}`}</span>
                  <span className={cx("fleet-wave-sub", i === 0 && canary && "is-mono")}>
                    {i === 0 && canary && first ? first : strings.fleetGrid.servers(n)}
                  </span>
                </li>
              );
            })}
          </ol>
        ) : (
          <span className="fleet-rollout-one">
            {servers === 0 ? t`No server takes a step, so there are no waves` : t`One server is a set of one: no waves`}
          </span>
        )}
      </div>
      <div className="fleet-rollout-row">
        <span className="fleet-rollout-label">{t`Canary`}</span>
        <Switch checked={canary} onChange={onCanary} label={t`One server first, on its own`} />
      </div>
      <div className="fleet-rollout-row">
        <span className="fleet-rollout-label">{t`Batch`}</span>
        <TextField
          aria-label={t`Servers in each wave`}
          className="fleet-rollout-number"
          inputMode="numeric"
          size="sm"
          placeholder={t`Auto: ${auto}`}
          value={batch}
          onChange={(e) => onBatch(e.target.value.replace(/\D/g, ""))}
          suffix={t`servers a wave`}
        />
      </div>
      <div className="fleet-rollout-row">
        <span className="fleet-rollout-label">{t`Pause`}</span>
        <TextField
          aria-label={t`Minutes between waves`}
          className="fleet-rollout-number"
          inputMode="decimal"
          size="sm"
          placeholder="0"
          value={pause}
          onChange={(e) => onPause(e.target.value.replace(/[^\d.]/g, ""))}
          suffix={t`min between waves`}
        />
      </div>
      <div className="fleet-rollout-row">
        <span className="fleet-rollout-label">{t`Stop if`}</span>
        <Switch
          checked={stopFirst}
          onChange={onStopFirst}
          label={t`A step fails`}
          description={
            stopFirst ? t`No server after it is touched` : t`Each server has its turn, and the change fails at the end`
          }
        />
      </div>
    </div>
  );
}

interface PlanInput {
  servers: string[] | undefined;
  selector: string;
  rollout: FleetRollout;
  action: ActionKind;
  everyUpdate: boolean;
  unitName: string;
  unitAction: SystemServiceAction;
}

/** The summary beside the form, with Review plan, which plans the action and opens its sheet. */
function Summary({
  targets,
  waves,
  target,
  action,
  everyUpdate,
  unitVerb,
  unitName,
  release,
  pauseMinutes,
  stopFirst,
  ready,
  plan,
}: {
  targets: number;
  waves: number[];
  target: string;
  action: ActionKind;
  everyUpdate: boolean;
  unitVerb: UnitVerb;
  unitName: string;
  release: string | undefined;
  pauseMinutes: number;
  stopFirst: boolean;
  ready: boolean;
  plan: PlanInput;
}) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const transport = useTransport();
  const sheet = useChangeSheet();
  const client = useMemo(() => createClient(ChangeService, transport), [transport]);
  const may = useMay();
  // Each action is its own method, which may ask more than the others (ADR 0094).
  const mayPlan = {
    updates: may(ChangeService.method.planInstallUpdates, { install: true }),
    unit: may(ChangeService.method.planSystemServiceAction, { install: true }),
    upgrade: may(ChangeService.method.planAgentUpgrade, { install: true }),
    reboot: may(ChangeService.method.planReboot, { install: true }),
  };
  const allowed = mayPlan[action];
  const [planning, setPlanning] = useState(false);
  const serverWords = strings.fleetGrid.servers(targets);
  const count = waves.length;
  const waveWords = plural(count, { one: "# wave", other: "# waves" });
  const headline = count > 1 ? t`${serverWords} in ${waveWords}` : serverWords;
  const sizes = waves.map((n) => plural(n, { one: "# server", other: "# servers" })).join(" → ");
  const service = unitName || "…";
  const unitWords = {
    restart: t`Restart ${service}`,
    reload: t`Reload ${service}`,
    start: t`Start ${service}`,
    stop: t`Stop ${service}`,
  }[unitVerb];
  const actionWords = {
    updates: everyUpdate ? t`Apply every OS update` : t`Apply OS security updates`,
    unit: unitWords,
    upgrade: release ? t`Upgrade the agent to ${release}` : t`Upgrade the agent`,
    reboot: t`Reboot`,
  }[action];
  const review = () => {
    setPlanning(true);
    const set = { serverIds: plan.servers ?? [], selector: plan.servers ? "" : plan.selector, rollout: plan.rollout };
    sheet.open({
      kind: "plan",
      make: async () => {
        try {
          switch (plan.action) {
            case "updates":
              return await client.planInstallUpdates({ ...set, all: plan.everyUpdate });
            case "unit":
              return await client.planSystemServiceAction({ ...set, name: plan.unitName, action: plan.unitAction });
            case "upgrade":
              return await client.planAgentUpgrade(set);
            case "reboot":
              return await client.planReboot(set);
          }
        } finally {
          setPlanning(false);
        }
      },
    });
  };
  return (
    <aside className="dw-panel fleet-summary" aria-label={t`Summary`}>
      <div className="dw-panel-head">
        <h2 className="dw-panel-title">{t`Summary`}</h2>
        <span className="fleet-summary-draft">{t`Draft`}</span>
      </div>
      <div className="dw-panel-body fleet-summary-body">
        <p className="fleet-summary-headline">{headline}</p>
        <dl className="fleet-summary-facts">
          <dt>{t`Target`}</dt>
          <dd className="is-mono">{target}</dd>
          <dt>{t`Action`}</dt>
          <dd className={action === "unit" ? "is-mono" : undefined}>{actionWords}</dd>
          <dt>{t`Waves`}</dt>
          <dd>{count > 1 ? sizes : t`None`}</dd>
          <dt>{t`Pause`}</dt>
          <dd>{pauseMinutes > 0 ? t`${pauseMinutes} min between waves` : t`None`}</dd>
          <dt>{t`Stops`}</dt>
          <dd>{stopFirst ? t`On the first step that fails` : t`Never; fails at the end if any step did`}</dd>
        </dl>
        <Button
          variant="primary"
          icon="commit"
          className="fleet-summary-review"
          loading={planning}
          disabled={!ready || !allowed.allowed}
          onClick={review}
          {...allowed.off}
        >
          <Trans>Review plan</Trans>
        </Button>
        <p className="fleet-summary-note">{t`The plan says what each server would do, before anything runs.`}</p>
      </div>
    </aside>
  );
}

/** How many fleet actions the panel lists, and how many pages of changes it reads at most to find them. */
const RECENT = 5;
const RECENT_PAGES = 25;
const RECENT_STALE_MS = 5 * 60_000;

/** A change that ran over a fleet in waves: applied, failed or running, not a plan nobody applied. */
function fleetAction(c: Change): boolean {
  return (
    (c.plan?.rollout?.waves.length ?? 0) > 0 && c.state !== ChangeState.PLANNED && c.state !== ChangeState.DISCARDED
  );
}

/**
 * The fleet actions run lately, newest first, each a link to its change (#800): what
 * it did, on how many servers, by whom and when. Plans nobody applied aren't actions.
 */
function RecentFleetActions() {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  // Page by page, newest first, until five are found or the changes run out, as ListChanges has no filter for
  // a plan with waves. PAGES bounds what one look reads: past it, an install with that much else since its last
  // fleet action shows none, and a filter on the server is the way past that (#800's review, L3).
  const changes = useAnyQuery({
    // Under ChangeService's own key, so sign-out drops it with the rest and a change applied reads it again
    // (#800's re-check, N1).
    queryKey: ["connect-query", { serviceName: ChangeService.typeName, methodName: "RecentFleetActions" }],
    retry: false,
    // A look can read 25 pages, so a remount or the window's focus keeps what it read for a few minutes; a
    // change applied still reads it again, as its key is ChangeService's (#858's N3).
    staleTime: RECENT_STALE_MS,
    refetchOnWindowFocus: false,
    queryFn: async ({ signal }) => {
      const client = createClient(ChangeService, transport);
      const found: Change[] = [];
      let pageToken = "";
      for (let page = 0; page < RECENT_PAGES && found.length < RECENT; page++) {
        const res = await client.listChanges({ pageSize: 200, pageToken }, { signal });
        found.push(...res.changes.filter(fleetAction));
        pageToken = res.nextPageToken;
        if (pageToken === "") break;
      }
      return found.slice(0, RECENT);
    },
  });
  const recent = changes.data ?? [];
  if (recent.length === 0) return null;
  return (
    <section className="dw-panel fleet-recent" aria-labelledby="fleet-recent-title">
      <div className="dw-panel-head">
        <h2 className="dw-panel-title" id="fleet-recent-title">
          {t`Recent fleet actions`}
        </h2>
        <Link to="/activity" className="fleet-recent-all">
          <Trans>Activity</Trans>
        </Link>
      </div>
      <ul className="fleet-recent-list">
        {recent.map((c) => {
          const servers = c.plan?.servers.length ?? 0;
          const when = c.applyTime ?? c.createTime;
          const by = c.appliedBy?.name || c.proposedBy?.name || "";
          return (
            <li key={c.id} className={cx("fleet-recent-item", c.state === ChangeState.FAILED && "is-failed")}>
              <Link to="/changes/$changeId" params={{ changeId: c.id }} className="fleet-recent-summary">
                {c.summary}
              </Link>
              <span className="fleet-recent-meta">
                {[
                  by,
                  plural(servers, { one: "# server", other: "# servers" }),
                  c.state === ChangeState.FAILED ? t`failed` : c.state === ChangeState.APPLYING ? t`running` : "",
                ]
                  .filter(Boolean)
                  .join(" · ")}
              </span>
              {when ? (
                <time className="fleet-recent-when" dateTime={timestampDate(when).toISOString()}>
                  {formatDay(timestampDate(when), i18n.locale)}
                </time>
              ) : null}
            </li>
          );
        })}
      </ul>
    </section>
  );
}
