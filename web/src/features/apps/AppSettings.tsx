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
  type ServiceInfo,
} from "@dockwell/api";
import {
  Button,
  Callout,
  cx,
  Glyph,
  Icon,
  Panel,
  PHONE_QUERY,
  PortField,
  SegmentedControl,
  SelectField,
  Switch,
  TextField,
  useMediaQuery,
  useUIStrings,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, type ReactNode, useEffect, useId, useMemo, useRef, useState } from "react";

import { useChangeSheet } from "../changes/ChangeSheet";
import { type PortHolder, useServerPorts } from "../servers/ports";
import { useApp } from "./AppContext";
import { useDeleteApp } from "./AppOverview";
import { appOf, environmentOf, isMoving, nameOf } from "./app";
import { isServerPath, pathReach, validPort } from "./form";
import {
  asGoodAsRoot,
  type CheckKind,
  checkCommand,
  checkKindOf,
  durationWords,
  joinWords,
  parseMemory,
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
  // The section being edited, with the app as it was when Edit was pressed: the edit is
  // made from that, and its revision, so a change made meanwhile makes the plan stale
  // rather than quietly undone. After this form's own apply fails, it follows the app
  // until the next plan, as the rollback gives it a revision of its own.
  const [editing, setEditing] = useState<Editing>();
  const latest = useRef(service);
  latest.current = service;
  const editButtons = useRef(new Map<SectionId, HTMLElement | null>());
  const [returnTo, setReturnTo] = useState<SectionId>();
  const app = appOf(service);
  const name = nameOf(service);
  const moving = isMoving(state);
  const active = useCurrentSection(SECTIONS);

  // The rollback of this form's own failed apply lands as a newer revision of the same
  // spec. The form keeps what was typed and plans from that, or the retry is stale.
  useEffect(() => {
    if (editing?.own && editing.from.revision !== service.revision) setEditing({ ...editing, from: service });
  }, [editing, service]);

  // Back from a form, focus is on its section's Edit, where it left from.
  useEffect(() => {
    if (!returnTo) return;
    editButtons.current.get(returnTo)?.focus();
    setReturnTo(undefined);
  }, [returnTo]);

  if (!app) {
    return (
      <Callout tone="warning" title={t`${name}'s spec can't be read`}>
        {service.specUnreadable}
      </Callout>
    );
  }

  const close = (id: SectionId) => {
    setEditing(undefined);
    setReturnTo(id);
  };
  const plan = (id: SectionId, from: ServiceInfo) => (summary: string, edits: Edits) => {
    // From here a change that lands is someone else's, as the form is planned again.
    setEditing((e) => (e?.own ? { ...e, own: false } : e));
    sheet.open({
      kind: "propose",
      summary,
      edits,
      onClose: (change: Change | undefined) => {
        if (change && (change.state === ChangeState.APPLIED || change.state === ChangeState.APPLYING)) {
          close(id);
          return;
        }
        // This form's own apply failed, and its revision and rollback moved the app: what
        // was typed stays, to fix and try again.
        if (change?.state === ChangeState.FAILED) {
          setEditing((e) => (e?.id === id ? { ...e, from: latest.current, own: true } : e));
          return;
        }
        // Someone changed it meanwhile: the form starts again from what it is now.
        const now = latest.current;
        if (now.revision !== from.revision) {
          setEditing({
            id,
            from: now,
            form: now.revision,
            note: t`${name} changed after you started, so the form shows it as it is now. Make your change again.`,
          });
        }
      },
      fix: (reason) =>
        reason === "plan_stale"
          ? t`${name} changed after you started, so this would undo that change. Close this, and the form shows it as it is now.`
          : undefined,
    });
  };

  const label: Record<SectionId | "danger", string> = {
    general: t`General`,
    health: t`Health check`,
    resources: t`Resources`,
    ports: t`Ports`,
    mounts: t`Volumes and mounts`,
    danger: t`Danger zone`,
  };
  const section = (id: SectionId, facts: (app: Container) => ReactNode, form: (props: FormProps) => ReactNode) => {
    const open = editing?.id === id ? editing : undefined;
    const from = open?.from ?? service;
    const fromApp = appOf(from) ?? app;
    return (
      <Section
        key={id}
        id={id}
        title={label[id]}
        wide={id === "mounts"}
        editing={open !== undefined}
        editRef={(el) => editButtons.current.set(id, el)}
        onEdit={() => setEditing({ id, from: service, form: service.revision })}
      >
        {open ? (
          <div key={`${open.form}`}>
            {open.note ? (
              <Callout tone="info" className="app-settings-note">
                {open.note}
              </Callout>
            ) : null}
            {form({
              service: from,
              app: fromApp,
              name,
              root: from.hostAccess.some((a) => a.sameAsRoot),
              moving,
              plan: plan(id, from),
              onCancel: () => close(id),
            })}
          </div>
        ) : (
          facts(app)
        )}
      </Section>
    );
  };
  return (
    <div className="app-settings">
      {phone ? null : (
        <nav aria-label={t`Settings sections`} className="app-settings-nav">
          {[...SECTIONS, "danger" as const].map((id) => (
            <a
              key={id}
              href={`#${id}`}
              className={cx("app-settings-nav-item", active === id && "is-active")}
              aria-current={active === id ? "true" : undefined}
            >
              {label[id]}
            </a>
          ))}
        </nav>
      )}
      <div className="app-settings-main">
        {section(
          "general",
          (a) => (
            <GeneralFacts app={a} />
          ),
          (props) => (
            <GeneralForm {...props} />
          ),
        )}
        {section(
          "health",
          (a) => (
            <HealthFacts app={a} />
          ),
          (props) => (
            <HealthForm {...props} />
          ),
        )}
        {section(
          "resources",
          (a) => (
            <ResourcesFacts app={a} />
          ),
          (props) => (
            <ResourcesForm {...props} />
          ),
        )}
        {section(
          "ports",
          (a) => (
            <PortsFacts app={a} />
          ),
          (props) => (
            <PortsForm {...props} />
          ),
        )}
        {section(
          "mounts",
          (a) => (
            <MountsFacts app={a} />
          ),
          (props) => (
            <MountsForm {...props} />
          ),
        )}
        <div id="danger" className="app-settings-anchor is-wide">
          <DangerZone title={label.danger} />
        </div>
      </div>
    </div>
  );
}

