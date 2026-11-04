// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type Change,
  ChangeService,
  ChangeState,
  type CheckComposeResponse,
  ComposeService,
  Connection,
  ResourceService,
} from "@dockwell/api";
import {
  Button,
  Callout,
  CodeField,
  cx,
  Glyph,
  Icon,
  PHONE_QUERY,
  SegmentedControl,
  SelectField,
  Sheet,
  StatusBadge,
  Switch,
  Tag,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import {
  type ChangeEvent,
  type ClipboardEvent,
  type FormEvent,
  type Ref,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
} from "react";

import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useFleetView } from "../servers/useFleet";
import { appOf, environmentOf, imageOf, nameFromImage, nameOf, SERVICE_NAME, tidyMessage } from "./app";
import {
  appEdits,
  type EnvRow,
  type FormErrors,
  fieldOf,
  type ImageForm,
  isServerPath,
  type MountRow,
  type PortRow,
  parseEnv,
  pathReach,
  validEnvName,
  validPort,
} from "./form";
import { usePlaces, useServices } from "./useServices";

export type NewAppSource = "image" | "compose";

/** A running count that gives each row of the form a key of its own. */
let rowKeys = 0;
const nextKey = () => ++rowKeys;

const CHECK_AFTER_MS = 300;

/**
 * New app (Apps-New): one sheet that takes an image or a Compose file and
 * ends in the plan. Nothing runs until the plan applies. An image needs only
 * its name; ports, variables and volumes wait behind a button each, and
 * where it runs shows only once there is a choice. A Compose file is read as
 * you type, says what it would make, and says at the line what Dockwell can't
 * run. Git and catalog sources come in phase 2, so they aren't here.
 */
