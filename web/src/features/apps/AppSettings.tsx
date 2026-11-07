// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { useQuery } from "@connectrpc/connect-query";
import {
  type Change,
  ChangeState,
  type Container,
  type EditSchema,
  HealthcheckSchema,
  MountSchema,
  PortMappingSchema,
  Protocol,
  ResourceService,
  ResourcesSchema,
  RestartPolicy,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Icon,
  Panel,
  PHONE_QUERY,
  SegmentedControl,
  SelectField,
  Switch,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, type ReactNode, useId, useState } from "react";

import { useChangeSheet } from "../changes/ChangeSheet";
import { useApp } from "./AppContext";
import { DeletePanel } from "./AppOverview";
import { appOf, environmentOf, isMoving, nameOf } from "./app";
import { isServerPath, pathReach, validPort } from "./form";
import {
  type CheckKind,
  checkCommand,
  checkKindOf,
  joinWords,
  serviceEdit,
  splitWords,
  validCpus,
  validDuration,
  validMemory,
} from "./specEdit";
import { useAppWords } from "./words";

type SectionId = "general" | "health" | "resources" | "ports" | "mounts";

type Edits = MessageInitShape<typeof EditSchema>[];

/**
 * How an app runs, to change after it's made, as App-Settings.dc.html lays it
 * out: what it runs and how it restarts, its health check, its limits, its
 * ports, its volumes and mounts, and deleting it. Each section edits on its
 * own and plans its own change, from the app as it was read: the plan says
 * what it replaces, and nothing changes until it's applied.
 */
export function AppSettings() {
  const { t } = useLingui();
  const { service, state } = useApp();
  const phone = useMediaQuery(PHONE_QUERY);
  const sheet = useChangeSheet();
  const [editing, setEditing] = useState<SectionId>();
  const app = appOf(service);
  const name = nameOf(service);
  const root = service.hostAccess.some((a) => a.sameAsRoot);
  const moving = isMoving(state);

  if (!app) {
    return (
      <Callout tone="warning" title={t`${name}'s spec can't be read`}>
        {service.specUnreadable}
      </Callout>
    );
  }

  const plan = (summary: string, edits: Edits) =>
    sheet.open({
      kind: "propose",
      summary,
      edits,
      onClose: (change: Change | undefined) => {
        if (change && (change.state === ChangeState.APPLIED || change.state === ChangeState.APPLYING)) {
          setEditing(undefined);
        }
      },
      fix: (reason) =>
        reason === "plan_stale"
          ? t`${name} changed after you started. Close this and review your edits again: they apply to what it is now.`
          : undefined,
    });

  const sections: { id: SectionId | "danger"; label: string }[] = [
    { id: "general", label: t`General` },
    { id: "health", label: t`Health check` },
    { id: "resources", label: t`Resources` },
    { id: "ports", label: t`Ports` },
    { id: "mounts", label: t`Volumes and mounts` },
    { id: "danger", label: t`Danger zone` },
  ];
  const common = { app, name, root, moving, plan, onCancel: () => setEditing(undefined) };
  return (
    <div className="app-settings">
      {phone ? null : (
        <nav aria-label={t`Settings sections`} className="app-settings-nav">
          {sections.map((s) => (
            <a key={s.id} href={`#${s.id}`} className="app-settings-nav-item">
              {s.label}
            </a>
          ))}
        </nav>
      )}
      <div className="app-settings-main">
        <Section id="general" title={t`General`} editing={editing === "general"} onEdit={() => setEditing("general")}>
          {editing === "general" ? <GeneralForm {...common} /> : <GeneralFacts app={app} />}
        </Section>
        <Section id="health" title={t`Health check`} editing={editing === "health"} onEdit={() => setEditing("health")}>
          {editing === "health" ? <HealthForm {...common} /> : <HealthFacts app={app} />}
        </Section>
        <Section
          id="resources"
          title={t`Resources`}
          editing={editing === "resources"}
          onEdit={() => setEditing("resources")}
        >
          {editing === "resources" ? <ResourcesForm {...common} /> : <ResourcesFacts app={app} />}
        </Section>
        <Section id="ports" title={t`Ports`} editing={editing === "ports"} onEdit={() => setEditing("ports")}>
          {editing === "ports" ? <PortsForm {...common} /> : <PortsFacts app={app} />}
        </Section>
        <Section
          id="mounts"
          title={t`Volumes and mounts`}
          editing={editing === "mounts"}
          onEdit={() => setEditing("mounts")}
        >
          {editing === "mounts" ? <MountsForm {...common} /> : <MountsFacts app={app} />}
        </Section>
        <div id="danger" className="app-settings-anchor">
          <DeletePanel title={t`Danger zone`} />
        </div>
      </div>
    </div>
  );
}

