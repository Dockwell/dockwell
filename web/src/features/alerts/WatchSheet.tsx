// SPDX-License-Identifier: AGPL-3.0-only

import { type AlertCheckInfo, AlertCheckState, ChangeService, ChangeState } from "@dockwell/api";
import { Advanced, Button, SelectField, Sheet, StatusBadge, Switch, TextField } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { RouterButtonLink } from "../../lib/links";
import { durationWords } from "../apps/specEdit";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import {
  alertsTo,
  CHECK_DEFAULTS,
  checkStatus,
  DOWN_AFTER,
  EVERIES,
  holdOf,
  hostOf,
  nameFor,
  tookMs,
  useChecks,
  withDefaults,
} from "./checks";
import { useChannels, useRules } from "./useAlerts";

/** Why an address can't be checked, in the control plane's terms, or undefined when it can. */
function refusalOf(url: string): "empty" | "url" | "user" | undefined {
  if (url.trim() === "") return "empty";
  let u: URL;
  try {
    u = new URL(url.trim());
  } catch {
    return "url";
  }
  if ((u.protocol !== "http:" && u.protocol !== "https:") || u.host === "") return "url";
  if (u.username || u.password) return "user";
  return undefined;
}

/**
 * Watches an address, or changes how a check watches one, as a plan: the URL,
 * filled in from where it was opened, and the defaults in one line, with Change one
 * step away. Once a new check applies, it stays open on the check's first answer.
 */