export function NewAppSheet({
  source,
  onSource,
  onClose,
}: {
  source: NewAppSource;
  onSource: (source: NewAppSource) => void;
  onClose: () => void;
}) {
  const { t } = useLingui();
  const phone = useMediaQuery(PHONE_QUERY);
  const transport = useTransport();
  const navigate = useNavigate();
  const sheet = useChangeSheet();
  const { fleet, servers, only } = useFleetView();
  const places = usePlaces();
  const services = useServices().data;
  const changes = useMemo(() => createClient(ChangeService, transport), [transport]);
  const composer = useMemo(() => createClient(ComposeService, transport), [transport]);
  const form = useRef<HTMLFormElement>(null);
  const firstField = useRef<HTMLInputElement & HTMLTextAreaElement>(null);
  // Moving to the other source goes on where the typing is.
  const shownSource = useRef(source);
  useEffect(() => {
    if (shownSource.current === source) return;
    shownSource.current = source;
    firstField.current?.focus();
  }, [source]);

  const choosesEnvironment = places.environments.length > 1;
  const [environment, setEnvironment] = useState("");
  const scope = environment || places.environments[0]?.id || "";
  const [chosenServers, setChosenServers] = useState<readonly string[]>([]);

  const [image, setImage] = useState<ImageForm>({ image: "", name: "", ports: [], env: [], mounts: [] });
  const [errors, setErrors] = useState<FormErrors>({});
  const [file, setFile] = useState("");
  const [variables, setVariables] = useState<Readonly<Record<string, string>>>({});
  const [removeOrphans, setRemoveOrphans] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  const [planning, setPlanning] = useState(false);
  const [leaving, setLeaving] = useState(false);

  const volumes = useQuery(ResourceService.method.listResources, { kind: "volume", scope }, { enabled: scope !== "" })
    .data?.resources;
  const name = image.name.trim() || nameFromImage(image.image);
  const check = useComposeCheck(source === "compose" ? file : "", variables);

  const inEnvironment = useMemo(
    () => (services ?? []).filter((s) => !scope || environmentOf(s) === scope),
    [services, scope],
  );
  const orphans = useMemo(() => {
    const inFile = new Set((check.data?.services ?? []).map(nameOf));
    return check.data && inFile.size > 0 ? inEnvironment.map(nameOf).filter((n) => !inFile.has(n)) : [];
  }, [check.data, inEnvironment]);

  const missingVariables = (check.data?.variables ?? [])
    .filter((v) => v.required && !variables[v.name])
    .map((v) => v.name);
  const composeReady =
    file.trim() !== "" &&
    !check.isPending &&
    !check.error &&
    !check.data?.refusal &&
    (check.data?.services.length ?? 0) > 0;
  const needsPlace = fleet && chosenServers.length === 0;

  const update = (next: Partial<ImageForm>, clear?: string) => {
    setImage((was) => ({ ...was, ...next }));
    if (clear) setErrors(({ [clear]: _, ...rest }) => rest);
  };

  /** Checks what a person can fix here, before asking the API. */
  const validate = (): FormErrors => {
    const out: Record<string, string> = {};
    if (image.image.trim() === "") out["image"] = t`Name the image to run, such as nginx:1.27.`;
    else if (/\s/.test(image.image.trim())) out["image"] = t`An image has no spaces. Write it as nginx:1.27.`;
    if (image.image.trim() !== "" && !SERVICE_NAME.test(name)) {
      out["name"] = t`Use lowercase letters, digits and dashes, starting with a letter, such as web.`;
    } else if (inEnvironment.some((s) => nameOf(s) === name)) {
      out["name"] = t`An app named ${name} already runs here. Choose another name, or deploy to it from its page.`;
    }
    for (const row of image.ports) {
      if (!validPort(row.host)) out[`ports.${row.key}.host`] = t`Use a port from 1 to 65535.`;
      if (!validPort(row.container)) out[`ports.${row.key}.container`] = t`Use a port from 1 to 65535.`;
    }
    for (const row of image.env) {
      if (!validEnvName(row.name)) {
        out[`env.${row.key}.name`] = t`Use letters, digits and underscores, starting with a letter or underscore.`;
      }
    }
    for (const row of image.mounts) {
      if (row.source.trim() === "")
        out[`mounts.${row.key}.source`] = t`Name a volume, or an absolute path on the server.`;
      if (!row.target.trim().startsWith("/")) out[`mounts.${row.key}.target`] = t`Use an absolute path, such as /data.`;
    }
    return out;
  };

  /** Takes you to the first field that was refused, which may have scrolled away. */
  const revealError = () =>
    requestAnimationFrame(() => {
      form.current?.querySelector<HTMLElement>('[aria-invalid="true"]')?.focus();
    });

  const review = (change: Change | undefined, open: "app" | "list") => {
    const service = change?.plan?.resources.find((r) => r.resource?.kind === "service");
    const label = service?.resource?.name ?? "";
    setPlanning(true);
    sheet.open({
      kind: "plan",
      make: () => Promise.resolve({ change }),
      ...(open === "app" && label ? { doneLabel: t`Open ${label}` } : {}),
      ...(open === "list" ? { doneLabel: t`Show the apps` } : {}),
      onClose: (after) => {
        // Cancelled, or it failed and came back as it was: back to the form, as you left it.
        if (
          !after ||
          after.state === ChangeState.PLANNED ||
          after.state === ChangeState.DISCARDED ||
          after.state === ChangeState.FAILED
        ) {
          setPlanning(false);
          return;
        }
        if (open === "app" && service) void navigate({ to: "/apps/$serviceId", params: { serviceId: service.id } });
        else void navigate({ to: "/apps" });
      },
    });
  };

  const submit = async (event?: FormEvent) => {
    event?.preventDefault();
    if (busy) return;
    setFailure(undefined);
    if (needsPlace) {
      setErrors({ servers: t`Choose where it runs.` });
      return;
    }
    if (source === "compose") {
      if (!composeReady) return;
      setBusy(true);
      try {
        const count = check.data?.services.length ?? 0;
        const res = await composer.importCompose({
          summary: plural(count, { one: "Run a Compose file of # service", other: "Run a Compose file of # services" }),
          compose: file,
          environment: choosesEnvironment ? scope : "",
          variables,
          servers: [...chosenServers],
          removeOrphans,
        });
        review(res.change, count === 1 ? "app" : "list");
      } catch (err) {
        setFailure(sentence(err));
      } finally {
        setBusy(false);
      }
      return;
    }
    const found = validate();
    setErrors(found);
    if (Object.keys(found).length > 0) {
      revealError();
      return;
    }
    setBusy(true);
    try {
      const wanted = image.image.trim();
      const res = await changes.proposeChange({
        summary: t`Run ${wanted} as ${name}`,
        edits: appEdits(image, name, {
          environment: choosesEnvironment ? scope : "",
          servers: chosenServers,
          volumes: (volumes ?? []).map((v) => v.resource?.name ?? ""),
        }),
      });
      review(res.change, "app");
    } catch (err) {
      const message = tidyMessage(sentence(err));
      const field = reasonOf(err) === "invalid" ? fieldOf(message, image, name) : "";
      if (field) {
        setErrors({ [field]: message });
        revealError();
      } else setFailure(message);
    } finally {
      setBusy(false);
    }
  };

  // What took typing isn't lost to a stray click on the scrim, or a stray Escape.
  const typed =
    image.image !== "" ||
    image.name !== "" ||
    image.ports.length > 0 ||
    image.env.length > 0 ||
    image.mounts.length > 0 ||
    file !== "";
  const leave = () => {
    if (typed && !leaving) setLeaving(true);
    else onClose();
  };
  const onlyName = only?.name ?? "";
  const subtitle = only && !choosesEnvironment ? t`Runs on ${onlyName}` : undefined;

  const hostReach = source === "image" && image.mounts.some((m) => pathReach(m.source));
  const fileReach =
    source === "compose" &&
    (check.data?.services ?? []).some((s) => (appOf(s)?.mounts ?? []).some((m) => m.source.case === "hostPath"));

  return (
    <Sheet
      open={!planning}
      initialFocusRef={firstField}
      title={t`New app`}
      subtitle={subtitle}
      onClose={leave}
      className={cx("app-sheet", "new-app", phone && "is-phone")}
      footer={
        leaving ? (
          <>
            <span className="dw-sheet-note" role="status">
              <Trans>Close, and lose what you typed here?</Trans>
            </span>
            <Button variant="ghost" size={phone ? "lg" : "md"} onClick={onClose}>
              <Trans>Discard</Trans>
            </Button>
            <Button variant="primary" size={phone ? "lg" : "md"} onClick={() => setLeaving(false)}>
              <Trans>Keep editing</Trans>
            </Button>
          </>
        ) : (
          <>
            <span className="dw-sheet-note">
              <Icon name={hostReach || fileReach ? "shield" : "commit"} size={14} />
              {hostReach || fileReach ? (
                <Trans>You approve what it reaches in the plan</Trans>
              ) : (
                <Trans>Nothing runs until you apply the plan</Trans>
              )}
            </span>
            <Button variant="ghost" size={phone ? "lg" : "md"} onClick={leave}>
              <Trans>Cancel</Trans>
            </Button>
            <Button
              variant="primary"
              size={phone ? "lg" : "md"}
              loading={busy}
              disabled={source === "compose" && !composeReady}
              kbd={phone ? undefined : onApple() ? "⌘↵" : "Ctrl ↵"}
              onClick={() => void submit()}
            >
              <Trans>Review plan</Trans>
            </Button>
          </>
        )
      }
    >
      <form
        ref={form}
        className="app-form"
        onSubmit={(event) => void submit(event)}
        onKeyDown={(event) => {
          if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) void submit(event);
        }}
      >
        {/* Lets Enter in a field do what the button in the sheet's footer does. */}
        <button type="submit" hidden />
        {failure ? (
          <Callout tone="critical" title={t`That didn't plan`}>
            {failure}
          </Callout>
        ) : null}
        <div className="app-field">
          <span className="dw-field-label" id="new-app-source">
            <Trans>Source</Trans>
          </span>
          <div>
            <SegmentedControl<NewAppSource>
              label={t`Source`}
              value={source}
              onChange={onSource}
              options={[
                { value: "image", label: t`Image`, icon: "layers" },
                { value: "compose", label: t`Compose`, icon: "file" },
              ]}
            />
          </div>
        </div>
        {choosesEnvironment ? (
          <SelectField
            label={t`Environment`}
            value={scope}
            options={places.environments.map((e) => ({ value: e.id, label: e.label }))}
            onChange={(event) => setEnvironment(event.target.value)}
          />
        ) : null}
        {source === "image" ? (
          <ImageFields form={image} errors={errors} update={update} onError={setErrors} fieldRef={firstField} />
        ) : (
          <ComposeFields
            file={file}
            onFile={setFile}
            variables={variables}
            onVariables={setVariables}
            check={check}
            missing={missingVariables}
            fieldRef={firstField}
          />
        )}
        {source === "compose" && orphans.length > 0 ? (
          <Switch
            label={t`Remove what isn't in this file`}
            description={plural(orphans.length, {
              one: `${orphans.join(", ")} is here already, and not in the file. Otherwise it stays as it is.`,
              other: `${orphans.join(", ")} are here already, and not in the file. Otherwise they stay as they are.`,
            })}
            checked={removeOrphans}
            onChange={setRemoveOrphans}
          />
        ) : null}
        {fleet ? (
          <RunsOnField
            value={chosenServers}
            onChange={(next) => {
              setChosenServers(next);
              setErrors(({ servers: _, ...rest }) => rest);
            }}
            error={errors["servers"]}
            servers={servers ?? []}
          />
        ) : null}
      </form>
    </Sheet>
  );
}