function Section({
  id,
  title,
  editing,
  onEdit,
  children,
}: {
  id: SectionId;
  title: string;
  editing: boolean;
  onEdit: () => void;
  children: ReactNode;
}) {
  const { t } = useLingui();
  return (
    <div id={id} className="app-settings-anchor">
      <Panel
        title={title}
        className={cx("app-settings-section", editing && "is-editing")}
        actions={
          editing ? null : (
            <Button size="sm" variant="ghost" icon="edit" aria-label={t`Edit ${title}`} onClick={onEdit}>
              <Trans>Edit</Trans>
            </Button>
          )
        }
      >
        {children}
      </Panel>
    </div>
  );
}

interface FormProps {
  app: Container;
  name: string;
  root: boolean;
  moving: boolean;
  plan: (summary: string, edits: Edits) => void;
  onCancel: () => void;
}

/** Cancel and Review plan, after what applying this asks of you. */
function Foot({
  name,
  root,
  moving,
  limits,
  unchanged,
  onCancel,
}: {
  name: string;
  root: boolean;
  moving: boolean;
  /** Limits never ask for a passkey, whatever the app reaches. */
  limits?: boolean;
  unchanged: boolean;
  onCancel: () => void;
}) {
  const { t } = useLingui();
  const note = moving
    ? t`A change is applying to ${name}. Review this once it ends.`
    : unchanged
      ? t`Nothing has changed yet.`
      : root && limits
        ? t`A limit never asks for your passkey, whatever ${name} reaches.`
        : root
          ? t`${name} is as good as root on its server, so applying this asks for your passkey.`
          : t`Review plan shows what this replaces first. Nothing changes until you apply it.`;
  return (
    <div className="app-settings-foot">
      <p className={cx("app-settings-foot-note", root && !limits && !unchanged && !moving && "is-root")}>
        {root && !limits && !unchanged && !moving ? <Icon name="key" size={14} /> : null}
        <span>{note}</span>
      </p>
      <div className="app-settings-foot-buttons">
        <Button type="button" variant="ghost" onClick={onCancel}>
          <Trans>Cancel</Trans>
        </Button>
        <Button type="submit" variant="primary" icon="commit" disabled={moving || unchanged}>
          <Trans>Review plan</Trans>
        </Button>
      </div>
    </div>
  );
}

function Facts({ children }: { children: ReactNode }) {
  return <dl className="app-facts app-settings-facts">{children}</dl>;
}

function Fact({ term, children }: { term: string; children: ReactNode }) {
  return (
    <>
      <dt>{term}</dt>
      <dd>{children}</dd>
    </>
  );
}

function Muted({ children }: { children: ReactNode }) {
  return <span className="app-muted">{children}</span>;
}

// General: what it runs, as whom, where, and how it restarts.

function GeneralFacts({ app }: { app: Container }) {
  const { t } = useLingui();
  const words = useAppWords();
  return (
    <Facts>
      <Fact term={t`Command`}>
        {app.command.length > 0 ? <code>{joinWords(app.command)}</code> : <Muted>{t`The image's own`}</Muted>}
      </Fact>
      <Fact term={t`Entrypoint`}>
        {app.noEntrypoint ? (
          <Muted>{t`None, not even the image's`}</Muted>
        ) : app.entrypoint.length > 0 ? (
          <code>{joinWords(app.entrypoint)}</code>
        ) : (
          <Muted>{t`The image's own`}</Muted>
        )}
      </Fact>
      <Fact term={t`User`}>{app.user ? <code>{app.user}</code> : <Muted>{t`The image's own`}</Muted>}</Fact>
      <Fact term={t`Directory`}>
        {app.workingDir ? <code>{app.workingDir}</code> : <Muted>{t`The image's own`}</Muted>}
      </Fact>
      <Fact term={t`Restarts`}>{words.restart(app.restart)}</Fact>
    </Facts>
  );
}

const RESTARTS = [RestartPolicy.UNLESS_STOPPED, RestartPolicy.ALWAYS, RestartPolicy.ON_FAILURE, RestartPolicy.NO];

