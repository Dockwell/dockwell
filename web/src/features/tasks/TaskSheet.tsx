// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { useQuery } from "@connectrpc/connect-query";
import { ChangeState, SettingsService, type Task, type TaskInfo, TaskSchema } from "@dockwell/api";
import {
  Advanced,
  Button,
  PHONE_QUERY,
  SegmentedControl,
  SelectField,
  Sheet,
  Switch,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useState } from "react";

import { nextRuns, parseCron } from "../../lib/cron";
import { usePlaces, useServices } from "../apps/useServices";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useFleetView } from "../servers/useFleet";
import { ScheduleField } from "./ScheduleField";
import { useTasks } from "./useTasks";

type Kind = "shell" | "app" | "request";

/** What a task's name takes, as the control plane checks a resource's name. */
const NAME = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/;

/** A name in the control plane's terms from words a person wrote: "Nightly sales report" is nightly-sales-report. */
export function slug(text: string): string {
  return text
    .normalize("NFKD")
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "")
    .slice(0, 63)
    .replace(/-+$/g, "");
}

/**
 * A name for a new task when nobody gave one: from what people call it, else from
 * what it runs (the program, the app, the host it asks), made unique among the
 * names already taken in its place.
 */
export function defaultName(
  display: string,
  run: { kind: Kind; command: string; app: string; url: string },
  taken: ReadonlySet<string>,
): string {
  let base = slug(display);
  if (!base) {
    if (run.kind === "request") {
      try {
        base = slug(new URL(run.url.trim()).hostname.split(".")[0] ?? "");
      } catch {
        base = "";
      }
      base = base ? `ping-${base}` : "request";
    } else {
      const program = run.command.trim().split(/\s+/)[0] ?? "";
      base = slug(program.split("/").pop() ?? "") || (run.kind === "app" ? slug(run.app) : "");
      if (run.kind === "app" && run.app && base !== slug(run.app)) base = slug(`${run.app}-${base}`);
    }
  }
  base = (base || "task").slice(0, 58).replace(/-+$/, "");
  if (!taken.has(base)) return base;
  for (let i = 2; ; i++) if (!taken.has(`${base}-${i}`)) return `${base}-${i}`;
}

/**
 * The zone a new task's schedule reads in: the install's from Settings, or UTC where
 * it has none, as the control plane gives a task that comes without one (ADR 0042).
 * Undefined until Settings answer, so it never says another.
 */
function useScheduleZone(): { zone: string | undefined; set: boolean } {
  const settings = useQuery(SettingsService.method.getSettings, {}).data?.settings;
  if (!settings) return { zone: undefined, set: false };
  return { zone: settings.timeZone || "UTC", set: settings.timeZone !== "" };
}

/** Every zone the browser knows, by name. */
function zones(): string[] {
  return typeof Intl.supportedValuesOf === "function" ? Intl.supportedValuesOf("timeZone") : ["UTC"];
}

/**
 * Makes or changes a scheduled task, as a plan: a shell command on a server, which
 * a passkey approves as it runs as root; a command in an app's image; or an HTTP
 * request. It asks what to run and when; the name, the folder, the zone, the
 * timeout and retries start at defaults, summed up under Advanced. Once a new one
 * is applied, onMade says which, so the page can open it where Run now is. Deleting
 * one is a plan too.
 */