/** The image, its name, and what else it takes once you ask for it. */
function ImageFields({
  form,
  errors,
  update,
  onError,
  fieldRef,
}: {
  fieldRef: Ref<HTMLInputElement & HTMLTextAreaElement>;
  form: ImageForm;
  errors: FormErrors;
  update: (next: Partial<ImageForm>, clear?: string) => void;
  onError: (errors: FormErrors) => void;
}) {
  const { t } = useLingui();
  const clear = (key: string) => {
    if (errors[key]) {
      const { [key]: _, ...rest } = errors;
      onError(rest);
    }
  };
  const placeholderName = nameFromImage(form.image) || "web";
  return (
    <>
      <TextField
        ref={fieldRef}
        label={t`Image`}
        mono
        placeholder="nginx:1.27"
        value={form.image}
        error={errors["image"]}
        hint={t`Any image your server can pull, from Docker Hub or another registry.`}
        onChange={(event) => update({ image: event.target.value }, "image")}
      />
      <TextField
        label={t`Name`}
        mono
        optional={form.name === ""}
        placeholder={placeholderName}
        value={form.name}
        error={errors["name"]}
        hint={t`Other apps on its network reach it by this name.`}
        onChange={(event) => update({ name: event.target.value }, "name")}
      />
      <div className="app-rule" />
      <PortsSection form={form} errors={errors} update={update} clear={clear} />
      <EnvSection form={form} errors={errors} update={update} clear={clear} />
      <MountsSection form={form} errors={errors} update={update} clear={clear} />
    </>
  );
}

