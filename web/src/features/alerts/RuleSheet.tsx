// SPDX-License-Identifier: AGPL-3.0-only

import {
  type AlertRuleInfo,
  ChangeService,
  AlertRule_Condition as Condition,
  AlertRule_Severity as Severity,
} from "@dockwell/api";
import { Button, SegmentedControl, SelectField, Sheet, Switch } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useChannels } from "./useAlerts";

/** What a condition takes, as internal/resource/alert checks a rule. */
const PERCENT = new Set([Condition.DISK_FULL, Condition.MEMORY_HIGH]);
const NO_DURATION = new Set([Condition.CHANGE_FAILED, Condition.BACKUP_FAILED]);
const NO_REPEAT = new Set([Condition.CHANGE_FAILED]);

/** Durations a person picks from, as a rule writes them: the control plane's own spelling, so an unchanged one compares equal. */
const HOLDS = ["1m", "2m", "5m", "10m", "15m", "30m", "1h", "2h"];
const LATE = ["15m", "30m", "1h", "2h", "6h", "12h"];
/** How late a run is before it fires, when a rule doesn't say: the control plane's alert.DefaultLate. */
const DEFAULT_LATE = "1h";

/** The level a rule fires at when it doesn't say, as the control plane fills it in (alert.Rule.Validate). */
function defaultThreshold(condition: Condition): number {
  if (PERCENT.has(condition)) return 90;
  return condition === Condition.CERTIFICATE_EXPIRING ? 14 : 0;
}
const REPEATS = ["", "10m", "30m", "1h", "4h", "12h", "24h", "168h"];
const PERCENTS = [70, 75, 80, 85, 90, 95, 98];
const DAYS = [3, 7, 14, 21, 30, 60];

/** Minutes in a duration as a rule writes it, such as "1h30m". */
function minutesOf(d: string): number {
  const h = /(\d+)h/.exec(d)?.[1];
  const m = /(\d+)m/.exec(d)?.[1];
  return Number(h ?? 0) * 60 + Number(m ?? 0);
}

/**
 * Changes a rule, as a plan: on or off, the level and how long it has to hold, how
 * much it matters, where it sends and how often it says it again. Each choice is
 * from the values the control plane takes for that condition; a value set some other
 * way, as with the CLI, stays on the list as it is.
 */