export function WatchSheet({
  url: start = "",
  check,
  onClose,
}: {
  /** The address to watch, from the domain row it was opened on. */
  url?: string;
  /** The check to change, in place of making one. */
  check?: AlertCheckInfo;
  onClose: () => void;
}) {
  const { t, i18n } = useLingui();
  const sheet = useChangeSheet();
  // A check is the install's, in no project.
  const mayPlan = useMay()(ChangeService.method.proposeChange, { install: true });
  const checks = useChecks().data?.checks ?? [];
  const rules = useRules().data?.rules;
  const channels = useChannels().data?.channels;
  const was = withDefaults(check?.spec);
  const [url, setUrl] = useState(check?.spec?.address ?? start);
  const [every, setEvery] = useState(was.every);
  const [downAfter, setDownAfter] = useState(was.downAfter);
  // A check reaches the internet alone unless it says it's inside the network (ADR 0066).
  const [inside, setInside] = useState(check?.spec?.insideNetwork ?? false);
  const [changing, setChanging] = useState(false);
  const [touched, setTouched] = useState(false);
  // The check just made, by its resource ID, whose first answer this waits for.
  const [made, setMade] = useState<string>();

  const refusal = refusalOf(url);
  const host = hostOf(url.trim());
  const everyWord = (e: string) => {
    switch (e) {
      case "30s":
        return t`Every 30 seconds`;
      case "5m":
        return t`Every 5 minutes`;
      case "15m":
        return t`Every 15 minutes`;
      default:
        return t`Every minute`;
    }
  };
  const downWord = (n: number) => plural(n, { one: "down after # failure", other: "down after # failures" });
  const sends = alertsTo(rules, channels);
  const names =
    sends.to === "some"
      ? new Intl.ListFormat(i18n.locale, { style: "short", type: "conjunction" }).format(sends.names)
      : "";
  const sendsWord =
    sends.to === "every"
      ? t`alerts every channel`
      : sends.to === "some"
        ? t`alerts ${names}`
        : sends.to === "none"
          ? t`no channel yet, so nothing is sent`
          : sends.to === "off"
            ? t`the Address down rule is off, so nothing is sent`
            : "";
  const summary = [everyWord(every), downWord(downAfter), inside ? t`inside the network` : "", sendsWord]
    .filter(Boolean)
    .join(" · ");

  const error =
    !touched && refusal === "empty"
      ? undefined
      : refusal === "empty"
        ? t`Type the address to watch, such as https://shop.example.com/.`
        : refusal === "url"
          ? t`Use an http or https address, such as https://shop.example.com/.`
          : refusal === "user"
            ? t`Leave the user and password out: a check asks as a visitor does.`
            : undefined;

  const review = () => {
    const name =
      check?.name ??
      nameFor(
        url,
        checks.map((c) => c.name),
      );
    sheet.open({
      kind: "propose",
      summary: check
        ? inside
          ? t`Change the check of ${host}, inside the network`
          : t`Change the check of ${host}`
        : inside
          ? t`Watch ${host}, inside the network`
          : t`Watch ${host}`,
      edits: [
        {
          resource: { kind: "alert.check", name },
          spec: {
            kind: {
              case: "alertCheck",
              // What this sheet doesn't show stays as the check has it.
              value: {
                address: url.trim(),
                every,
                downAfter,
                insideNetwork: inside,
                timeout: check?.spec?.timeout ?? "",
                expectFrom: check?.spec?.expectFrom ?? 0,
                expectTo: check?.spec?.expectTo ?? 0,
                keyword: check?.spec?.keyword ?? "",
                keywordAbsent: check?.spec?.keywordAbsent ?? false,
              },
            },
          },
        },
      ],
      onClose: (change) => {
        if (change?.state !== ChangeState.APPLIED) return;
        const id = change.plan?.resources[0]?.id;
        if (check || !id) onClose();
        else setMade(id);
      },
    });
  };

  const remove = () => {
    if (!check) return;
    const name = check.name;
    sheet.open({
      kind: "propose",
      summary: t`Stop watching ${host}`,
      edits: [{ resource: { kind: "alert.check", name }, delete: true }],
      onClose: (change) => {
        if (change?.state === ChangeState.APPLIED) onClose();
      },
    });
  };

  if (made) return <FirstAnswer id={made} host={host} hold={durationWords(holdOf(rules))} onClose={onClose} />;
  // What counts as up, as this check has it: the CLI can set what the sheet doesn't.
  const from = was.expectFrom;
  const to = was.expectTo;
  const timeout = durationWords(was.timeout);

  return (
    <Sheet
      open
      onClose={onClose}
      title={check ? check.name : t`Watch this address`}
      subtitle={check ? undefined : t`Dockwell asks it from the control server, and alerts when it stops answering.`}
      footer={
        <>
          {check ? (
            <Button variant="danger" onClick={remove} {...mayPlan.off}>
              <Trans>Stop watching</Trans>
            </Button>
          ) : null}
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" onClick={review} disabled={refusal !== undefined} {...mayPlan.off}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <div className="alerts-sheet">
        <TextField
          label={t`Address`}
          value={url}
          mono
          inputMode="url"
          autoComplete="off"
          spellCheck={false}
          onChange={(event) => setUrl(event.target.value)}
          onBlur={() => setTouched(true)}
          error={error}
          hint={t`It shows in plans and on Activity, so leave tokens and passwords out of it.`}
        />
        <Advanced label={t`Change`} summary={summary} open={changing} onOpenChange={setChanging}>
          <div className="alerts-sheet-row">
            <SelectField
              label={t`How often`}
              value={every}
              options={
                (EVERIES as readonly string[]).includes(every)
                  ? EVERIES.map((e) => ({ value: e, label: everyWord(e) }))
                  : [{ value: every, label: every }, ...EVERIES.map((e) => ({ value: e, label: everyWord(e) }))]
              }
              onChange={(event) => setEvery(event.target.value)}
            />
            <SelectField
              label={t`Down after`}
              value={String(downAfter)}
              options={((DOWN_AFTER as readonly number[]).includes(downAfter)
                ? DOWN_AFTER
                : [downAfter, ...DOWN_AFTER]
              ).map((n) => ({
                value: String(n),
                label: plural(n, { one: "# failed run", other: "# failed runs" }),
              }))}
              onChange={(event) => setDownAfter(Number(event.target.value))}
            />
          </div>
          <Switch
            checked={inside}
            onChange={setInside}
            label={t`Inside the network`}
            description={t`Reach an address on the control server's private networks too. Without it, a check reaches the internet alone.`}
          />
          <p className="alerts-muted">
            <Trans>
              Up is an answer from {from} to {to} within {timeout}, so a redirect counts. Where the alert goes is the
              Address down rule's, under Rules.
            </Trans>
          </p>
        </Advanced>
      </div>
    </Sheet>
  );
}

/** A new check's first answer, as the control plane gets it, a moment after it applies. */
function FirstAnswer({ id, host, hold, onClose }: { id: string; host: string; hold: string; onClose: () => void }) {
  const { t, i18n } = useLingui();
  const check = useChecks({ poll: 1000 }).data?.checks.find((c) => c.id === id);
  const run = check?.runs[0];
  const waiting = !check || check.state === AlertCheckState.WAITING || !run;
  const state = checkStatus(check?.state);
  const ms = run ? tookMs(run) : undefined;
  return (
    <Sheet
      open
      onClose={onClose}
      title={t`Watching ${host}`}
      footer={
        <>
          <RouterButtonLink to="/alerts/checks" variant="secondary" onClick={onClose}>
            <Trans>See every check</Trans>
          </RouterButtonLink>
          <Button variant="primary" onClick={onClose}>
            <Trans>Done</Trans>
          </Button>
        </>
      }
    >
      <div className="alerts-sheet" aria-live="polite">
        {waiting ? (
          <p className="alerts-muted">{t`Asking ${host} from the control server…`}</p>
        ) : (
          <>
            <StatusBadge status={state.status} label={i18n._(state.word)} labelIsState />
            <p>
              {run.ok
                ? ms === undefined
                  ? t`It answered ${run.status}.`
                  : t`It answered ${run.status} in ${ms} ms.`
                : t`It didn't answer as it should: ${run.said}.`}
            </p>
            <p className="alerts-muted">
              {plural(check.spec?.downAfter || CHECK_DEFAULTS.downAfter, {
                one: "Dockwell asks again on its own, and the check reads down after # failed run.",
                other: "Dockwell asks again on its own, and the check reads down after # failed runs in a row.",
              })}{" "}
              {/* Up after 2 passes in a row: internal/control/checks, UpAfter. */}
              {t`It alerts once it has been down ${hold}, and reads up again after 2 passing runs in a row.`}
            </p>
          </>
        )}
      </div>
    </Sheet>
  );
}