interface SectionProps {
  form: ImageForm;
  errors: FormErrors;
  update: (next: Partial<ImageForm>, clear?: string) => void;
  clear: (key: string) => void;
}

function SectionHead({
  title,
  id,
  add,
  onAdd,
  children,
}: {
  title: string;
  id: string;
  add: string;
  onAdd: () => void;
  children?: React.ReactNode;
}) {
  return (
    <div className="app-section-head">
      <div className="app-section-text">
        <span id={id} className="dw-field-label">
          {title}
        </span>
        {children ? <span className="dw-field-hint">{children}</span> : null}
      </div>
      <Button variant="ghost" size="sm" icon="plus" onClick={onAdd} className="app-section-add">
        {add}
      </Button>
    </div>
  );
}

function PortsSection({ form, errors, update, clear }: SectionProps) {
  const { t } = useLingui();
  const id = useId();
  const add = () => update({ ports: [...form.ports, { key: nextKey(), host: "", container: "", protocol: "tcp" }] });
  const change = (row: PortRow, next: Partial<PortRow>) =>
    update({ ports: form.ports.map((p) => (p.key === row.key ? { ...p, ...next } : p)) });
  return (
    <fieldset className="app-section" aria-labelledby={id}>
      <SectionHead title={t`Ports`} id={id} add={t`Add port`} onAdd={add}>
        {form.ports.length === 0 ? t`Nothing outside the server reaches it until you publish a port.` : null}
      </SectionHead>
      {form.ports.map((row) => (
        <div key={row.key} className="app-row-group">
          <div className="app-row app-port">
            <TextField
              label={t`On the server`}
              mono
              inputMode="numeric"
              autoFocus={row.host === "" && row.container === ""}
              placeholder="8080"
              value={row.host}
              invalid={Boolean(errors[`ports.${row.key}.host`])}
              aria-describedby={`${id}-${row.key}`}
              onChange={(event) => {
                change(row, { host: event.target.value });
                clear(`ports.${row.key}.host`);
              }}
            />
            <Icon name="arrow-right" size={14} className="app-port-arrow" />
            <TextField
              label={t`In the app`}
              mono
              inputMode="numeric"
              placeholder="80"
              value={row.container}
              invalid={Boolean(errors[`ports.${row.key}.container`])}
              aria-describedby={`${id}-${row.key}`}
              onChange={(event) => {
                change(row, { container: event.target.value });
                clear(`ports.${row.key}.container`);
              }}
            />
            <SelectField
              label={t`Protocol`}
              className="app-port-protocol"
              value={row.protocol}
              options={[
                { value: "tcp", label: "TCP" },
                { value: "udp", label: "UDP" },
              ]}
              onChange={(event: ChangeEvent<HTMLSelectElement>) =>
                change(row, { protocol: event.target.value === "udp" ? "udp" : "tcp" })
              }
            />
            <RemoveRow
              label={t`Remove this port`}
              onClick={() => update({ ports: form.ports.filter((p) => p.key !== row.key) })}
            />
          </div>
          <RowError
            id={`${id}-${row.key}`}
            message={errors[`ports.${row.key}.host`] ?? errors[`ports.${row.key}.container`]}
          />
        </div>
      ))}
    </fieldset>
  );
}