export function TaskSheet({
  task,
  onClose,
  onMade,
}: {
  task?: TaskInfo;
  onClose: () => void;
  /** Called once a new task's change has applied, with its name and where it is. */
  onMade?: (name: string, scope: string) => void;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const sheet = useChangeSheet();
  const places = usePlaces();
  const services = useServices().data ?? [];
  const existing = useTasks().data?.tasks ?? [];
  const { servers } = useFleetView();
  const spec = task?.spec;
  const runs = spec?.runs;
  const [kind, setKind] = useState<Kind>(runs?.case ?? "shell");
  const [typedName, setTypedName] = useState<string>();
  const [display, setDisplay] = useState(spec?.displayName ?? "");
  const [command, setCommand] = useState(runs?.case === "shell" || runs?.case === "app" ? runs.value.command : "");
  const [folder, setFolder] = useState(runs?.case === "shell" ? runs.value.workingDir : "");
  const [app, setApp] = useState(runs?.case === "app" ? `${task?.resource?.scope}/${runs.value.service}` : "");
  const [url, setUrl] = useState(runs?.case === "request" ? runs.value.url : "");
  const [method, setMethod] = useState(runs?.case === "request" ? runs.value.method || "GET" : "GET");
  const [expect, setExpect] = useState(
    runs?.case === "request" && runs.value.expectStatus ? String(runs.value.expectStatus) : "",
  );
  const [schedule, setSchedule] = useState(spec?.schedule ?? "0 4 * * *");
  const [timeout, setTimeoutMinutes] = useState(String(Math.max(1, Math.round((spec?.timeoutSeconds ?? 600) / 60))));
  const [retries, setRetries] = useState(String(spec?.retries ?? 0));
  const [paused, setPaused] = useState(spec?.paused ?? false);
  const [skipMissed, setSkipMissed] = useState(spec?.skipMissed ?? false);
  // An edit keeps where a task runs, on every server, unless a person picks one:
  // "" leaves it as it is.
  const [server, setServer] = useState(task ? "" : (servers?.[0]?.id ?? ""));
  const [zone, setZone] = useState(spec?.timeZone ?? "");
  const scheduleZone = useScheduleZone();
  const [environment, setEnvironment] = useState(task?.resource?.scope ?? places.defaultEnvironment?.id ?? "");
  const [tried, setTried] = useState(false);
  const [advanced, setAdvanced] = useState(false);

  const chosenApp = services.find((s) => `${s.resource?.scope}/${s.resource?.name}` === app);
  const appServers = chosenApp?.deployments.map((d) => ({ id: d.serverId, name: d.serverName })) ?? [];
  const serverChoices = kind === "app" ? appServers : (servers ?? []).map((s) => ({ id: s.id, name: s.name }));
  const scope = kind === "app" ? (chosenApp?.resource?.scope ?? "") : environment;
  const taken = new Set(existing.filter((x) => x.resource?.scope === scope).map((x) => x.resource?.name ?? ""));
  const suggested = defaultName(display, { kind, command, app: chosenApp?.resource?.name ?? "", url }, taken);
  const name = typedName ?? suggested;
  const minutes = Number(timeout);
  // What the schedule's runs read in: the task's own zone, else the install's.
  const readsIn = zone || scheduleZone.zone || "UTC";
  const parsedSchedule = parseCron(schedule);
  // A schedule no date matches, such as 30 February, the control plane refuses: it never runs.
  const neverRuns = parsedSchedule ? nextRuns(parsedSchedule, Date.now(), "UTC", 1).length === 0 : false;
  const errors = {
    name: task || NAME.test(name) ? undefined : t`Use lowercase letters, digits and hyphens, such as nightly-report.`,
    command: kind !== "request" && command.trim() === "" ? t`Say what it runs.` : undefined,
    app: kind === "app" && !chosenApp ? t`Choose the app it runs in.` : undefined,
    url:
      kind === "request" && !/^https?:\/\/\S+$/.test(url.trim())
        ? t`Use a URL that starts with http:// or https://.`
        : undefined,
    expect:
      expect !== "" && !(Number(expect) >= 100 && Number(expect) <= 599)
        ? t`Use an HTTP status from 100 to 599, or leave it empty for any 2xx.`
        : undefined,
    schedule: !parsedSchedule
      ? t`Use cron's five fields, such as 0 4 * * *.`
      : neverRuns
        ? t`No date matches it, so it never runs.`
        : undefined,
    timeout: !(Number.isInteger(minutes) && minutes >= 1 && minutes <= 1440) ? t`Use 1 to 1,440 minutes.` : undefined,
  };
  const valid = Object.values(errors).every((e) => e === undefined);
  const resourceName = task?.resource?.name ?? name;
  const shownName = display.trim() || resourceName;

  const review = () => {
    setTried(true);
    if (!valid) {
      // What's wrong may be under Advanced: open it, so the error is in view.
      if (errors.name || errors.expect || errors.timeout) setAdvanced(true);
      return;
    }
    const value: Task = create(TaskSchema, {
      schedule: schedule.trim(),
      // Left out of a new task, the control plane gives it the install's zone.
      timeZone: zone,
      timeoutSeconds: minutes * 60,
      retries: Number(retries),
      paused,
      skipMissed,
      displayName: display.trim(),
      runs:
        kind === "shell"
          ? { case: "shell", value: { command: command.trim(), workingDir: folder.trim() } }
          : kind === "app"
            ? { case: "app", value: { service: chosenApp?.resource?.name ?? "", command: command.trim() } }
            : { case: "request", value: { url: url.trim(), method, expectStatus: expect === "" ? 0 : Number(expect) } },
    });
    sheet.open({
      kind: "propose",
      summary: task ? t`Change the task ${resourceName}` : t`Run ${shownName} on a schedule`,
      edits: [
        {
          resource: { kind: "task", scope, name: resourceName },
          spec: { kind: { case: "task", value } },
          // An empty list keeps the servers a task is on.
          servers: server ? [server] : [],
        },
      ],
      doneLabel: task ? undefined : t`Open ${shownName}`,
      onClose: (change) => {
        if (!change) return;
        if (!task && change.state === ChangeState.APPLIED) onMade?.(resourceName, scope);
        onClose();
      },
    });
  };
  const remove = () => {
    if (!task) return;
    sheet.open({
      kind: "propose",
      summary: t`Delete the task ${resourceName}`,
      edits: [{ resource: { kind: "task", scope: task.resource?.scope ?? "", name: resourceName }, delete: true }],
      onClose: (change) => {
        if (change) onClose();
      },
    });
  };

  // The defaults, in one line, so nobody opens Advanced to find out.
  const failing = {
    "0": t`no retries`,
    "1": t`one retry`,
    "2": t`two retries`,
    "3": t`three retries`,
  }[retries];
  const summary = [
    task ? "" : t`Named ${name}`,
    // Its own context: "in {0}" is also a time ("in 3 min"), which zh-CN puts after the number.
    kind === "shell" ? t({ message: `in ${folder.trim() || "/"}`, context: "the folder a task runs in" }) : "",
    t`stops after ${minutes || "?"} min`,
    failing ?? "",
    skipMissed ? t`skips missed runs` : "",
    kind === "request" ? `${method}${expect ? ` · ${expect}` : ""}` : "",
  ]
    .filter(Boolean)
    .join(" · ");

  return (
    <Sheet
      open
      onClose={onClose}
      title={task ? t`Edit ${display || resourceName}` : t`New task`}
      footer={
        <>
          {task ? (
            <Button variant="danger" onClick={remove}>
              <Trans>Delete</Trans>
            </Button>
          ) : null}
          <span className="tasks-fill" />
          <Button variant="secondary" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" onClick={review}>
            <Trans>Review plan</Trans>
          </Button>
        </>
      }
    >
      <form
        className="tasks-form"
        onSubmit={(event) => {
          event.preventDefault();
          review();
        }}
      >
        <SegmentedControl<Kind>
          label={t`What it runs`}
          value={kind}
          onChange={setKind}
          options={
            phone
              ? [
                  { value: "shell", label: t`Shell` },
                  { value: "app", label: t`In an app` },
                  { value: "request", label: t`Request` },
                ]
              : [
                  { value: "shell", label: t`Shell command` },
                  { value: "app", label: t`Command in an app` },
                  { value: "request", label: t`HTTP request` },
                ]
          }
        />
        {kind === "app" ? (
          <SelectField
            label={t`App`}
            value={app}
            error={tried ? errors.app : undefined}
            options={[
              { value: "", label: t`Choose an app` },
              ...services.map((s) => ({
                value: `${s.resource?.scope}/${s.resource?.name}`,
                label: places.severalProjects
                  ? `${s.resource?.name} · ${places.label(s.resource?.scope ?? "")}`
                  : (s.resource?.name ?? ""),
              })),
            ]}
            onChange={(event) => setApp(event.target.value)}
          />
        ) : null}
        {kind === "request" ? (
          <TextField
            label={t`URL`}
            value={url}
            mono
            placeholder="https://example.com/cron"
            error={tried ? errors.url : undefined}
            hint={t`The server asks it, so it reaches what the server reaches.`}
            onChange={(event) => setUrl(event.target.value)}
          />
        ) : (
          <TextField
            label={t`Command`}
            value={command}
            mono
            spellCheck={false}
            placeholder={kind === "shell" ? "find /var/log/app -mtime +7 -delete" : "/app/api report sales"}
            error={tried ? errors.command : undefined}
            hint={
              kind === "shell"
                ? t`It runs with /bin/sh as root, so applying it takes your passkey.`
                : t`It runs in a new container of the app's image, with its environment.`
            }
            onChange={(event) => setCommand(event.target.value)}
          />
        )}
        <ScheduleField
          value={schedule}
          onChange={setSchedule}
          zone={readsIn}
          error={tried ? errors.schedule : undefined}
        />
        <TextField
          label={t`Name`}
          value={display}
          optional
          placeholder={t`Nightly sales report`}
          onChange={(event) => setDisplay(event.target.value)}
        />
        {kind !== "app" && places.environments.length > 1 && !task ? (
          <SelectField
            label={t`Environment`}
            value={environment}
            options={places.environments.map((e) => ({ value: e.id, label: e.label }))}
            onChange={(event) => setEnvironment(event.target.value)}
          />
        ) : null}
        {serverChoices.length > 1 ? (
          <SelectField
            label={t`Server it runs on`}
            value={server}
            options={[
              ...(task
                ? [{ value: "", label: t`Where it runs now: ${task.servers.map((s) => s.serverName).join(", ")}` }]
                : []),
              ...serverChoices.map((s) => ({ value: s.id, label: s.name })),
            ]}
            onChange={(event) => setServer(event.target.value)}
          />
        ) : null}
        <Advanced open={advanced} onOpenChange={setAdvanced} summary={summary}>
          {task ? null : (
            <TextField
              label={t`Short name`}
              value={name}
              mono
              autoComplete="off"
              spellCheck={false}
              error={tried ? errors.name : undefined}
              hint={t`What the CLI and the API call it.`}
              onChange={(event) => setTypedName(event.target.value)}
            />
          )}
          {kind === "shell" ? (
            <TextField
              label={t`Folder it runs in`}
              value={folder}
              mono
              optional
              placeholder="/"
              onChange={(event) => setFolder(event.target.value)}
            />
          ) : null}
          {kind === "request" ? (
            <div className="tasks-form-row">
              <SelectField
                label={t`Method`}
                value={method}
                options={["GET", "HEAD", "POST"].map((m) => ({ value: m, label: m }))}
                onChange={(event) => setMethod(event.target.value)}
              />
              <TextField
                label={t`Status it expects`}
                value={expect}
                mono
                optional
                placeholder={t`Any 2xx`}
                error={tried ? errors.expect : undefined}
                onChange={(event) => setExpect(event.target.value)}
              />
            </div>
          ) : null}
          <SelectField
            label={t`Time zone`}
            value={zone}
            options={[
              ...(spec?.timeZone
                ? []
                : [
                    {
                      value: "",
                      label:
                        scheduleZone.zone === undefined
                          ? t`The install's`
                          : scheduleZone.set
                            ? t`The install's, ${scheduleZone.zone}`
                            : t`UTC, as the install has none set`,
                    },
                  ]),
              ...zones().map((z) => ({ value: z, label: z })),
            ]}
            onChange={(event) => setZone(event.target.value)}
          />
          <div className="tasks-form-row">
            <TextField
              label={t`Timeout, in minutes`}
              value={timeout}
              mono
              inputMode="numeric"
              error={tried ? errors.timeout : undefined}
              onChange={(event) => setTimeoutMinutes(event.target.value)}
            />
            <SelectField
              label={t`If a run fails`}
              value={retries}
              options={[
                { value: "0", label: t`Leave it` },
                { value: "1", label: t`Try once more, a minute later` },
                { value: "2", label: t`Try twice more` },
                { value: "3", label: t`Try three times more` },
              ]}
              onChange={(event) => setRetries(event.target.value)}
            />
          </div>
          <Switch
            checked={skipMissed}
            onChange={setSkipMissed}
            label={t`Skip runs missed while the server was off`}
            description={t`Otherwise one runs as the server comes back.`}
          />
          {task ? (
            <Switch
              checked={paused}
              onChange={setPaused}
              label={t`Paused`}
              description={t`It runs only when you run it now.`}
            />
          ) : null}
        </Advanced>
        <button type="submit" hidden />
      </form>
    </Sheet>
  );
}
