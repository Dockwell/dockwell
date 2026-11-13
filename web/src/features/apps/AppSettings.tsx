// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { useQuery } from "@connectrpc/connect-query";
import {
  AutoUpdate_Level,
  AutoUpdateSchema,
  type Change,
  ChangeService,
  ChangeState,
  type Container,
  type EditSchema,
  HealthcheckSchema,
  type ImageConfig,
  MountSchema,
  type MountSeed,
  PortMappingSchema,
  Protocol,
  ResourceService,
  ResourcesSchema,
  RestartPolicy,
  type ServiceInfo,
} from "@dockwell/api";
import {
  Advanced,
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
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { type FormEvent, type ReactNode, useEffect, useId, useMemo, useRef, useState } from "react";

import { formatBytes } from "../../lib/format";
import { MayEdit } from "../auth/MayEdit";
import { useMay } from "../auth/may";
import { useChangeSheet } from "../changes/ChangeSheet";
import { type PortHolder, useServerPorts } from "../servers/ports";
import { useFleet } from "../servers/useFleet";
import { useApp } from "./AppContext";
import { appOf, environmentOf, isMoving, nameOf } from "./app";
import { DangerZone } from "./DangerZone";
import { isServerPath, pathReach, validPort } from "./form";
import { useImageConfig } from "./imageConfig";
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

type SectionId = "general" | "health" | "resources" | "ports" | "mounts" | "updates";

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
  // An app that runs an image of its own takes updates; one that builds or runs from a folder moves on otherwise.
  const updatable = service.spec?.build === undefined && service.spec?.runtime === undefined;
  const sections = useMemo(() => (updatable ? [...SECTIONS, "updates" as const] : SECTIONS), [updatable]);
  const active = useCurrentSection(sections);
  const mayPlan = useMay()(ChangeService.method.proposeChange, { environment: environmentOf(service) });
  // What the image declares, so a setting the app leaves to its image says what that is.
  const image = useImageConfig(service, app?.image ?? "");

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
    updates: t`Automatic updates`,
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
              image,
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
          {[...sections, "danger" as const].map((id) => (
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
        {/* For someone who may not plan here, its settings are there to read, with why (ADR 0059). */}
        <MayEdit offer={mayPlan}>
          {section(
            "general",
            (a) => (
              <GeneralFacts app={a} image={image} />
            ),
            (props) => (
              <GeneralForm {...props} />
            ),
          )}
          {section(
            "health",
            (a) => (
              <HealthFacts app={a} image={image} />
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
          {updatable
            ? section(
                "updates",
                () => <UpdatesFacts service={service} />,
                (props) => <UpdatesForm {...props} />,
              )
            : null}
          <div id="danger" className="app-settings-anchor is-wide">
            <DangerZone service={service} title={label.danger} />
          </div>
        </MayEdit>
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

interface FormProps {
  /** The app as it was when Edit was pressed, which the edit is made from. */
  service: ServiceInfo;
  app: Container;
  /** What its image declares, once read. */
  image: ImageConfig | undefined;
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

/** What the image sets where the app sets nothing: its value, once read, beside who set it. */
function ImagesOwn({ value }: { value: string | undefined }) {
  const { t } = useLingui();
  if (!value) return <Muted>{t`The image's own`}</Muted>;
  return (
    <span>
      <code>{value}</code> <Muted>{t({ message: "the image's", context: "a setting the image makes" })}</Muted>
    </span>
  );
}

function GeneralFacts({ app, image }: { app: Container; image: ImageConfig | undefined }) {
  const { t } = useLingui();
  const words = useAppWords();
  return (
    <Facts>
      <Fact term={t`Image`}>
        <code className="app-settings-image">{app.image}</code>
      </Fact>
      <Fact term={t`Command`}>
        {app.command.length > 0 ? (
          <code>{joinWords(app.command)}</code>
        ) : (
          <ImagesOwn value={image && joinWords(image.command)} />
        )}
      </Fact>
      <Fact term={t`Entrypoint`}>
        {app.noEntrypoint ? (
          <Muted>{t`None, not even the image's`}</Muted>
        ) : app.entrypoint.length > 0 ? (
          <code>{joinWords(app.entrypoint)}</code>
        ) : (
          <ImagesOwn value={image && joinWords(image.entrypoint)} />
        )}
      </Fact>
      <Fact term={t`User`}>
        {app.user ? <code>{app.user}</code> : <ImagesOwn value={image && (image.user || "root")} />}
      </Fact>
      <Fact term={t`Directory`}>
        {app.workingDir ? <code>{app.workingDir}</code> : <ImagesOwn value={image && (image.workingDir || "/")} />}
      </Fact>
      <Fact term={t`Restarts`}>{words.restart(app.restart)}</Fact>
    </Facts>
  );
}

const RESTARTS = [RestartPolicy.UNLESS_STOPPED, RestartPolicy.ALWAYS, RestartPolicy.ON_FAILURE, RestartPolicy.NO];

function GeneralForm({ service, app, image, name, root, moving, plan, onCancel }: FormProps) {
  const { t } = useLingui();
  const words = useAppWords();
  const runtime = service.spec?.runtime !== undefined;
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
      {runtime ? (
        <p className="app-settings-help">
          <Trans>Its runtime sets the command, entrypoint, user and directory.</Trans>
        </p>
      ) : null}
      <div className="app-settings-grid">
        <TextField
          label={t`Command`}
          mono
          value={command}
          disabled={runtime}
          placeholder={(image && joinWords(image.command)) || t`The image's own`}
          hint={runtime ? undefined : t`Empty runs the image's own.`}
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
            disabled={runtime || noEntrypoint}
            placeholder={noEntrypoint ? t`None` : (image && joinWords(image.entrypoint)) || t`The image's own`}
            error={errors.entrypoint}
            onChange={(event) => setEntrypoint(event.target.value)}
            spellCheck={false}
            autoComplete="off"
          />
          <Switch
            label={t`Run no entrypoint, not even the image's`}
            checked={noEntrypoint}
            disabled={runtime}
            onChange={setNoEntrypoint}
          />
        </div>
        <TextField
          label={t`User`}
          mono
          value={user}
          disabled={runtime}
          placeholder={image ? image.user || "root" : t`The image's own`}
          hint={t`A name or a number, such as 1000.`}
          onChange={(event) => setUser(event.target.value)}
          spellCheck={false}
          autoComplete="off"
        />
        <TextField
          label={t`Directory`}
          mono
          value={dir}
          disabled={runtime}
          placeholder={image ? image.workingDir || "/" : t`The image's own`}
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

function HealthFacts({ app, image }: { app: Container; image: ImageConfig | undefined }) {
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
          <Muted>{t`Turned off, so Running only means it started`}</Muted>
        ) : (
          <ImagesCheck image={image} />
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

/** The image's own check, as its HEALTHCHECK says, once read. */
function ImagesCheck({ image }: { image: ImageConfig | undefined }) {
  const { t } = useLingui();
  if (!image) return <Muted>{t`The image's own, if it has one`}</Muted>;
  const test = imageCheckWords(image.healthcheck);
  if (!test) return <Muted>{t`The image has none, so Running only means it started`}</Muted>;
  return (
    <span>
      <code>{test}</code> <Muted>{t({ message: "the image's", context: "a setting the image makes" })}</Muted>
    </span>
  );
}

/** An image's HEALTHCHECK test as it's written, such as curl -f http://localhost/, or "" for none. */
function imageCheckWords(test: readonly string[]): string {
  const [form, ...rest] = test;
  switch (form) {
    case "CMD-SHELL":
      return rest.join(" ");
    case "CMD":
      return joinWords(rest);
  }
  return "";
}

/** Paths an app's health is often read at, offered as it's typed. */
const HEALTH_PATHS = ["/", "/health", "/healthz", "/api/health", "/status", "/ping"];

function HealthForm({ service, app, image, name, root, moving, plan, onCancel }: FormProps) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const runtime = service.spec?.runtime !== undefined;
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
  const timed = kind === "http" || (kind === "tcp" && !runtime) || kind === "command";
  // What runs it when nothing's typed: the agent's for HTTP and TCP, Docker's for a command.
  const every = kind === "command" ? "30s" : "10s";
  const limit = kind === "command" ? "30s" : "5s";
  const everyNow = interval.trim() || every;
  const limitNow = timeout.trim() || limit;
  const failures = retries.trim() || "3";
  // The ports it listens on, its own and its image's, offered for the check.
  const listens = [
    ...new Set(
      [...app.ports.map((p) => p.containerPort), ...(image?.ports ?? []).map((p) => p.port)].filter((p) => p > 0),
    ),
  ];
  const portsId = useId();
  const pathsId = useId();
  const pick = (next: CheckKind) => {
    setKind(next);
    // A check of a port starts at the one it listens on.
    if ((next === "http" || next === "tcp") && port === "" && listens[0]) setPort(String(listens[0]));
  };
  const checkKinds: { value: CheckKind; label: string }[] = [
    { value: "http", label: t`HTTP` },
    { value: "tcp", label: t`TCP` },
    { value: "command", label: t({ message: "Command", context: "health check" }) },
    { value: "image", label: t`The image's` },
    { value: "none", label: t({ message: "None", context: "health check" }) },
  ];
  const availableCheckKinds = runtime ? checkKinds.filter((choice) => choice.value !== "image") : checkKinds;
  const unchanged =
    runtime && kind === "tcp"
      ? checkKindOf(check) === "tcp"
      : kind === checkKindOf(check) &&
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
      port:
        (kind === "http" || (kind === "tcp" && !runtime)) && !validPort(port)
          ? t`Use a port from 1 to 65535.`
          : undefined,
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
            // The runtime writes its own TCP check, including its start grace.
            a.healthcheck = runtime ? undefined : create(HealthcheckSchema, { ...timing, tcp: { port: Number(port) } });
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
          onChange={(event) => pick(event.target.value as CheckKind)}
          options={availableCheckKinds}
        />
      ) : (
        <SegmentedControl<CheckKind>
          label={t`How it's checked`}
          value={kind}
          onChange={pick}
          options={availableCheckKinds}
        />
      )}
      <p className="app-settings-help">
        {kind === "http" ? (
          t`Any status from 200 to 399 passes.`
        ) : kind === "tcp" ? (
          runtime ? (
            t`Its runtime checks its port by TCP, with 10 minutes to start.`
          ) : (
            t`It passes once the app takes a connection.`
          )
        ) : kind === "command" ? (
          t`It passes when it exits with 0.`
        ) : kind === "image" ? (
          <ImagesCheck image={image} />
        ) : (
          t`Running only means it started.`
        )}
      </p>
      {kind === "http" || (kind === "tcp" && !runtime) || kind === "command" ? (
        <div className="app-settings-grid">
          {kind === "http" || kind === "tcp" ? (
            <TextField
              label={t`Port in the app`}
              mono
              inputMode="numeric"
              value={port}
              error={errors.port}
              list={listens.length > 0 ? portsId : undefined}
              onChange={(event) => setPort(event.target.value)}
            />
          ) : null}
          {kind === "http" ? (
            <TextField
              label={t`Path`}
              mono
              value={path}
              placeholder="/"
              list={pathsId}
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
        </div>
      ) : null}
      {timed ? (
        <Advanced
          label={t`Timing`}
          summary={t`Every ${everyNow}, ${limitNow} to answer, unhealthy after ${failures} failures`}
          defaultOpen={Object.values(errors).some(Boolean) && !errors.port && !errors.command}
        >
          <div className="app-settings-grid">
            <TextField
              label={t`Every`}
              mono
              value={interval}
              placeholder={every}
              error={errors.interval}
              onChange={(event) => setEvery(event.target.value)}
            />
            <TextField
              label={t`Timeout`}
              mono
              value={timeout}
              placeholder={limit}
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
        </Advanced>
      ) : null}
      <datalist id={portsId}>
        {listens.map((p) => (
          <option key={p} value={p} />
        ))}
      </datalist>
      <datalist id={pathsId}>
        {HEALTH_PATHS.map((p) => (
          <option key={p} value={p} />
        ))}
      </datalist>
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

/** What a limit is picked from, up to what the server has, and anything else typed. */
const MiB = 1024 * 1024;
const MEMORY_PICKS = [
  { value: "256M", bytes: 256 * MiB },
  { value: "512M", bytes: 512 * MiB },
  { value: "1G", bytes: 1024 * MiB },
  { value: "2G", bytes: 2048 * MiB },
  { value: "4G", bytes: 4096 * MiB },
  { value: "8G", bytes: 8192 * MiB },
];
const CPU_PICKS = ["0.5", "1", "2", "4", "8"];
const OTHER = "other";

function ResourcesForm({ service, app, name, root, moving, plan, onCancel }: FormProps) {
  const { t, i18n } = useLingui();
  // What its server has, so a limit is picked from what fits.
  const fleet = useFleet();
  const server =
    fleet.status === "connecting" ? undefined : fleet.servers.find((s) => s.id === service.deployments[0]?.serverId);
  const memoryTotal = Number(server?.inventory?.memoryBytes ?? 0n);
  const cpuCount = server?.inventory?.cpuCount ?? 0;
  const memoryPicks = MEMORY_PICKS.filter((m) => memoryTotal === 0 || m.bytes < memoryTotal).map((m) => m.value);
  const cpuPicks = CPU_PICKS.filter((c) => cpuCount === 0 || Number(c) <= cpuCount);
  const totalWords = formatBytes(memoryTotal, i18n.locale);
  const serverName = server?.name ?? "";
  const r = app.resources;
  const [cpus, setCpus] = useState(r?.cpus ?? "");
  const [memory, setMemory] = useState(r?.memory ?? "");
  const [reserved, setReserved] = useState(r?.memoryReservation ?? "");
  const [errors, setErrors] = useState<Partial<Record<"cpus" | "memory" | "reserved", string>>>({});
  // A value that isn't one of the picks is typed, under Other.
  const [memoryOther, setMemoryOther] = useState(memory !== "" && !memoryPicks.includes(memory));
  const [cpusOther, setCpusOther] = useState(cpus !== "" && !cpuPicks.includes(cpus));
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
  const choose = (value: string, set: (v: string) => void, other: (o: boolean) => void) => {
    other(value === OTHER);
    if (value !== OTHER) set(value);
  };
  const noLimit = t`No limit`;
  return (
    <form className="app-settings-form" aria-label={t`Edit Resources`} onSubmit={submit} noValidate>
      <div className="app-settings-grid">
        <div className="app-settings-field">
          <SelectField
            label={t`CPU limit`}
            value={cpusOther ? OTHER : cpus}
            hint={server && cpuCount > 0 ? t`${serverName} has ${cpuCount} vCPU.` : undefined}
            onChange={(event) => choose(event.target.value, setCpus, setCpusOther)}
            options={[
              { value: "", label: noLimit },
              ...cpuPicks.map((c) => ({ value: c, label: t`${c} vCPU` })),
              { value: OTHER, label: t({ message: "Other", context: "a limit typed in" }) },
            ]}
          />
          {cpusOther ? (
            <TextField
              aria-label={t`CPU limit, typed`}
              mono
              inputMode="decimal"
              value={cpus}
              placeholder="1.5"
              suffix="vCPU"
              error={errors.cpus}
              onChange={(event) => setCpus(event.target.value)}
            />
          ) : null}
        </div>
        <div className="app-settings-field">
          <SelectField
            label={t`Memory limit`}
            value={memoryOther ? OTHER : memory}
            hint={server && memoryTotal > 0 ? t`${serverName} has ${totalWords}.` : undefined}
            onChange={(event) => choose(event.target.value, setMemory, setMemoryOther)}
            options={[
              { value: "", label: noLimit },
              ...memoryPicks.map((m) => ({ value: m, label: m })),
              { value: OTHER, label: t({ message: "Other", context: "a limit typed in" }) },
            ]}
          />
          {memoryOther ? (
            <TextField
              aria-label={t`Memory limit, typed`}
              mono
              value={memory}
              placeholder="768M"
              error={errors.memory}
              onChange={(event) => setMemory(event.target.value)}
            />
          ) : null}
        </div>
      </div>
      <Advanced
        label={t`Memory kept for it`}
        summary={reserved.trim() ? reserved.trim() : t`Nothing set aside`}
        defaultOpen={errors.reserved !== undefined || reserved !== ""}
      >
        <TextField
          label={t`Memory kept for it`}
          mono
          value={reserved}
          placeholder={t`None`}
          hint={t`What Docker leaves it when the server runs short.`}
          error={errors.reserved}
          onChange={(event) => setReserved(event.target.value)}
        />
      </Advanced>
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
  /** What a following port showed when someone went into its field: it holds still there while it has focus. */
  held?: string;
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
  // The server is asked about each row's ports as they're typed, over UDP where a row is.
  const watch = useMemo(() => typed.flatMap((r) => [Number(r.inApp), Number(r.host)]).filter((n) => n > 0), [typed]);
  const watchUdp = useMemo(() => typed.filter((r) => r.udp).map((r) => Number(r.host)), [typed]);
  const ports = useServerPorts(serverIds, service.id, watch, watchUdp);
  const holderName = (holder: PortHolder) => (holder.kind === "proxy" ? t`Dockwell's proxy` : holder.name);
  const strings = useUIStrings();
  const id = useId();
  // A port added here takes a free one, in order, so no two rows take one.
  // A row someone is in holds what it showed, as a free port landing under the cursor would
  // run into what they type, and counts as taken.
  const rows = useMemo(() => {
    const fixed = typed.map((r) => (r.auto && r.held !== undefined ? { ...r, host: r.held } : r));
    if (!ports.ready) return fixed;
    const taken = fixed
      .filter((r) => (!r.auto || r.held !== undefined) && validPort(r.host))
      .map((r) => Number(r.host));
    return fixed.map((r) => {
      if (!r.auto || r.held !== undefined) return r;
      const free = ports.free(validPort(r.inApp) ? Number(r.inApp) : undefined, r.udp, taken);
      taken.push(free);
      return { ...r, host: String(free) };
    });
  }, [typed, ports]);
  const unchanged = JSON.stringify(rows.map(({ auto: _, held: __, ...r }) => r)) === JSON.stringify(first);
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
                  onFocus={() => row.auto && change(row.key, { held: row.host })}
                  onBlur={() => row.auto && change(row.key, { held: undefined })}
                  onChange={(host) => change(row.key, { host, auto: false, held: undefined })}
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
        if (m.source.case === "file") {
          const lines = m.source.value.content.split("\n").filter((l, i, all) => l !== "" || i < all.length - 1).length;
          return (
            <li key={`file:${m.target}`} className="app-settings-file">
              <details>
                <summary>
                  <Icon name="file" size={14} />
                  <span>
                    {plural(lines, { one: "A file Dockwell writes, # line", other: "A file Dockwell writes, # lines" })}
                  </span>
                  <Icon name="arrow-right" size={12} />
                  <code>{m.target}</code>
                  <span className="app-muted">{t`Read-only`}</span>
                </summary>
                <pre className="app-settings-file-content">{m.source.value.content}</pre>
              </details>
            </li>
          );
        }
        const source = m.source.case === "hostPath" || m.source.case === "volume" ? m.source.value : "";
        return (
          <li key={`${source}:${m.target}`}>
            {m.source.case === "hostPath" ? <Icon name="folder" size={14} /> : <Icon name="database" size={14} />}
            <code>{source}</code>
            <Icon name="arrow-right" size={12} />
            <code>{m.target}</code>
            {m.readOnly ? <span className="app-muted">{t`Read-only`}</span> : null}
            {m.makeOwner ? <span className="app-muted">{t`Owned by ${m.makeOwner}`}</span> : null}
            {m.makeMode ? <span className="app-muted">{t`Mode ${modeText(m.makeMode)}`}</span> : null}
            {m.seed.length > 0 ? (
              <span className="app-muted">
                {plural(m.seed.length, { one: "Starts with # file", other: "Starts with # files" })}
              </span>
            ) : null}
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
  rest: { relabel: number; external: boolean; makeOwner: string; makeMode: number; seed: MountSeed[] };
}

function MountsForm({ service, app, image, name, root, moving, plan, onCancel }: FormProps) {
  const { t } = useLingui();
  const volumesId = useId();
  const targetsId = useId();
  const scope = environmentOf(service);
  const volumes = useQuery(
    ResourceService.method.listResources,
    { kind: "volume", scope },
    { enabled: scope !== "" },
  ).data?.resources.map((v) => v.resource?.name ?? "");
  // A file Dockwell writes isn't this form's to change: it stays as it is, and the
  // rows are the rest.
  const files = app.mounts.filter((m) => m.source.case === "file");
  const first = app.mounts
    .filter((m) => m.source.case !== "file")
    .map(
      (m, i): MountRow => ({
        key: i,
        source: m.source.case === "hostPath" || m.source.case === "volume" ? m.source.value : "",
        target: m.target,
        readOnly: m.readOnly,
        rest: { relabel: m.relabel, external: m.external, makeOwner: m.makeOwner, makeMode: m.makeMode, seed: m.seed },
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
        a.mounts = [
          ...files,
          ...rows.map((r) =>
            create(MountSchema, {
              source: isServerPath(r.source)
                ? { case: "hostPath", value: r.source.trim() }
                : { case: "volume", value: r.source.trim() },
              target: r.target.trim(),
              readOnly: r.readOnly,
              relabel: r.rest.relabel,
              // Whom a volume is made for, its mode and what it starts with go with the
              // volume they were given for: a row that now names another source makes
              // nothing of them.
              ...(r.source.trim() === first.find((f) => f.key === r.key)?.source
                ? { makeOwner: r.rest.makeOwner, makeMode: r.rest.makeMode, seed: r.rest.seed }
                : {}),
              external: r.rest.external,
            }),
          ),
        ];
      }),
    ]);
  };
  return (
    <form className="app-settings-form" aria-label={t`Edit Volumes and mounts`} onSubmit={submit} noValidate>
      {files.length > 0 ? (
        <ul className="app-settings-list">
          {files.map((m) => (
            <li key={`file:${m.target}`}>
              <Icon name="file" size={14} />
              <span>{t`A file Dockwell writes`}</span>
              <Icon name="arrow-right" size={12} />
              <code>{m.target}</code>
              <span className="app-muted">{t`Stays as it is`}</span>
            </li>
          ))}
        </ul>
      ) : null}
      {rows.length === 0 && files.length === 0 ? (
        <p className="app-settings-empty">
          <Trans>It mounts nothing. Add a volume to keep what it writes across deploys.</Trans>
        </p>
      ) : rows.length === 0 ? null : (
        <ul className="app-settings-rows">
          {rows.map((row, i) => (
            <MountEditRow
              key={row.key}
              row={row}
              n={i + 1}
              first={i === 0}
              fixed={service.spec?.runtime !== undefined && row.target === "/app"}
              volumesList={(volumes ?? []).length > 0 ? volumesId : undefined}
              targetsList={(image?.volumes ?? []).length > 0 ? targetsId : undefined}
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
              {
                key: next,
                source: "",
                target: "",
                readOnly: false,
                rest: { relabel: 0, external: false, makeOwner: "", makeMode: 0, seed: [] },
              },
            ]);
            setNext(next + 1);
          }}
        >
          <Trans>Add a volume or mount</Trans>
        </Button>
      </div>
      {/* The environment's volumes for a source, and where the image keeps its data for a target. */}
      <datalist id={volumesId}>
        {(volumes ?? []).map((v) => (
          <option key={v} value={v} />
        ))}
      </datalist>
      <datalist id={targetsId}>
        {(image?.volumes ?? []).map((v) => (
          <option key={v} value={v} />
        ))}
      </datalist>
      <Foot name={name} root={root || reachesRoot} moving={moving} unchanged={unchanged} onCancel={onCancel} />
    </form>
  );
}

function MountEditRow({
  row,
  n,
  first,
  fixed,
  volumesList,
  targetsList,
  errors,
  onChange,
  onRemove,
}: {
  row: MountRow;
  n: number;
  first: boolean;
  fixed: boolean;
  /** The datalists of volumes to offer as a source, and of the image's paths as a target. */
  volumesList: string | undefined;
  targetsList: string | undefined;
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
        disabled={fixed}
        placeholder="data"
        // A server path is typed until the websites area's folder picker lands (#429).
        list={volumesList}
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
        disabled={fixed}
        placeholder="/data"
        list={targetsList}
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
          disabled={fixed}
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
        disabled={fixed}
        onClick={onRemove}
      />
      {fixed ? (
        <p className="app-reach-note">
          <Trans>Its runtime mounts this folder at /app.</Trans>
        </p>
      ) : reach === "socket" ? (
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

// Automatic updates: which updates Dockwell takes for it on its own, and when (ADR 0080).

type UpdateDays = "every" | "weekdays" | "weekends" | "day";

const WEEKDAYS = [1, 2, 3, 4, 5];
const WEEKENDS = [0, 6];
const WINDOW_PICKS = [60, 120, 240, 480];

function daysOf(weekdays: readonly number[]): { days: UpdateDays; day: number } {
  const same = (a: readonly number[]) => a.length === weekdays.length && a.every((d) => weekdays.includes(d));
  if (weekdays.length === 0) return { days: "every", day: 0 };
  if (same(WEEKDAYS)) return { days: "weekdays", day: 1 };
  if (same(WEEKENDS)) return { days: "weekends", day: 0 };
  return { days: "day", day: weekdays[0] ?? 0 };
}

/** What a level takes, in a few words. */
function useLevelWords(): (level: AutoUpdate_Level) => string {
  const { t } = useLingui();
  return (level) =>
    ({
      [AutoUpdate_Level.UNSPECIFIED]: t`Off`,
      [AutoUpdate_Level.PATCH]: t`Patch updates`,
      [AutoUpdate_Level.MINOR]: t`Minor and patch updates`,
      [AutoUpdate_Level.ANY]: t`Any update`,
    })[level];
}

/**
 * The tag an app runs where it names less than one release whole, as latest, 16 or 1.27 do, whose new
 * image may be another release: the server classes its move by the versions its images say (ADR 0080).
 */
function movingTagOf(image: string): string | undefined {
  const bare = image.split("@")[0] ?? image;
  const colon = bare.lastIndexOf(":");
  const tag = colon > bare.lastIndexOf("/") ? bare.slice(colon + 1) : "latest";
  const parts = (tag.split("-")[0] ?? "").replace(/^v/, "").split(".");
  return parts.length >= 3 && parts.every((p) => /^\d+$/.test(p)) ? undefined : tag;
}

/** What patch and minor do with a moving tag's new images, in a line, where the app runs one. */
function useMovingTagWords(): (name: string, image: string, level: AutoUpdate_Level) => string | undefined {
  const { t } = useLingui();
  return (name, image, level) => {
    const tag = movingTagOf(image);
    if (!tag || (level !== AutoUpdate_Level.PATCH && level !== AutoUpdate_Level.MINOR)) return undefined;
    return t`${name} runs ${tag}: patch and minor take its new images only when they say their version.`;
  };
}

function UpdatesFacts({ service }: { service: ServiceInfo }) {
  const { t, i18n } = useLingui();
  const level = useLevelWords();
  const movingWords = useMovingTagWords();
  const weekdays = useWeekdayNames();
  const a = service.spec?.autoUpdate;
  if (!a || a.level === AutoUpdate_Level.UNSPECIFIED) {
    return (
      <Facts>
        <Fact term={t`Takes`}>
          <Muted>{t`Nothing on its own: each update waits for you`}</Muted>
        </Fact>
      </Facts>
    );
  }
  const { days, day } = daysOf(a.weekdays);
  const on = {
    every: t`Every day`,
    weekdays: t`Monday to Friday`,
    weekends: t`Saturday and Sunday`,
    day:
      new Intl.ListFormat(i18n.locale, { type: "conjunction" }).format(a.weekdays.map((d) => weekdays[d] ?? "")) ||
      (weekdays[day] ?? ""),
  }[days];
  const hours = a.minutes / 60;
  const length = Number.isInteger(hours) ? t`${hours} h` : t`${a.minutes} min`;
  const zone = a.timeZone;
  const app = service.spec?.runs.case === "app" ? service.spec.runs.value : undefined;
  const moves = movingWords(service.resource?.name ?? "", app?.image ?? "", a.level);
  return (
    <Facts>
      <Fact term={t`Takes`}>
        {level(a.level)}
        {moves ? (
          <>
            <br />
            <Muted>{moves}</Muted>
          </>
        ) : null}
      </Fact>
      <Fact term={t`When`}>{t`${on} from ${a.at} for ${length}, ${zone}`}</Fact>
      <Fact term={t`Backup first`}>{a.skipBackup ? <Muted>{t`No`}</Muted> : t`Yes`}</Fact>
    </Facts>
  );
}

/** The days of the week, Sunday first, as the locale names them. */
function useWeekdayNames(): string[] {
  const { i18n } = useLingui();
  const format = new Intl.DateTimeFormat(i18n.locale, { weekday: "long", timeZone: "UTC" });
  // 4 Jan 1970 was a Sunday.
  return Array.from({ length: 7 }, (_, d) => format.format(new Date(Date.UTC(1970, 0, 4 + d))));
}

function UpdatesForm({ service, app, name, root, moving, plan, onCancel }: FormProps) {
  const { t } = useLingui();
  const level = useLevelWords();
  const movingWords = useMovingTagWords();
  const weekdays = useWeekdayNames();
  const a = service.spec?.autoUpdate;
  const was = daysOf(a?.weekdays ?? []);
  const [takes, setTakes] = useState<AutoUpdate_Level>(a?.level ?? AutoUpdate_Level.UNSPECIFIED);
  const [days, setDays] = useState<UpdateDays>(was.days);
  const [day, setDay] = useState(String(was.day));
  const [at, setAt] = useState(a?.at || "03:00");
  const [minutes, setMinutes] = useState(String(a?.minutes || 120));
  const [backup, setBackup] = useState(!a?.skipBackup);
  const [error, setError] = useState<string>();
  const weekdaysOf = (): number[] => ({ every: [], weekdays: WEEKDAYS, weekends: WEEKENDS, day: [Number(day)] })[days];
  const off = takes === AutoUpdate_Level.UNSPECIFIED;
  const next = off
    ? undefined
    : create(AutoUpdateSchema, {
        level: takes,
        weekdays: weekdaysOf(),
        at,
        minutes: Number(minutes),
        timeZone: a?.timeZone ?? "",
        skipBackup: !backup,
      });
  const unchanged =
    (off && (a === undefined || a.level === AutoUpdate_Level.UNSPECIFIED)) ||
    (!off &&
      a !== undefined &&
      a.level === takes &&
      a.at === at &&
      a.minutes === Number(minutes) &&
      a.skipBackup === !backup &&
      daysOf(a.weekdays).days === days &&
      (days !== "day" || String(daysOf(a.weekdays).day) === day));
  const minutePicks = WINDOW_PICKS.includes(Number(minutes)) ? WINDOW_PICKS : [...WINDOW_PICKS, Number(minutes)];
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!off && !/^([01]\d|2[0-3]):[0-5]\d$/.test(at)) {
      setError(t`Use 24-hour HH:MM, such as 03:00.`);
      return;
    }
    setError(undefined);
    if (unchanged) return;
    plan(off ? t`Stop updating ${name} on its own` : t`Update ${name} on its own`, [
      serviceEdit(service, (_, spec) => {
        spec.autoUpdate = next;
      }),
    ]);
  };
  return (
    <form className="app-settings-form" aria-label={t`Edit Automatic updates`} onSubmit={submit} noValidate>
      <SelectField
        label={t`Updates it takes on its own`}
        value={String(takes)}
        hint={
          takes === AutoUpdate_Level.ANY ? t`Never a database's major release.` : movingWords(name, app.image, takes)
        }
        onChange={(event) => setTakes(Number(event.target.value) as AutoUpdate_Level)}
        options={[
          AutoUpdate_Level.UNSPECIFIED,
          AutoUpdate_Level.PATCH,
          AutoUpdate_Level.MINOR,
          AutoUpdate_Level.ANY,
        ].map((l) => ({
          value: String(l),
          label: l === AutoUpdate_Level.UNSPECIFIED ? t`Off: each update waits for you` : level(l),
        }))}
      />
      {off ? null : (
        <>
          <SegmentedControl<UpdateDays>
            label={t({ message: "On", context: "days of the week" })}
            value={days}
            onChange={setDays}
            options={[
              { value: "every", label: t`Every day` },
              { value: "weekdays", label: t`Weekdays` },
              { value: "weekends", label: t`Weekends` },
              { value: "day", label: t`One day` },
            ]}
          />
          <div className="app-settings-grid">
            {days === "day" ? (
              <SelectField
                label={t`Day`}
                value={day}
                options={weekdays.map((label, i) => ({ value: String(i), label }))}
                onChange={(event) => setDay(event.target.value)}
              />
            ) : null}
            <TextField
              label={t`From`}
              mono
              value={at}
              hint={a?.timeZone ? t`In ${a.timeZone}` : undefined}
              error={error}
              onChange={(event) => setAt(event.target.value)}
            />
            <SelectField
              label={t`For`}
              value={minutes}
              options={minutePicks.map((m) => ({
                value: String(m),
                label: m % 60 === 0 ? t`${m / 60} h` : t`${m} min`,
              }))}
              onChange={(event) => setMinutes(event.target.value)}
            />
          </div>
          <Switch
            checked={backup}
            onChange={() => setBackup(!backup)}
            label={t`Back up first`}
            description={
              backup
                ? t`Each update backs up its volumes first`
                : t`Undo won't bring back what an update changes in its data`
            }
          />
          <p className="app-settings-help">
            <Trans>A failed update rolls back on its own, and alerts through the rule for failed changes.</Trans>
          </p>
        </>
      )}
      <Foot name={name} root={root} moving={moving} unchanged={unchanged} onCancel={onCancel} />
    </form>
  );
}

/** A mode as people write it, such as 0770. */
function modeText(mode: number): string {
  return `0${mode.toString(8)}`;
}