function EnvSection({ form, errors, update, clear }: SectionProps) {
  const { t } = useLingui();
  const id = useId();
  const add = () => update({ env: [...form.env, { key: nextKey(), name: "", value: "" }] });
  const change = (row: EnvRow, next: Partial<EnvRow>) =>
    update({ env: form.env.map((e) => (e.key === row.key ? { ...e, ...next } : e)) });
  const paste = (row: EnvRow, event: ClipboardEvent<HTMLInputElement>) => {
    const text = event.clipboardData.getData("text");
    if (!text.includes("=")) return;
    const parsed = parseEnv(text);
    if (!parsed) return;
    event.preventDefault();
    const rows = parsed.map((e, i) => ({ key: i === 0 ? row.key : nextKey(), ...e }));
    const at = form.env.findIndex((e) => e.key === row.key);
    update({ env: [...form.env.slice(0, at), ...rows, ...form.env.slice(at + 1)] });
  };
  return (
    <fieldset className="app-section" aria-labelledby={id}>
      <SectionHead title={t`Environment variables`} id={id} add={t`Add variable`} onAdd={add}>
        {t`Plans say a value changed, and never show it.`}
      </SectionHead>
      {form.env.map((row) => (
        <div key={row.key} className="app-row-group">
          <div className="app-row app-env">
            <TextField
              label={t`Name`}
              mono
              autoFocus={row.name === ""}
              placeholder="LOG_LEVEL"
              value={row.name}
              invalid={Boolean(errors[`env.${row.key}.name`])}
              aria-describedby={`${id}-${row.key}`}
              onPaste={(event) => paste(row, event)}
              onChange={(event) => {
                change(row, { name: event.target.value });
                clear(`env.${row.key}.name`);
              }}
            />
            <TextField
              label={t`Value`}
              mono
              autoComplete="off"
              value={row.value}
              onChange={(event) => change(row, { value: event.target.value })}
            />
            <RemoveRow
              label={t`Remove this variable`}
              onClick={() => update({ env: form.env.filter((e) => e.key !== row.key) })}
            />
          </div>
          <RowError id={`${id}-${row.key}`} message={errors[`env.${row.key}.name`]} />
        </div>
      ))}
    </fieldset>
  );
}