/**
 * A section's form, open. form keys what the form holds: a new one starts it
 * again from from. own marks a form whose own apply failed, until it plans again.
 */
interface Editing {
  id: SectionId;
  from: ServiceInfo;
  form: bigint;
  note?: string;
  own?: boolean;
}

const SECTIONS: readonly SectionId[] = ["general", "health", "resources", "ports", "mounts"];

/**
 * The section in view, for the nav to mark: the first whose top has passed the
 * top quarter of the screen and whose end hasn't, as a reader's eye is there.
 */
function useCurrentSection(ids: readonly SectionId[]): SectionId | "danger" | undefined {
  const [current, setCurrent] = useState<SectionId | "danger">();
  useEffect(() => {
    if (typeof IntersectionObserver === "undefined") return;
    const all = [...ids, "danger"];
    const seen = new Map<string, boolean>();
    const observer = new IntersectionObserver(
      (entries) => {
        for (const entry of entries) seen.set(entry.target.id, entry.isIntersecting);
        const first = all.find((id) => seen.get(id));
        if (first) setCurrent(first as SectionId | "danger");
      },
      { rootMargin: "-25% 0px -60% 0px" },
    );
    for (const id of all) {
      const el = document.getElementById(id);
      if (el) observer.observe(el);
    }
    return () => observer.disconnect();
  }, [ids]);
  return current;
}