function GeneralForm({ app, name, root, moving, plan, onCancel }: FormProps) {
  const { t } = useLingui();
  const words = useAppWords();
  const service = useApp().service;
  const [command, setCommand] = useState(joinWords(app.command));
  const [entrypoint, setEntrypoint] = useState(joinWords(app.entrypoint));
  const [noEntrypoint, setNoEntrypoint] = useState(app.noEntrypoint);
  const [user, setUser] = useState(app.user);
  const [dir, setDir] = useState(app.workingDir);
  const [restart, setRestart] = useState<RestartPolicy>(
    app.restart === RestartPolicy.UNSPECIFIED ? RestartPolicy.UNLESS_STOPPED : app.restart,
  );
  const [errors, setErrors] = useState<Partial<Record<"command" | "entrypoint" | "dir", string>>>({});
  const unclosed = t`A quote isn't closed. Close it, or leave it out.`;
  const unchanged =
    command === joinWords(app.command) &&
    entrypoint === joinWords(app.entrypoint) &&
    noEntrypoint === app.noEntrypoint &&
    user === app.user &&
    dir === app.workingDir &&
    restart === (app.restart === RestartPolicy.UNSPECIFIED ? RestartPolicy.UNLESS_STOPPED : app.restart);

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const cmd = splitWords(command);
    const entry = splitWords(entrypoint);
    const next = {
      command: cmd ? undefined : unclosed,
      entrypoint: entry || noEntrypoint ? undefined : unclosed,
      dir: dir.trim() === "" || dir.trim().startsWith("/") ? undefined : t`Use an absolute path, such as /app.`,
    };
    setErrors(next);
    if (next.command || next.entrypoint || next.dir || !cmd || unchanged) return;
    plan(t`Change how ${name} runs`, [
      // Its own environment's values stay as they are, unread.
      serviceEdit(service, (a) => {
        a.command = cmd;
        a.noEntrypoint = noEntrypoint;
        a.entrypoint = noEntrypoint ? [] : (entry ?? []);
        a.user = user.trim();
        a.workingDir = dir.trim();
        a.restart = restart;
        if (restart !== RestartPolicy.ON_FAILURE) a.restartRetries = 0;
      }),
    ]);
  };

  return (
    <form className="app-settings-form" aria-label={t`Edit General`} onSubmit={submit} noValidate>
      <div className="app-settings-grid">
        <TextField
          label={t`Command`}
          mono
          value={command}
          placeholder={t`The image's own`}
          hint={t`Leave it empty to run the image's own.`}
          error={errors.command}
          onChange={(event) => setCommand(event.target.value)}
          spellCheck={false}
          autoComplete="off"
        />
        <div className="app-settings-field">
          <TextField
            label={t`Entrypoint`}
            mono
            value={noEntrypoint ? "" : entrypoint}
            disabled={noEntrypoint}
            placeholder={noEntrypoint ? t`None` : t`The image's own`}
            error={errors.entrypoint}
            onChange={(event) => setEntrypoint(event.target.value)}
            spellCheck={false}
            autoComplete="off"
          />
          <Switch
            label={t`Run no entrypoint, not even the image's`}
            checked={noEntrypoint}
            onChange={setNoEntrypoint}
          />
        </div>
        <TextField
          label={t`User`}
          mono
          value={user}
          placeholder={t`The image's own`}
          hint={t`A name or a number, such as 1000.`}
          onChange={(event) => setUser(event.target.value)}
          spellCheck={false}
          autoComplete="off"
        />
        <TextField
          label={t`Directory`}
          mono
          value={dir}
          placeholder={t`The image's own`}
          error={errors.dir}
          onChange={(event) => setDir(event.target.value)}
          spellCheck={false}
          autoComplete="off"
        />
        <SelectField
          label={t`Restarts`}
          value={String(restart)}
          onChange={(event) => setRestart(Number(event.target.value) as RestartPolicy)}
          options={RESTARTS.map((p) => ({ value: String(p), label: words.restart(p) }))}
        />
      </div>
      <Foot name={name} root={root} moving={moving} unchanged={unchanged} onCancel={onCancel} />
    </form>
  );
}

// Health check: how Dockwell and Docker tell it works.

function HealthFacts({ app }: { app: Container }) {
  const { t } = useLingui();
  const check = app.healthcheck;
  const kind = checkKindOf(check);
  const retries = check?.retries ?? 0;
  return (
    <Facts>
      <Fact term={t`Check`}>
        {kind === "http" ? (
          <span>
            <code>GET {check?.http?.path || "/"}</code> <Trans>on port {check?.http?.port}</Trans>
          </span>
        ) : kind === "tcp" ? (
          <Trans>A connection to port {check?.tcp?.port}</Trans>
        ) : kind === "command" ? (
          <code>{checkCommand(check)}</code>
        ) : kind === "none" ? (
          <Muted>{t`Turned off. Without one, Running only means it started.`}</Muted>
        ) : (
          <Muted>{t`None set here. The image may have one.`}</Muted>
        )}
      </Fact>
      {check && kind !== "none" ? (
        <>
          <Fact term={t`Every`}>{check.interval || <Muted>{t`Docker's default`}</Muted>}</Fact>
          <Fact term={t`Timeout`}>{check.timeout || <Muted>{t`Docker's default`}</Muted>}</Fact>
          <Fact term={t`Unhealthy after`}>
            {check.retries > 0 ? (
              <Trans>{retries} failed checks in a row</Trans>
            ) : (
              <Trans>3 failed checks in a row</Trans>
            )}
          </Fact>
          {check.startPeriod ? <Fact term={t`Grace at start`}>{check.startPeriod}</Fact> : null}
        </>
      ) : null}
    </Facts>
  );
}