function MountsSection({ form, errors, update, clear }: SectionProps) {
  const { t } = useLingui();
  const id = useId();
  const add = () => update({ mounts: [...form.mounts, { key: nextKey(), source: "", target: "", readOnly: false }] });
  const change = (row: MountRow, next: Partial<MountRow>) =>
    update({ mounts: form.mounts.map((m) => (m.key === row.key ? { ...m, ...next } : m)) });
  return (
    <fieldset className="app-section" aria-labelledby={id}>
      <SectionHead title={t`Volumes`} id={id} add={t`Add volume`} onAdd={add}>
        {form.mounts.length === 0 ? t`What it writes elsewhere goes when its container is replaced.` : null}
      </SectionHead>
      {form.mounts.map((row) => {
        const refused = errors[`mounts.${row.key}.source`] ?? errors[`mounts.${row.key}.target`];
        // Once it is refused, what it would have reached is beside the point.
        const reach = refused ? undefined : pathReach(row.source);
        const source = row.source.trim();
        return (
          <div key={row.key} className="app-row-group">
            <div className="app-row app-mount-row">
              <TextField
                label={t`Volume or server path`}
                mono
                autoFocus={row.source === ""}
                placeholder="data"
                value={row.source}
                invalid={Boolean(errors[`mounts.${row.key}.source`])}
                aria-describedby={`${id}-${row.key}`}
                onChange={(event) => {
                  change(row, { source: event.target.value });
                  clear(`mounts.${row.key}.source`);
                }}
              />
              <TextField
                label={t`Path in the app`}
                mono
                placeholder="/data"
                value={row.target}
                invalid={Boolean(errors[`mounts.${row.key}.target`])}
                aria-describedby={`${id}-${row.key}`}
                onChange={(event) => {
                  change(row, { target: event.target.value });
                  clear(`mounts.${row.key}.target`);
                }}
              />
              <RemoveRow
                label={t`Remove this volume`}
                onClick={() => update({ mounts: form.mounts.filter((m) => m.key !== row.key) })}
              />
            </div>
            <RowError id={`${id}-${row.key}`} message={refused} />
            <label className="app-check">
              <input
                type="checkbox"
                checked={row.readOnly}
                onChange={(event) => change(row, { readOnly: event.target.checked })}
              />
              <span>{t`Read-only`}</span>
            </label>
            {reach === "socket" ? (
              <p className="app-reach-note is-danger">
                <Icon name="shield" size={14} />
                <span>
                  <Trans>
                    <code>{source}</code> gives this app control of Docker on the server, the same as root. A signed-in
                    person has to approve that in the plan.
                  </Trans>
                </span>
              </p>
            ) : reach === "path" ? (
              <p className="app-reach-note">
                <Icon name="shield" size={14} />
                <span>
                  <Trans>
                    This reaches <code>{source}</code> on the server. A signed-in person has to approve that in the
                    plan.
                  </Trans>
                </span>
              </p>
            ) : isServerPath(row.source) ? null : source !== "" ? (
              <p className="app-field-note">
                <Trans>
                  Dockwell makes the volume <code>{source}</code> if it doesn't exist yet.
                </Trans>
              </p>
            ) : null}
          </div>
        );
      })}
    </fieldset>
  );
}

/** What's wrong with a row's fields, under the row, across its width. */
function RowError({ id, message }: { id: string; message: string | undefined }) {
  if (!message) return null;
  return (
    <div id={id} className="dw-field-error app-row-error">
      <Glyph shape="square" size={9} />
      <span>{message}</span>
    </div>
  );
}

function RemoveRow({ label, onClick }: { label: string; onClick: () => void }) {
  return <Button variant="ghost" icon="x" aria-label={label} onClick={onClick} className="app-row-remove" />;
}

/** Reads a Compose file as it is typed, a moment after each key. */
function useComposeCheck(file: string, variables: Readonly<Record<string, string>>) {
  const [settled, setSettled] = useState({ file, variables });
  useEffect(() => {
    const timer = setTimeout(() => setSettled({ file, variables }), CHECK_AFTER_MS);
    return () => clearTimeout(timer);
  }, [file, variables]);
  const query = useQuery(
    ComposeService.method.checkCompose,
    { compose: settled.file, variables: settled.variables },
    { enabled: settled.file.trim() !== "", placeholderData: (previous) => previous, staleTime: 60_000 },
  );
  // Until the file settles, what is shown is about an older one, so Review plan waits.
  const waiting = file !== settled.file || variables !== settled.variables || query.isFetching;
  return { data: file.trim() === "" ? undefined : query.data, error: query.error, isPending: waiting };
}