function Section({
  id,
  title,
  wide,
  editing,
  editRef,
  onEdit,
  children,
}: {
  id: SectionId;
  title: string;
  /** Spans both columns, as what it lists runs wide. */
  wide?: boolean;
  editing: boolean;
  editRef: (el: HTMLButtonElement | null) => void;
  onEdit: () => void;
  children: ReactNode;
}) {
  const { t } = useLingui();
  const body = useRef<HTMLDivElement>(null);
  // Into the form at its first field, as Edit opens it.
  useEffect(() => {
    if (!editing) return;
    body.current
      ?.querySelector<HTMLElement>("input:not([disabled]), select, textarea, [role='radio'][aria-checked='true']")
      ?.focus();
  }, [editing]);
  return (
    <div
      id={id}
      ref={body}
      className={cx("app-settings-anchor", (wide || editing) && "is-wide", editing && "is-editing")}
    >
      <Panel
        title={title}
        className={cx("app-settings-section", editing && "is-editing")}
        actions={
          editing ? null : (
            <Button ref={editRef} size="sm" variant="ghost" icon="edit" aria-label={t`Edit ${title}`} onClick={onEdit}>
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

/** Deleting the app, as the mock's danger zone has it: what goes, and a red button to go on. */
function DangerZone({ title }: { title: string }) {
  const { t } = useLingui();
  const { service } = useApp();
  const remove = useDeleteApp(service);
  const name = nameOf(service);
  return (
    <Panel title={title} className="app-danger">
      <div className="app-danger-row">
        <div className="app-danger-text">
          <span className="app-danger-title">{t`Delete ${name}`}</span>
          <span className="app-danger-what">
            {remove.routes > 0 ? (
              <Trans>
                Stops it and removes its container, and its domains go with it. Its volumes stay, and you review a plan
                first.
              </Trans>
            ) : (
              <Trans>Stops it and removes its container. Its volumes stay, and you review a plan first.</Trans>
            )}
          </span>
        </div>
        <Button variant="danger" icon="trash" onClick={remove.open}>
          {t`Delete ${name}`}
        </Button>
      </div>
    </Panel>
  );
}

interface FormProps {
  /** The app as it was when Edit was pressed, which the edit is made from. */
  service: ServiceInfo;
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
      <Fact term={t`Image`}>
        <code className="app-settings-image">{app.image}</code>
      </Fact>
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

function GeneralForm({ service, app, name, root, moving, plan, onCancel }: FormProps) {
  const { t } = useLingui();
  const words = useAppWords();
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
          <Fact term={t`Every`}>
            {check.interval ? durationWords(check.interval) : <Muted>{t`Docker's default`}</Muted>}
          </Fact>
          <Fact term={t`Timeout`}>
            {check.timeout ? durationWords(check.timeout) : <Muted>{t`Docker's default`}</Muted>}
          </Fact>
          <Fact term={t`Unhealthy after`}>
            {check.retries > 0 ? (
              <Trans>{retries} failed checks in a row</Trans>
            ) : (
              <Trans>3 failed checks in a row</Trans>
            )}
          </Fact>
          {check.startPeriod ? <Fact term={t`Grace at start`}>{durationWords(check.startPeriod)}</Fact> : null}
        </>
      ) : null}
    </Facts>
  );
}

function HealthForm({ service, app, name, root, moving, plan, onCancel }: FormProps) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
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
          case "command": {
            // An exec-form check keeps its words while they're as they were: a shell's form
            // needs /bin/sh in the image, which one with no shell hasn't got.
            const kept = check?.test[0] === "CMD" && command === checkCommand(check);
            a.healthcheck = create(HealthcheckSchema, {
              ...timing,
              test: kept ? [...(check?.test ?? [])] : ["CMD-SHELL", command.trim()],
            });
            break;
          }
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

function ResourcesForm({ service, app, name, root, moving, plan, onCancel }: FormProps) {
  const { t } = useLingui();
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
      reserved:
        reserved.trim() !== "" && !validMemory(reserved)
          ? mem
          : reserved.trim() !== "" && memory.trim() !== "" && (parseMemory(reserved) ?? 0) > (parseMemory(memory) ?? 0)
            ? t`Keep no more for it than its limit, ${memory}.`
            : undefined,
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
        <Trans>None. Only what shares a network with it can reach it.</Trans>
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
  /** A port added here follows a free one on the server, until someone types their own. */
  auto?: boolean;
}

function PortsForm({ service, app, name, root, moving, plan, onCancel }: FormProps) {
  const { t } = useLingui();
  const first = app.ports.map(
    (p, i): PortRow => ({
      key: i,
      host: String(p.hostPort),
      inApp: String(p.containerPort),
      udp: p.protocol === Protocol.UDP,
      hostIp: p.hostIp,
    }),
  );
  const [typed, setRows] = useState<PortRow[]>(first);
  const [next, setNext] = useState(first.length);
  const [errors, setErrors] = useState<Record<string, string>>({});
  // What holds each port on its servers, besides this app, whose ports are its own to keep.
  const serverIds = useMemo(() => service.deployments.map((d) => d.serverId), [service.deployments]);
  const ports = useServerPorts(serverIds, service.id);
  const holderName = (holder: PortHolder) => (holder.kind === "proxy" ? t`Dockwell's proxy` : holder.name);
  const strings = useUIStrings();
  const id = useId();
  // A port added here takes a free one, in order, so no two rows take one.
  const rows = useMemo(() => {
    if (!ports.ready) return typed;
    const taken = typed.filter((r) => !r.auto && validPort(r.host)).map((r) => Number(r.host));
    return typed.map((r) => {
      if (!r.auto) return r;
      const free = ports.free(validPort(r.inApp) ? Number(r.inApp) : undefined, r.udp, taken);
      taken.push(free);
      return { ...r, host: String(free) };
    });
  }, [typed, ports]);
  const unchanged = JSON.stringify(rows.map(({ auto: _, ...r }) => r)) === JSON.stringify(first);
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
            const others = rows
              .filter((r) => r.key !== row.key && r.udp === row.udp && validPort(r.host))
              .map((r) => Number(r.host));
            const check = ports.check(row.host, row.udp, holderName, others);
            const offer =
              check.state === "taken"
                ? ports.free(validPort(row.inApp) ? Number(row.inApp) : undefined, row.udp, [
                    ...others,
                    Number(row.host),
                  ])
                : undefined;
            // What's wrong with its server port, under the row, across it, so the row's fields stay level.
            const said =
              errors[`${row.key}.host`] ??
              (check.state === "taken" ? (
                <>
                  {check.by ? strings.portField.takenBy(check.by) : strings.portField.taken}
                  {offer !== undefined ? (
                    <>
                      {" · "}
                      <button
                        type="button"
                        className="dw-port-use"
                        onClick={() => change(row.key, { host: String(offer), auto: false })}
                      >
                        {strings.portField.use(offer)}
                      </button>
                    </>
                  ) : null}
                </>
              ) : undefined);
            return (
              <li key={row.key} className="app-settings-row is-port">
                <TextField
                  aria-label={t`Port ${n} in the app`}
                  label={i === 0 ? t`In the app` : undefined}
                  mono
                  inputMode="numeric"
                  value={row.inApp}
                  error={errors[`${row.key}.app`]}
                  onChange={(event) => change(row.key, { inApp: event.target.value })}
                />
                <PortField
                  aria-label={t`Port ${n} on the server`}
                  label={i === 0 ? t`On the server` : undefined}
                  // An address it's bound to stays as it is, and shows, as the port is its.
                  prefix={row.hostIp ? `${row.hostIp}:` : undefined}
                  inRow
                  value={row.host}
                  check={check}
                  invalid={Boolean(errors[`${row.key}.host`])}
                  aria-describedby={said ? `${id}-${row.key}` : undefined}
                  onChange={(host) => change(row.key, { host, auto: false })}
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
                {said ? (
                  <p id={`${id}-${row.key}`} className="dw-field-error app-settings-row-note" role="alert">
                    <Glyph shape="square" size={9} />
                    <span>{said}</span>
                  </p>
                ) : null}
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
            setRows((was) => [...was, { key: next, host: "", inApp: "", udp: false, hostIp: "", auto: true }]);
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

function MountsForm({ service, app, name, root, moving, plan, onCancel }: FormProps) {
  const { t } = useLingui();
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
  // A row that's as good as root asks for the passkey, whatever the app reaches now.
  const reachesRoot = rows.some((r) => isServerPath(r.source) && asGoodAsRoot(r.source, r.readOnly));
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
      // An external volume is Docker's own, as one taken on from another tool: Dockwell never makes it.
      if (isServerPath(source) || made.has(source) || row.rest.external) continue;
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
      <Foot name={name} root={root || reachesRoot} moving={moving} unchanged={unchanged} onCancel={onCancel} />
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
              <code>{source}</code> gives this app control of Docker on the server, the same as root, so applying this
              asks for your passkey. API tokens can't.
            </Trans>
          </span>
        </p>
      ) : reach === "path" && asGoodAsRoot(row.source, row.readOnly) ? (
        <p className="app-reach-note is-danger">
          <Icon name="shield" size={14} />
          <span>
            <Trans>
              <code>{source}</code> on the server is as good as root there, so applying this asks for your passkey. API
              tokens can't.
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