function HealthForm({ app, name, root, moving, plan, onCancel }: FormProps) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const service = useApp().service;
  const check = app.healthcheck;
  const [kind, setKind] = useState<CheckKind>(checkKindOf(check));
  const [port, setPort] = useState(String(check?.http?.port || check?.tcp?.port || ""));
  const [path, setPath] = useState(check?.http?.path ?? "");
  const [command, setCommand] = useState(checkCommand(check));
  const [interval, setEvery] = useState(check?.interval ?? "");
  const [timeout, setLimit] = useState(check?.timeout ?? "");
  const [retries, setRetries] = useState(check?.retries ? String(check.retries) : "");
  const [start, setStart] = useState(check?.startPeriod ?? "");
  const [errors, setErrors] = useState<
    Partial<Record<"port" | "command" | "interval" | "timeout" | "start" | "retries", string>>
  >({});
  const timed = kind === "http" || kind === "tcp" || kind === "command";
  const checkKinds: { value: CheckKind; label: string }[] = [
    { value: "http", label: t`HTTP` },
    { value: "tcp", label: t`TCP` },
    { value: "command", label: t({ message: "Command", context: "health check" }) },
    { value: "image", label: t`The image's` },
    { value: "none", label: t({ message: "None", context: "health check" }) },
  ];
  const unchanged =
    kind === checkKindOf(check) &&
    port === String(check?.http?.port || check?.tcp?.port || "") &&
    path === (check?.http?.path ?? "") &&
    command === checkCommand(check) &&
    interval === (check?.interval ?? "") &&
    timeout === (check?.timeout ?? "") &&
    retries === (check?.retries ? String(check.retries) : "") &&
    start === (check?.startPeriod ?? "");

  const submit = (event: FormEvent) => {
    event.preventDefault();
    const duration = (v: string) =>
      v.trim() === "" || validDuration(v) ? undefined : t`Use a duration, such as 10s or 1m.`;
    const next = {
      port: (kind === "http" || kind === "tcp") && !validPort(port) ? t`Use a port from 1 to 65535.` : undefined,
      command:
        kind === "command" && command.trim() === ""
          ? t`Say what to run, such as curl -f http://localhost/.`
          : undefined,
      interval: timed ? duration(interval) : undefined,
      timeout: timed ? duration(timeout) : undefined,
      start: timed ? duration(start) : undefined,
      retries:
        timed && retries.trim() !== "" && !/^[1-9]\d{0,2}$/.test(retries.trim())
          ? t`Use a whole number from 1.`
          : undefined,
    };
    setErrors(next);
    if (Object.values(next).some(Boolean) || unchanged) return;
    plan(t`Change the health check of ${name}`, [
      serviceEdit(service, (a) => {
        const timing = timed
          ? {
              interval: interval.trim(),
              timeout: timeout.trim(),
              startPeriod: start.trim(),
              retries: retries.trim() === "" ? 0 : Number(retries),
            }
          : {};
        switch (kind) {
          case "image":
            a.healthcheck = undefined;
            break;
          case "none":
            a.healthcheck = create(HealthcheckSchema, { test: ["NONE"] });
            break;
          case "http":
            a.healthcheck = create(HealthcheckSchema, {
              ...timing,
              http: { port: Number(port), path: path.trim(), host: check?.http?.host ?? "" },
            });
            break;
          case "tcp":
            a.healthcheck = create(HealthcheckSchema, { ...timing, tcp: { port: Number(port) } });
            break;
          case "command":
            a.healthcheck = create(HealthcheckSchema, { ...timing, test: ["CMD-SHELL", command.trim()] });
            break;
        }
      }),
    ]);
  };

  return (
    <form className="app-settings-form" aria-label={t`Edit Health check`} onSubmit={submit} noValidate>
      {phone ? (
        <SelectField
          label={t`How it's checked`}
          value={kind}
          onChange={(event) => setKind(event.target.value as CheckKind)}
          options={checkKinds}
        />
      ) : (
        <SegmentedControl<CheckKind> label={t`How it's checked`} value={kind} onChange={setKind} options={checkKinds} />
      )}
      <p className="app-settings-help">
        {kind === "http"
          ? t`Dockwell's agent asks the app for the path on its port. Any status from 200 to 399 passes, and the image needs no curl.`
          : kind === "tcp"
            ? t`Dockwell's agent connects to the port. It passes once the app takes the connection.`
            : kind === "command"
              ? t`Docker runs this in the container. It passes when it exits with 0.`
              : kind === "image"
                ? t`The image's own check, if it has one. Without one, Running only means it started.`
                : t`No check at all, not even the image's. Running only means it started.`}
      </p>
      {kind === "http" || kind === "tcp" || kind === "command" ? (
        <div className="app-settings-grid">
          {kind === "http" || kind === "tcp" ? (
            <TextField
              label={t`Port in the app`}
              mono
              inputMode="numeric"
              value={port}
              error={errors.port}
              onChange={(event) => setPort(event.target.value)}
            />
          ) : null}
          {kind === "http" ? (
            <TextField
              label={t`Path`}
              mono
              value={path}
              placeholder="/"
              onChange={(event) => setPath(event.target.value)}
              spellCheck={false}
            />
          ) : null}
          {kind === "command" ? (
            <TextField
              label={t({ message: "Command", context: "health check" })}
              mono
              value={command}
              error={errors.command}
              onChange={(event) => setCommand(event.target.value)}
              spellCheck={false}
              autoComplete="off"
            />
          ) : null}
          <TextField
            label={t`Every`}
            mono
            value={interval}
            placeholder={kind === "command" ? "30s" : "10s"}
            error={errors.interval}
            onChange={(event) => setEvery(event.target.value)}
          />
          <TextField
            label={t`Timeout`}
            mono
            value={timeout}
            placeholder={kind === "command" ? "30s" : "5s"}
            error={errors.timeout}
            onChange={(event) => setLimit(event.target.value)}
          />
          <TextField
            label={t`Unhealthy after`}
            mono
            inputMode="numeric"
            value={retries}
            placeholder="3"
            suffix={t`failures`}
            error={errors.retries}
            onChange={(event) => setRetries(event.target.value)}
          />
          <TextField
            label={t`Grace at start`}
            mono
            value={start}
            placeholder="0s"
            hint={t`Failures in this time don't count.`}
            error={errors.start}
            onChange={(event) => setStart(event.target.value)}
          />
        </div>
      ) : null}
      <Foot name={name} root={root} moving={moving} unchanged={unchanged} onCancel={onCancel} />
    </form>
  );
}