function ComposeFields({
  file,
  onFile,
  variables,
  onVariables,
  check,
  missing,
  fieldRef,
}: {
  fieldRef: Ref<HTMLInputElement & HTMLTextAreaElement>;
  file: string;
  onFile: (file: string) => void;
  variables: Readonly<Record<string, string>>;
  onVariables: (variables: Readonly<Record<string, string>>) => void;
  check: ReturnType<typeof useComposeCheck>;
  missing: readonly string[];
}) {
  const { t } = useLingui();
  const input = useRef<HTMLInputElement>(null);
  const [showOptional, setShowOptional] = useState(false);
  const refusal = check.data?.refusal;
  const allVars = check.data?.variables ?? [];
  const required = allVars.filter((v) => v.required);
  const optional = allVars.filter((v) => !v.required);
  // A refusal of a variable that has no value yet is what the fields below already ask for.
  const refusedMessage =
    refusal && !missing.some((name) => refusal.message.includes(name)) ? refusal.message : undefined;
  const readFile = async (chosen: File | undefined) => {
    if (chosen) onFile(await chosen.text());
  };
  return (
    <>
      {/* biome-ignore lint/a11y/noStaticElementInteractions: dropping a file is a shortcut for the button beside the label */}
      <div
        className="app-field"
        onDrop={(event) => {
          event.preventDefault();
          void readFile(event.dataTransfer.files[0]);
        }}
        onDragOver={(event) => event.preventDefault()}
      >
        <CodeField
          ref={fieldRef}
          label={t`Compose file`}
          lines={12}
          value={file}
          onChange={(event) => onFile(event.target.value)}
          placeholder={'services:\n  web:\n    image: nginx:1.27\n    ports:\n      - "8080:80"'}
          error={refusedMessage ? capitalize(refusedMessage) : undefined}
          markedLine={refusedMessage ? refusal?.line : undefined}
          hint={t`Paste it, or drop the file here. Dockwell reads it as you type, and nothing is saved until you apply the plan.`}
          actions={
            <>
              <input
                ref={input}
                type="file"
                className="dw-sr"
                tabIndex={-1}
                aria-hidden="true"
                accept=".yml,.yaml,text/yaml,application/yaml"
                onChange={(event) => {
                  void readFile(event.target.files?.[0]);
                  event.target.value = "";
                }}
              />
              <Button variant="ghost" size="sm" icon="upload" onClick={() => input.current?.click()}>
                <Trans>Choose a file</Trans>
              </Button>
            </>
          }
        />
      </div>
      {check.error ? (
        <Callout tone="critical" title={t`Can't read the file yet`}>
          {sentence(check.error)}
        </Callout>
      ) : null}
      {required.length > 0 || optional.length > 0 ? (
        <fieldset className="app-section" aria-label={t`Variables`}>
          <div className="app-section-head">
            <div className="app-section-text">
              <span className="dw-field-label">
                <Trans>Variables</Trans>
              </span>
              {required.length > 0 ? (
                <span className="dw-field-hint">
                  <Trans>The file names these, with no default. They aren't saved with it.</Trans>
                </span>
              ) : null}
            </div>
            {optional.length > 0 ? (
              <Button
                variant="ghost"
                size="sm"
                icon={showOptional ? "chevron-down" : "chevron-right"}
                aria-expanded={showOptional}
                onClick={() => setShowOptional((was) => !was)}
              >
                {plural(optional.length, { one: "# with a default", other: "# with a default" })}
              </Button>
            ) : null}
          </div>
          <div className="app-vars">
            {[...required, ...(showOptional ? optional : [])].map((v) => (
              <TextField
                key={v.name}
                label={<code className="app-var-name">{v.name}</code>}
                optional={!v.required}
                mono
                autoComplete="off"
                value={variables[v.name] ?? ""}
                onChange={(event) => onVariables({ ...variables, [v.name]: event.target.value })}
              />
            ))}
          </div>
        </fieldset>
      ) : null}
      <ComposePreview check={check.data} />
    </>
  );
}