export function RuleSheet({ rule, onClose }: { rule: AlertRuleInfo; onClose: () => void }) {
  const { t, i18n } = useLingui();
  const sheet = useChangeSheet();
  // A rule is the install's, in no project.
  const mayPlan = useMay()(ChangeService.method.proposeChange, { install: true });
  const channels = useChannels().data?.channels ?? [];
  const spec = rule.spec;
  const ruleName = rule.name;
  const condition = spec?.condition ?? Condition.UNSPECIFIED;
  const [on, setOn] = useState(!spec?.disabled);
  // What's shown is what's planned: an empty level or wait starts at the default the control plane would fill in,
  // so a rule left as it was plans no change either way.
  const [threshold, setThreshold] = useState(spec?.threshold || defaultThreshold(condition));
  const [duration, setDuration] = useState(spec?.duration || (condition === Condition.BACKUP_LATE ? DEFAULT_LATE : ""));
  const [severity, setSeverity] = useState(spec?.severity ?? Severity.WARNING);
  const [repeat, setRepeat] = useState(spec?.repeat ?? "");
  const [named, setNamed] = useState<string[]>(spec?.channels ?? []);
  const [some, setSome] = useState((spec?.channels.length ?? 0) > 0);

  // How a duration reads: "5 min", "2 h", "1 h 30 min", "a week".
  const span = (d: string) => {
    const total = minutesOf(d);
    if (total === 0) return t`At once`;
    const h = Math.floor(total / 60);
    const m = total % 60;
    if (h === 0) return t`${m} min`;
    if (m === 0) return t`${h} h`;
    return t`${h} h ${m} min`;
  };
  // How often it says it again: "Never", "Every 10 min", "Every hour", "Daily", "Weekly".
  const again = (d: string) => {
    switch (minutesOf(d)) {
      case 0:
        return t`Never`;
      case 60:
        return t`Every hour`;
      case 24 * 60:
        return t`Daily`;
      case 168 * 60:
        return t`Weekly`;
      default: {
        const each = span(d);
        return t`Every ${each}`;
      }
    }
  };
  const withCurrent = (list: string[], current: string) => (list.includes(current) ? list : [current, ...list]);
  const listFormat = new Intl.ListFormat(i18n.locale, { style: "short", type: "conjunction" });

  const review = () =>
    sheet.open({
      kind: "propose",
      summary: on ? t`Change the alert rule ${ruleName}` : t`Turn off the alert rule ${ruleName}`,
      edits: [
        {
          resource: { kind: "alert.rule", name: rule.name },
          spec: {
            kind: {
              case: "alertRule",
              value: {
                condition,
                threshold,
                duration: NO_DURATION.has(condition) ? "" : duration,
                severity,
                repeat: NO_REPEAT.has(condition) ? "" : repeat,
                channels: some ? named : [],
                disabled: !on,
              },
            },
          },
        },
      ],
      onClose: (change) => {
        if (change) onClose();
      },
    });

  return (
    <Sheet
      open
      onClose={onClose}
      title={rule.title}
      subtitle={rule.expression}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" onClick={review} disabled={some && named.length === 0} {...mayPlan.off}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="alerts-sheet">
        <Switch
          checked={on}
          onChange={setOn}
          label={t`Watch for this`}
          description={on ? undefined : t`Nothing fires, and what's open now resolves.`}
        />
        {on ? (
          <>
            <div className="alerts-sheet-row">
              {PERCENT.has(condition) ? (
                <SelectField
                  label={
                    condition === Condition.DISK_FULL ? t`When a disk is fuller than` : t`When memory in use is over`
                  }
                  value={String(threshold)}
                  options={(PERCENTS.includes(threshold) ? PERCENTS : [threshold, ...PERCENTS]).map((p) => ({
                    value: String(p),
                    label: `${p}%`,
                  }))}
                  onChange={(event) => setThreshold(Number(event.target.value))}
                />
              ) : null}
              {condition === Condition.CERTIFICATE_EXPIRING ? (
                <SelectField
                  label={t`When it expires within`}
                  value={String(threshold)}
                  options={(DAYS.includes(threshold) ? DAYS : [threshold, ...DAYS]).map((d) => ({
                    value: String(d),
                    label: t`${d} days`,
                  }))}
                  onChange={(event) => setThreshold(Number(event.target.value))}
                />
              ) : null}
              {condition === Condition.BACKUP_LATE ? (
                <SelectField
                  label={t`When a run is late by`}
                  value={duration}
                  options={withCurrent(LATE, duration).map((d) => ({ value: d, label: span(d) }))}
                  onChange={(event) => setDuration(event.target.value)}
                />
              ) : NO_DURATION.has(condition) ? null : (
                <SelectField
                  label={t`For`}
                  value={duration}
                  options={withCurrent(HOLDS, duration).map((d) => ({ value: d, label: span(d) }))}
                  onChange={(event) => setDuration(event.target.value)}
                />
              )}
            </div>
            <SegmentedControl<"warning" | "critical">
              label={t`How much it matters`}
              value={severity === Severity.CRITICAL ? "critical" : "warning"}
              onChange={(next) => setSeverity(next === "critical" ? Severity.CRITICAL : Severity.WARNING)}
              options={[
                { value: "warning", label: t`Warning` },
                { value: "critical", label: t`Critical` },
              ]}
            />
            {NO_REPEAT.has(condition) ? null : (
              <SelectField
                label={t`While it lasts, say it again`}
                value={repeat}
                options={withCurrent(REPEATS, repeat).map((d) => ({ value: d, label: again(d) }))}
                onChange={(event) => setRepeat(event.target.value)}
              />
            )}
            <SegmentedControl<"every" | "some">
              label={t`Sends to`}
              value={some ? "some" : "every"}
              onChange={(next) => setSome(next === "some")}
              options={[
                { value: "every", label: t`Every channel` },
                { value: "some", label: t`Only some` },
              ]}
            />
            {some ? (
              channels.length === 0 ? (
                <p className="alerts-muted">{t`No channel yet.`}</p>
              ) : (
                <fieldset className="alerts-sheet-channels">
                  <legend className="dw-sr">{t`Channels`}</legend>
                  {channels.map((c) => (
                    <Switch
                      key={c.name}
                      checked={named.includes(c.name)}
                      onChange={(checked) =>
                        setNamed((now) => (checked ? [...now, c.name] : now.filter((n) => n !== c.name)))
                      }
                      label={c.name}
                    />
                  ))}
                  {named.length > 0 ? (
                    <span className="alerts-muted">{listFormat.format(named)}</span>
                  ) : (
                    <span className="alerts-muted">{t`Pick at least one.`}</span>
                  )}
                </fieldset>
              )
            ) : null}
          </>
        ) : null}
      </div>
    </Sheet>
  );
}