// Resources: what it may use of its server.

function ResourcesFacts({ app }: { app: Container }) {
  const { t } = useLingui();
  const r = app.resources;
  return (
    <Facts>
      <Fact term={t`CPU`}>{r?.cpus ? <Trans>{r.cpus} vCPU</Trans> : <Muted>{t`No limit`}</Muted>}</Fact>
      <Fact term={t`Memory`}>{r?.memory ? <code>{r.memory}</code> : <Muted>{t`No limit`}</Muted>}</Fact>
      <Fact term={t`Kept for it`}>
        {r?.memoryReservation ? <code>{r.memoryReservation}</code> : <Muted>{t`Nothing set aside`}</Muted>}
      </Fact>
    </Facts>
  );
}

function ResourcesForm({ app, name, root, moving, plan, onCancel }: FormProps) {
  const { t } = useLingui();
  const service = useApp().service;
  const r = app.resources;
  const [cpus, setCpus] = useState(r?.cpus ?? "");
  const [memory, setMemory] = useState(r?.memory ?? "");
  const [reserved, setReserved] = useState(r?.memoryReservation ?? "");
  const [errors, setErrors] = useState<Partial<Record<"cpus" | "memory" | "reserved", string>>>({});
  const unchanged =
    cpus === (r?.cpus ?? "") && memory === (r?.memory ?? "") && reserved === (r?.memoryReservation ?? "");
  const submit = (event: FormEvent) => {
    event.preventDefault();
    const mem = t`Use an amount from 6M, such as 512M or 2G.`;
    const next = {
      cpus: cpus.trim() === "" || validCpus(cpus) ? undefined : t`Use a number of CPUs above 0, such as 0.5 or 2.`,
      memory: memory.trim() === "" || validMemory(memory) ? undefined : mem,
      reserved: reserved.trim() === "" || validMemory(reserved) ? undefined : mem,
    };
    setErrors(next);
    if (next.cpus || next.memory || next.reserved || unchanged) return;
    plan(t`Change the limits of ${name}`, [
      serviceEdit(service, (a) => {
        const none = cpus.trim() === "" && memory.trim() === "" && reserved.trim() === "";
        a.resources = none
          ? undefined
          : create(ResourcesSchema, { cpus: cpus.trim(), memory: memory.trim(), memoryReservation: reserved.trim() });
      }),
    ]);
  };
  return (
    <form className="app-settings-form" aria-label={t`Edit Resources`} onSubmit={submit} noValidate>
      <div className="app-settings-grid">
        <TextField
          label={t`CPU limit`}
          mono
          inputMode="decimal"
          value={cpus}
          placeholder={t`No limit`}
          suffix="vCPU"
          error={errors.cpus}
          onChange={(event) => setCpus(event.target.value)}
        />
        <TextField
          label={t`Memory limit`}
          mono
          value={memory}
          placeholder={t`No limit`}
          hint={t`Such as 512M or 2G.`}
          error={errors.memory}
          onChange={(event) => setMemory(event.target.value)}
        />
        <TextField
          label={t`Memory kept for it`}
          mono
          value={reserved}
          placeholder={t`None`}
          hint={t`What Docker leaves it when the server runs short.`}
          error={errors.reserved}
          onChange={(event) => setReserved(event.target.value)}
        />
      </div>
      <p className="app-settings-help">
        <Trans>Docker changes a limit on the running container, except to lift one, which replaces it.</Trans>
      </p>
      <Foot name={name} root={root} moving={moving} limits unchanged={unchanged} onCancel={onCancel} />
    </form>
  );
}