function capitalize(text: string): string {
  const out = text.charAt(0).toUpperCase() + text.slice(1);
  return /[.!?]$/.test(out) ? out : `${out}.`;
}

/** What the file would make, as the API reads it. */
function ComposePreview({ check }: { check: CheckComposeResponse | undefined }) {
  const { t } = useLingui();
  const services = check?.services ?? [];
  if (services.length === 0) return null;
  const volumes = check?.volumes ?? [];
  const networks = check?.networks ?? [];
  return (
    <section className="compose-preview" aria-label={t`What this file makes`}>
      <h3 className="dw-text-label">
        {plural(services.length, { one: "This makes # service", other: "This makes # services" })}
      </h3>
      <ul className="compose-services">
        {services.map((service) => {
          const app = appOf(service);
          const hostPaths = (app?.mounts ?? []).flatMap((m) => (m.source.case === "hostPath" ? [m.source.value] : []));
          const ports = (app?.ports ?? []).map((p) => `${p.hostPort}:${p.containerPort}`);
          const secrets = Object.keys(app?.env ?? {}).length;
          return (
            <li key={nameOf(service)} className="compose-service">
              <span className="compose-service-head">
                <span className="compose-service-name">{nameOf(service)}</span>
                <code className="compose-service-image">{imageOf(service)}</code>
              </span>
              {ports.length > 0 || hostPaths.length > 0 || secrets > 0 ? (
                <span className="compose-service-tags">
                  {ports.map((port) => (
                    <Tag key={port} icon="globe">
                      {port}
                    </Tag>
                  ))}
                  {secrets > 0 ? (
                    <Tag icon="key">{plural(secrets, { one: "# variable", other: "# variables" })}</Tag>
                  ) : null}
                  {hostPaths.map((path) => (
                    <StatusBadge key={path} status="degraded" label={t`Reaches ${path}`} />
                  ))}
                </span>
              ) : null}
            </li>
          );
        })}
      </ul>
      {volumes.length > 0 || networks.length > 0 ? (
        <dl className="compose-rest">
          {volumes.length > 0 ? (
            <>
              <dt>{plural(volumes.length, { one: "Volume", other: "Volumes" })}</dt>
              <dd>
                {volumes.map((v) => (
                  <code key={v}>{v}</code>
                ))}
              </dd>
            </>
          ) : null}
          {networks.length > 0 ? (
            <>
              <dt>{plural(networks.length, { one: "Network", other: "Networks" })}</dt>
              <dd>
                {networks.map((n) => (
                  <code key={n}>{n}</code>
                ))}
              </dd>
            </>
          ) : null}
        </dl>
      ) : null}
    </section>
  );
}

/** Where it runs, once there is a choice (PRD principle 4). There is no scheduler yet, so a person says. */
function RunsOnField({
  value,
  onChange,
  error,
  servers,
}: {
  value: readonly string[];
  onChange: (next: readonly string[]) => void;
  error: string | undefined;
  servers: NonNullable<ReturnType<typeof useFleetView>["servers"]>;
}) {
  const { t } = useLingui();
  const id = useId();
  return (
    <fieldset className="app-section" aria-labelledby={id}>
      <span id={id} className="dw-field-label">
        <Trans>Runs on</Trans>
      </span>
      <div className="app-servers">
        {servers.map((server) => (
          <label key={server.id} className="app-check is-server">
            <input
              type="checkbox"
              checked={value.includes(server.id)}
              onChange={(event) =>
                onChange(event.target.checked ? [...value, server.id] : value.filter((s) => s !== server.id))
              }
            />
            <code className="app-server-name">{server.name}</code>
            {server.connection === Connection.CONNECTED ? null : <Tag>{t`Offline`}</Tag>}
          </label>
        ))}
      </div>
      {error ? (
        <span className="dw-field-error" role="alert">
          {error}
        </span>
      ) : null}
    </fieldset>
  );
}