// Ports: what it publishes on its server.

function PortsFacts({ app }: { app: Container }) {
  const { t } = useLingui();
  const words = useAppWords();
  if (app.ports.length === 0) {
    return (
      <p className="app-settings-empty">
        <Trans>None. Nothing outside the server reaches it, but what shares a network with it.</Trans>
      </p>
    );
  }
  return (
    <ul className="app-settings-list">
      {app.ports.map((p) => (
        <li key={`${p.hostIp}:${p.hostPort}/${p.protocol}`}>
          <code>{p.hostIp ? `${p.hostIp}:${p.hostPort}` : p.hostPort}</code>
          <Icon name="arrow-right" size={12} />
          <code>{p.containerPort}</code>
          <span className="app-muted">{words.protocol(p.protocol)}</span>
          <span className="dw-sr">{t`on the server, to the app`}</span>
        </li>
      ))}
    </ul>
  );
}

interface PortRow {
  key: number;
  host: string;
  inApp: string;
  udp: boolean;
  hostIp: string;
}

function PortsForm({ app, name, root, moving, plan, onCancel }: FormProps) {
  const { t } = useLingui();
  const service = useApp().service;
  const first = app.ports.map(
    (p, i): PortRow => ({
      key: i,
      host: String(p.hostPort),
      inApp: String(p.containerPort),
      udp: p.protocol === Protocol.UDP,
      hostIp: p.hostIp,
    }),
  );
  const [rows, setRows] = useState<PortRow[]>(first);
  const [next, setNext] = useState(first.length);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const unchanged = JSON.stringify(rows) === JSON.stringify(first);
  const change = (key: number, part: Partial<PortRow>) =>
    setRows((was) => was.map((r) => (r.key === key ? { ...r, ...part } : r)));
  const submit = (event: FormEvent) => {
    event.preventDefault();
    const found: Record<string, string> = {};
    const seen = new Set<string>();
    for (const row of rows) {
      if (!validPort(row.host)) found[`${row.key}.host`] = t`Use a port from 1 to 65535.`;
      if (!validPort(row.inApp)) found[`${row.key}.app`] = t`Use a port from 1 to 65535.`;
      const id = `${row.hostIp}:${row.host.trim()}/${row.udp}`;
      if (seen.has(id)) found[`${row.key}.host`] = t`It publishes this port already.`;
      seen.add(id);
    }
    setErrors(found);
    if (Object.keys(found).length > 0 || unchanged) return;
    plan(t`Change the ports of ${name}`, [
      serviceEdit(service, (a) => {
        a.ports = rows.map((r) =>
          create(PortMappingSchema, {
            hostPort: Number(r.host),
            containerPort: Number(r.inApp),
            protocol: r.udp ? Protocol.UDP : Protocol.TCP,
            hostIp: r.hostIp,
          }),
        );
      }),
    ]);
  };
  return (
    <form className="app-settings-form" aria-label={t`Edit Ports`} onSubmit={submit} noValidate>
      {rows.length === 0 ? (
        <p className="app-settings-empty">
          <Trans>It publishes none. Add one to reach it from outside the server.</Trans>
        </p>
      ) : (
        <ul className="app-settings-rows">
          {rows.map((row, i) => {
            const n = i + 1;
            return (
              <li key={row.key} className="app-settings-row is-port">
                <TextField
                  aria-label={t`Port ${n} on the server`}
                  label={i === 0 ? t`On the server` : undefined}
                  mono
                  inputMode="numeric"
                  value={row.host}
                  error={errors[`${row.key}.host`]}
                  onChange={(event) => change(row.key, { host: event.target.value })}
                />
                <TextField
                  aria-label={t`Port ${n} in the app`}
                  label={i === 0 ? t`In the app` : undefined}
                  mono
                  inputMode="numeric"
                  value={row.inApp}
                  error={errors[`${row.key}.app`]}
                  onChange={(event) => change(row.key, { inApp: event.target.value })}
                />
                <Switch label="UDP" checked={row.udp} onChange={(udp) => change(row.key, { udp })} />
                <Button
                  type="button"
                  size="sm"
                  variant="ghost"
                  icon="trash"
                  aria-label={t`Remove port ${n}`}
                  onClick={() => setRows((was) => was.filter((r) => r.key !== row.key))}
                />
              </li>
            );
          })}
        </ul>
      )}
      <div>
        <Button
          type="button"
          variant="secondary"
          icon="plus"
          onClick={() => {
            setRows((was) => [...was, { key: next, host: "", inApp: "", udp: false, hostIp: "" }]);
            setNext(next + 1);
          }}
        >
          <Trans>Add a port</Trans>
        </Button>
      </div>
      <Foot name={name} root={root} moving={moving} unchanged={unchanged} onCancel={onCancel} />
    </form>
  );
}

// Volumes and mounts: what it keeps, and what of the server it reaches.

function MountsFacts({ app }: { app: Container }) {
  const { t } = useLingui();
  if (app.mounts.length === 0) {
    return (
      <p className="app-settings-empty">
        <Trans>None. What it writes goes when its container is replaced.</Trans>
      </p>
    );
  }
  return (
    <ul className="app-settings-list">
      {app.mounts.map((m) => {
        const source = m.source.case === "hostPath" || m.source.case === "volume" ? m.source.value : "";
        return (
          <li key={`${source}:${m.target}`}>
            {m.source.case === "hostPath" ? <Icon name="folder" size={14} /> : <Icon name="database" size={14} />}
            <code>{source}</code>
            <Icon name="arrow-right" size={12} />
            <code>{m.target}</code>
            {m.readOnly ? <span className="app-muted">{t`Read-only`}</span> : null}
          </li>
        );
      })}
    </ul>
  );
}

interface MountRow {
  key: number;
  source: string;
  target: string;
  readOnly: boolean;
  /** What the form doesn't show, kept as it is. */
  rest: { relabel: number; external: boolean };
}

function MountsForm({ app, name, root, moving, plan, onCancel }: FormProps) {
  const { t } = useLingui();
  const { service } = useApp();
  const scope = environmentOf(service);
  const volumes = useQuery(
    ResourceService.method.listResources,
    { kind: "volume", scope },
    { enabled: scope !== "" },
  ).data?.resources.map((v) => v.resource?.name ?? "");
  const first = app.mounts.map(
    (m, i): MountRow => ({
      key: i,
      source: m.source.case === "hostPath" || m.source.case === "volume" ? m.source.value : "",
      target: m.target,
      readOnly: m.readOnly,
      rest: { relabel: m.relabel, external: m.external },
    }),
  );
  const [rows, setRows] = useState<MountRow[]>(first);
  const [next, setNext] = useState(first.length);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const unchanged = JSON.stringify(rows) === JSON.stringify(first);
  const change = (key: number, part: Partial<MountRow>) =>
    setRows((was) => was.map((r) => (r.key === key ? { ...r, ...part } : r)));
  const submit = (event: FormEvent) => {
    event.preventDefault();
    const found: Record<string, string> = {};
    for (const row of rows) {
      const source = row.source.trim();
      if (source === "")
        found[`${row.key}.source`] = t`Name a volume, such as data, or a path on the server, such as /srv/app.`;
      else if (!isServerPath(source) && !/^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/.test(source))
        found[`${row.key}.source`] = t`Use letters, digits, dots, dashes and underscores for a volume's name.`;
      if (!row.target.trim().startsWith("/"))
        found[`${row.key}.target`] = t`Use an absolute path in the app, such as /data.`;
    }
    setErrors(found);
    if (Object.keys(found).length > 0 || unchanged) return;
    // A volume it names that isn't there yet comes with it, on the servers it runs on.
    const servers = [...new Set(service.deployments.map((d) => d.serverId))];
    const made = new Set(volumes ?? []);
    const extra: Edits = [];
    for (const row of rows) {
      const source = row.source.trim();
      if (isServerPath(source) || made.has(source)) continue;
      made.add(source);
      extra.push({
        resource: { kind: "volume", scope, name: source },
        spec: { kind: { case: "volume", value: {} } },
        servers,
      });
    }
    plan(t`Change the volumes and mounts of ${name}`, [
      ...extra,
      serviceEdit(service, (a) => {
        a.mounts = rows.map((r) =>
          create(MountSchema, {
            source: isServerPath(r.source)
              ? { case: "hostPath", value: r.source.trim() }
              : { case: "volume", value: r.source.trim() },
            target: r.target.trim(),
            readOnly: r.readOnly,
            relabel: r.rest.relabel,
            external: r.rest.external,
          }),
        );
      }),
    ]);
  };
  return (
    <form className="app-settings-form" aria-label={t`Edit Volumes and mounts`} onSubmit={submit} noValidate>
      {rows.length === 0 ? (
        <p className="app-settings-empty">
          <Trans>It mounts nothing. Add a volume to keep what it writes across deploys.</Trans>
        </p>
      ) : (
        <ul className="app-settings-rows">
          {rows.map((row, i) => (
            <MountEditRow
              key={row.key}
              row={row}
              n={i + 1}
              first={i === 0}
              errors={errors}
              onChange={(part) => change(row.key, part)}
              onRemove={() => setRows((was) => was.filter((r) => r.key !== row.key))}
            />
          ))}
        </ul>
      )}
      <div>
        <Button
          type="button"
          variant="secondary"
          icon="plus"
          onClick={() => {
            setRows((was) => [
              ...was,
              { key: next, source: "", target: "", readOnly: false, rest: { relabel: 0, external: false } },
            ]);
            setNext(next + 1);
          }}
        >
          <Trans>Add a volume or mount</Trans>
        </Button>
      </div>
      <Foot name={name} root={root} moving={moving} unchanged={unchanged} onCancel={onCancel} />
    </form>
  );
}

function MountEditRow({
  row,
  n,
  first,
  errors,
  onChange,
  onRemove,
}: {
  row: MountRow;
  n: number;
  first: boolean;
  errors: Record<string, string>;
  onChange: (part: Partial<MountRow>) => void;
  onRemove: () => void;
}) {
  const { t } = useLingui();
  const id = useId();
  const source = row.source.trim();
  const reach = pathReach(row.source);
  return (
    <li className="app-settings-row is-mount">
      <TextField
        aria-label={t`Mount ${n}: volume or server path`}
        label={first ? t`Volume or server path` : undefined}
        mono
        value={row.source}
        placeholder="data"
        error={errors[`${row.key}.source`]}
        onChange={(event) => onChange({ source: event.target.value })}
        spellCheck={false}
        autoComplete="off"
      />
      <TextField
        aria-label={t`Mount ${n}: path in the app`}
        label={first ? t`Path in the app` : undefined}
        mono
        value={row.target}
        placeholder="/data"
        error={errors[`${row.key}.target`]}
        onChange={(event) => onChange({ target: event.target.value })}
        spellCheck={false}
        autoComplete="off"
      />
      <label className="app-check" htmlFor={id}>
        <input
          id={id}
          type="checkbox"
          checked={row.readOnly}
          onChange={(event) => onChange({ readOnly: event.target.checked })}
        />
        <span>{t`Read-only`}</span>
      </label>
      <Button
        type="button"
        size="sm"
        variant="ghost"
        icon="trash"
        aria-label={t`Remove mount ${n}`}
        onClick={onRemove}
      />
      {reach === "socket" ? (
        <p className="app-reach-note is-danger">
          <Icon name="shield" size={14} />
          <span>
            <Trans>
              <code>{source}</code> gives this app control of Docker on the server, the same as root. You approve this
              in the plan; API tokens can't.
            </Trans>
          </span>
        </p>
      ) : reach === "path" ? (
        <p className="app-reach-note">
          <Icon name="shield" size={14} />
          <span>
            <Trans>
              This reaches <code>{source}</code> on the server. You approve this in the plan; API tokens can't.
            </Trans>
          </span>
        </p>
      ) : null}
    </li>
  );
}
