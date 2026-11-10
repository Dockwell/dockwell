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
  type ExposedVariable,
  ResourceService,
  RestartPolicy,
} from "@dockwell/api";
import {
  Advanced,
  Button,
  Callout,
  CodeField,
  cx,
  Glyph,
  Icon,
  PHONE_QUERY,
  PortField,
  SegmentedControl,
  SelectField,
  Sheet,
  StatusBadge,
  Switch,
  Tag,
  TextField,
  type TextFieldProps,
  useMediaQuery,
  useUIStrings,
} from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useNavigate } from "@tanstack/react-router";
import {
  type ChangeEvent,
  type ClipboardEvent,
  type FormEvent,
  Fragment,
  type Ref,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
} from "react";

import { editCommand } from "../../lib/cli";
import { reasonOf, sentence } from "../../lib/errors";
import { onApple } from "../../lib/platform";
import { type Opening, PlanView } from "../changes/ChangeSheet";
import { type PortHolder, type ServerPorts, useServerPorts } from "../servers/ports";
import { useFleetView } from "../servers/useFleet";
import { CopyAsCommand } from "../system/CopyCommand";
import {
  appOf,
  environmentOf,
  failedAtField,
  imageOf,
  nameFromImage,
  nameOf,
  portRefusal,
  SERVICE_NAME,
  tidyMessage,
  toLabel,
} from "./app";
import { composeRefusal, type RefusedAt, waitsForValues, withCode } from "./composeWords";
import {
  appEdits,
  changedSettings,
  checkPort,
  DEFAULT_SETTINGS,
  type EnvRow,
  type FormErrors,
  fieldOf,
  type ImageForm,
  isServerPath,
  type MoreSettings,
  type MountRow,
  type PortRow,
  parseEnv,
  pathReach,
  validEnvName,
  validPort,
} from "./form";
import { joinWords, splitWords, validCpus, validMemory } from "./specEdit";
import { usePlaces, useServices } from "./useServices";
import { useAppWords } from "./words";

/**
 * Where a new app comes from. Each is one more choice beside these, as a Git
 * repository (#336) and a folder with a runtime (#358) will be.
 */
export type NewAppSource = "image" | "compose";

/** A running count that gives each row of the form a key of its own. */
let rowKeys = 0;
const nextKey = () => ++rowKeys;

const CHECK_AFTER_MS = 300;

/** The largest Compose file the API reads (compose.MaxFile). A larger one gets a plain word here, rather than a protocol error. */
const MAX_FILE = 256 << 10;

/** How many lines the Compose field starts with, and the most it grows to before it scrolls. */
const FILE_LINES = 12;

/** How many lines of an .env show before it scrolls. */
const ENV_LINES = 6;

/**
 * New app (Apps-New): one sheet that takes an image or a Compose file and
 * ends in the plan. Nothing runs until the plan applies. An image needs only
 * its name; ports, variables and volumes wait behind a button each, and
 * where it runs shows only once there is a choice. A Compose file gets a
 * project of its own, named from the file (ADR 0026); it is read as you type,
 * says what it would make, and says at the line what Dockwell can't run. Git
 * sources come in phase 2, and the catalog has a page of its own.
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

  const [environment, setEnvironment] = useState("");
  const [chosenServers, setChosenServers] = useState<readonly string[]>([]);

  const [image, setImage] = useState<ImageForm>({ image: "", name: "", ports: [], env: [], mounts: [] });
  const [errors, setErrors] = useState<FormErrors>({});
  const [file, setFile] = useState("");
  const [typedProject, setTypedProject] = useState<string>();
  const [variables, setVariables] = useState<Readonly<Record<string, string>>>({});
  // An .env holds secrets: it lives here and in what is sent, and in nothing kept.
  const [envFile, setEnvFile] = useState("");
  const [directory, setDirectory] = useState("");
  const [removeOrphans, setRemoveOrphans] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string>();
  // The plan under review takes this sheet over, and Cancel brings the form back as it was.
  const [plan, setPlan] = useState<Opening>();
  const [leaving, setLeaving] = useState(false);

  const fileBytes = useMemo(() => new TextEncoder().encode(file).length, [file]);
  const fileTooBig = fileBytes > MAX_FILE;
  const check = useComposeCheck(source === "compose" && !fileTooBig ? file : "", variables, envFile, directory);

  // A Compose file goes in the project it names, and an image in the one chosen.
  const project = typedProject ?? toLabel(check.data?.name ?? "");
  const projectName = project.trim();
  const projectKnown = places.projects.includes(projectName);
  const defaultPlace = places.defaultEnvironment;
  const defaultHasApps = (services ?? []).some((s) => places.place(environmentOf(s))?.isDefault);
  const fileEnvironments = useMemo(
    () => places.environments.filter((e) => (projectName ? e.project === projectName : e.isDefault)),
    [places.environments, projectName],
  );
  const choices = source === "compose" ? fileEnvironments : places.environments;
  const choosesEnvironment = choices.length > 1;
  // A file's project may be new, and then nothing here is its environment.
  const scope =
    (choices.some((e) => e.id === environment) ? environment : "") ||
    (source === "compose" ? fileEnvironments[0]?.id : defaultPlace?.id) ||
    "";

  const volumes = useQuery(ResourceService.method.listResources, { kind: "volume", scope }, { enabled: scope !== "" })
    .data?.resources;
  const volumeNames = useMemo(() => (volumes ?? []).map((v) => v.resource?.name ?? ""), [volumes]);
  const name = image.name.trim() || nameFromImage(image.image);
  // What's free is read on the servers it will run on: the one there is, or those chosen.
  const onlyId = only?.id;
  const serverIds = useMemo(
    () => (fleet ? [...chosenServers] : onlyId ? [onlyId] : []),
    [fleet, chosenServers, onlyId],
  );
  const ports = useServerPorts(serverIds);
  // The form as it plans: each port that follows a free one has it.
  const shown = useMemo(() => withFreePorts(image, ports), [image, ports]);
  const [advancedOpen, setAdvancedOpen] = useState(false);
  const address = servers?.find((s) => s.id === serverIds[0])?.address;

  const inEnvironment = useMemo(
    () => (services ?? []).filter((s) => !scope || environmentOf(s) === scope),
    [services, scope],
  );
  const orphans = useMemo(() => {
    const inFile = new Set((check.data?.services ?? []).map(nameOf));
    if (!check.data || inFile.size === 0 || (projectName !== "" && !projectKnown)) return [];
    return inEnvironment.map(nameOf).filter((n) => !inFile.has(n));
  }, [check.data, inEnvironment, projectName, projectKnown]);

  const missingVariables = (check.data?.variables ?? [])
    .filter((v) => v.required && !variables[v.name])
    .map((v) => v.name);
  const projectInvalid = projectName !== "" && !SERVICE_NAME.test(projectName);
  const projectMissing = projectName === "" && defaultHasApps;
  const composeReady =
    file.trim() !== "" &&
    !fileTooBig &&
    !check.isPending &&
    !check.error &&
    !check.data?.refusal &&
    !projectInvalid &&
    !projectMissing &&
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
    for (const row of shown.ports) {
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
    const more = image.more ?? DEFAULT_SETTINGS;
    if (splitWords(more.command) === undefined)
      out["more.command"] = t`A quote isn't closed. Close it, or leave it out.`;
    if (more.memory.trim() !== "" && !validMemory(more.memory))
      out["more.memory"] = t`Use an amount from 6M, such as 512M or 2G.`;
    if (more.cpus.trim() !== "" && !validCpus(more.cpus))
      out["more.cpus"] = t`Use a number of CPUs above 0, such as 0.5 or 2.`;
    if (more.healthPath.trim() !== "" && !more.healthPath.trim().startsWith("/"))
      out["more.healthPath"] = t`Start it with /, such as /healthz.`;
    return out;
  };

  /** Takes you to the first field that was refused, which may have scrolled away, ready to change. */
  const revealError = () =>
    requestAnimationFrame(() => {
      const refused = form.current?.querySelector<HTMLInputElement>('[aria-invalid="true"]');
      refused?.focus();
      refused?.select();
    });

  // Back from a plan that failed, what it said is at the field.
  const revealAfter = useRef(false);
  // biome-ignore lint/correctness/useExhaustiveDependencies: it runs when the form comes back, and revealError reads only what's on screen
  useEffect(() => {
    if (plan || !revealAfter.current) return;
    revealAfter.current = false;
    revealError();
  }, [plan]);

  const fix = (reason: string) => {
    // A Compose file has a command to check, which is what the server says. This form hasn't.
    if (source === "compose") return undefined;
    // A check that fails says what it answered, and what to do about that, which is the server's.
    if (reason === "exits") return t`Check the image and its variables, then try again.`;
    return reason === "command" ? t`Check the image and tag, then try again.` : undefined;
  };

  const review = (change: Change | undefined, open: "app" | "list") => {
    const service = change?.plan?.resources.find((r) => r.resource?.kind === "service");
    const label = service?.resource?.name ?? "";
    setPlan({
      kind: "plan",
      make: () => Promise.resolve({ change }),
      retryLabel: t`Edit and try again`,
      onDone: onClose,
      fix,
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
          if (after?.state === ChangeState.FAILED) {
            const message = failedAtField(after, name, fix);
            if (source === "image") {
              // What ran is what to change, unless it says what else was wrong.
              setErrors({ [fieldOf(message, shown, name) || "image"]: message });
              revealAfter.current = true;
            } else setFailure(message);
          }
          setPlan(undefined);
          return;
        }
        // Undone in the same sheet, it has no page to open.
        if (open === "app" && service && after.reverts === "") {
          void navigate({ to: "/apps/$serviceId", params: { serviceId: service.id } });
        } else void navigate({ to: "/apps" });
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
          summary: projectName ? t`Run ${projectName} from a Compose file` : t`Run a Compose file`,
          project: projectName,
          compose: file,
          environment: choosesEnvironment ? scope : "",
          variables,
          env: envFile,
          directory: directory.trim(),
          servers: [...chosenServers],
          removeOrphans,
        });
        review(res.change, count === 1 ? "app" : "list");
      } catch (err) {
        if (reasonOf(err) === "needs_project") setErrors({ project: sentence(err) });
        else {
          const message = tidyMessage(sentence(err));
          setFailure(portRefusal(message) ?? message);
        }
      } finally {
        setBusy(false);
      }
      return;
    }
    const found = validate();
    setErrors(found);
    if (Object.keys(found).length > 0) {
      // What it refused under Advanced has to be in sight to be fixed.
      if (Object.keys(found).some((key) => key.startsWith("more."))) setAdvancedOpen(true);
      revealError();
      return;
    }
    setBusy(true);
    try {
      const wanted = image.image.trim();
      const res = await changes.proposeChange({
        summary: t`Run ${wanted} as ${name}`,
        edits: appEdits(shown, name, {
          environment: choosesEnvironment ? scope : "",
          servers: chosenServers,
          volumes: volumeNames,
        }),
      });
      review(res.change, "app");
    } catch (err) {
      const message = tidyMessage(sentence(err));
      const field = reasonOf(err) === "invalid" ? fieldOf(message, shown, name) : "";
      if (field) {
        setErrors({ [field]: message });
        revealError();
      } else setFailure(portRefusal(message) ?? message);
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
  // The same for an image and a file, so the header stays where it is between them.
  const subtitle = only ? t`Runs on ${onlyName}` : undefined;

  // Why Review plan can't be pressed yet, where it can't.
  const whyNot: string | undefined = (() => {
    if (source !== "compose" || composeReady) return undefined;
    if (file.trim() === "") return t`Paste a Compose file to review its plan`;
    if (fileTooBig) return t`This file is too large to read`;
    if (check.isPending) return t`Reading the file`;
    const refused = check.data?.refusal;
    // What it would refuse whatever its variables hold comes first: no one should
    // type a secret for a file it then refuses.
    if (refused && !waitsForValues(refused.message)) {
      const at = composeRefusal(refused).field;
      if (at === "env") return t`Fix the .env file to review the plan`;
      if (at === "directory") return t`Say where the file is to review the plan`;
      const line = refused.line;
      const end = refused.endLine;
      if (end > line) return t`Fix lines ${line}–${end} to review the plan`;
      return line > 0 ? t`Fix line ${line} to review the plan` : t`Fix the file to review the plan`;
    }
    if (missingVariables.length > 0) {
      const [first] = missingVariables;
      return missingVariables.length === 1
        ? t`Give ${first} a value to review the plan`
        : t`Give each variable a value to review the plan`;
    }
    if (projectMissing || projectInvalid) return t`Name it to review the plan`;
    return undefined;
  })();
  // Where what it needs is, which may be out of sight below a long file.
  const whyNotAt = (() => {
    if (!whyNot || file.trim() === "" || fileTooBig || check.isPending) return undefined;
    const refused = check.data?.refusal;
    if (refused && !waitsForValues(refused.message)) {
      const at = composeRefusal(refused).field;
      return at === "file" ? "textarea" : `[data-field="${at}"] textarea, input[data-field="${at}"]`;
    }
    const [first] = missingVariables;
    if (first) return `[data-variable="${first}"]`;
    return '[data-field="project"]';
  })();
  const goToWhyNot = () => {
    if (!whyNotAt) return;
    const target = form.current?.querySelector<HTMLElement>(whyNotAt);
    target?.scrollIntoView({ block: "center" });
    target?.focus();
  };

  // The commands that do what this form does, for whoever would rather script it.
  const command =
    source === "image" && image.image.trim() !== ""
      ? appEdits(shown, name, {
          environment: choosesEnvironment ? scope : "",
          servers: chosenServers,
          volumes: volumeNames,
        })
          .map((edit) =>
            editCommand("create", edit, {
              environment: choosesEnvironment ? scope : undefined,
              on: fleet ? chosenServers : undefined,
            }),
          )
          .join("\n")
      : "";

  const hostReach = source === "image" && image.mounts.some((m) => pathReach(m.source));
  const fileReach =
    source === "compose" &&
    (check.data?.services ?? []).some((s) => (appOf(s)?.mounts ?? []).some((m) => m.source.case === "hostPath"));

  return (
    <Sheet
      open
      over={plan ? <PlanView opening={plan} /> : undefined}
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
              {whyNot && whyNotAt ? (
                <button type="button" className="app-why-not" onClick={goToWhyNot}>
                  {whyNot}
                </button>
              ) : whyNot ? (
                whyNot
              ) : hostReach || fileReach ? (
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
          <Callout tone="critical" title={t`Can't make a plan`}>
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
        {source === "image" ? (
          <ImageFields
            form={shown}
            errors={errors}
            update={update}
            onError={setErrors}
            fieldRef={firstField}
            ports={ports}
            address={address}
            volumes={volumeNames}
            advancedOpen={advancedOpen}
            onAdvanced={setAdvancedOpen}
            command={command}
            place={
              choosesEnvironment ? (
                <SelectField
                  label={places.showsEnvironment ? t`Environment` : t`Project`}
                  value={scope}
                  options={places.environments.map((e) => ({ value: e.id, label: e.label }))}
                  onChange={(event) => setEnvironment(event.target.value)}
                />
              ) : null
            }
          />
        ) : (
          <ComposeFields
            file={file}
            onFile={(next) => {
              setFile(next);
              setErrors(({ project: _, ...rest }) => rest);
            }}
            tooBig={fileTooBig ? fileBytes : undefined}
            variables={variables}
            onVariables={setVariables}
            envFile={envFile}
            onEnvFile={setEnvFile}
            directory={directory}
            onDirectory={setDirectory}
            check={check}
            fieldRef={firstField}
            project={project}
            onProject={(next) => {
              setTypedProject(next);
              setErrors(({ project: _, ...rest }) => rest);
            }}
            projectError={
              errors["project"] ??
              (projectInvalid
                ? t`Use lowercase letters, digits and dashes, starting with a letter, such as shop.`
                : undefined)
            }
            projectKnown={projectKnown}
            projectRequired={defaultHasApps}
          />
        )}
        {source === "compose" && choosesEnvironment ? (
          <SelectField
            label={t`Environment`}
            value={scope}
            options={fileEnvironments.map((e) => ({ value: e.id, label: e.name }))}
            onChange={(event) => setEnvironment(event.target.value)}
          />
        ) : null}
        {source === "compose" && orphans.length > 0 ? (
          <Orphans names={orphans} checked={removeOrphans} onChange={setRemoveOrphans} />
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

/**
 * What a re-import can delete, on a control that can delete all of it, so it
 * says how many and lists them only when asked.
 */
function Orphans({
  names,
  checked,
  onChange,
}: {
  names: readonly string[];
  checked: boolean;
  onChange: (next: boolean) => void;
}) {
  const { t } = useLingui();
  const [shown, setShown] = useState(false);
  const count = names.length;
  return (
    <div className="app-orphans">
      <Switch
        label={t`Remove what isn't in this file`}
        description={plural(count, {
          one: "# other app is here. Otherwise it stays as it is.",
          other: "# other apps are here. Otherwise they stay as they are.",
        })}
        checked={checked}
        onChange={onChange}
      />
      <Button
        variant="ghost"
        size="sm"
        icon={shown ? "chevron-down" : "chevron-right"}
        aria-expanded={shown}
        onClick={() => setShown((was) => !was)}
      >
        {plural(count, { one: "Show it", other: "Show them" })}
      </Button>
      {shown ? (
        <ul className="app-orphan-list">
          {names.map((name) => (
            <li key={name}>
              <code>{name}</code>
            </li>
          ))}
        </ul>
      ) : null}
    </div>
  );
}

/**
 * The form with each port that follows a free one given that port, in order,
 * so no two take one, and what's shown is what's planned.
 */
function withFreePorts(form: ImageForm, ports: ServerPorts): ImageForm {
  if (!ports.ready || !form.ports.some((p) => p.auto)) return form;
  const taken = form.ports.filter((p) => !p.auto && validPort(p.host)).map((p) => Number(p.host));
  return {
    ...form,
    ports: form.ports.map((p) => {
      if (!p.auto) return p;
      const listens = validPort(p.container) ? Number(p.container) : undefined;
      const free = ports.free(listens, p.protocol === "udp", taken);
      taken.push(free);
      return { ...p, host: String(free) };
    }),
  };
}

/** The image, its name, and what else it takes once you ask for it. */
function ImageFields({
  form,
  errors,
  update,
  onError,
  fieldRef,
  ports,
  address,
  volumes,
  advancedOpen,
  onAdvanced,
  command,
  place,
}: {
  fieldRef: Ref<HTMLInputElement & HTMLTextAreaElement>;
  form: ImageForm;
  errors: FormErrors;
  update: (next: Partial<ImageForm>, clear?: string) => void;
  onError: (errors: FormErrors) => void;
  ports: ServerPorts;
  /** The address of the server it runs on, to say where a port answers. */
  address: string | undefined;
  /** The volumes there are already, to offer. */
  volumes: readonly string[];
  advancedOpen: boolean;
  onAdvanced: (open: boolean) => void;
  /** The CLI's commands that do what the form does. */
  command: string;
  /** Where it goes, where there's a choice. */
  place: React.ReactNode;
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
        value={form.image}
        error={errors["image"]}
        placeholder="nginx:1.27"
        hint={t`Any image your server can pull, such as nginx:1.27.`}
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
      {place}
      <div className="app-rule" />
      <PortsSection form={form} errors={errors} update={update} clear={clear} ports={ports} address={address} />
      <EnvSection form={form} errors={errors} update={update} clear={clear} />
      <MountsSection form={form} errors={errors} update={update} clear={clear} volumes={volumes} />
      <MoreSection
        form={form}
        errors={errors}
        update={update}
        clear={clear}
        open={advancedOpen}
        onOpen={onAdvanced}
        command={command}
      />
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
  more,
  children,
}: {
  title: string;
  id: string;
  add: string;
  onAdd: () => void;
  /** Another way to add, beside the button, such as from a file. */
  more?: React.ReactNode;
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
      <div className="app-section-actions">
        {more}
        <Button variant="ghost" size="sm" icon="plus" onClick={onAdd} className="app-section-add">
          {add}
        </Button>
      </div>
    </div>
  );
}

/**
 * Each port: what the app listens on first, then the server's port, which
 * starts at a free one, says beside it whether a typed one is free, and says
 * under the row what holds it if not, and where it will answer if so.
 */
function PortsSection({
  form,
  errors,
  update,
  clear,
  ports,
  address,
}: SectionProps & { ports: ServerPorts; address: string | undefined }) {
  const { t } = useLingui();
  const strings = useUIStrings();
  const id = useId();
  const add = () =>
    update({ ports: [...form.ports, { key: nextKey(), host: "", container: "", protocol: "tcp", auto: true }] });
  const change = (row: PortRow, next: Partial<PortRow>) =>
    update({ ports: form.ports.map((p) => (p.key === row.key ? { ...p, ...next } : p)) });
  const holderName = (holder: PortHolder) => (holder.kind === "proxy" ? t`Dockwell's proxy` : holder.name);
  return (
    <fieldset className="app-section" aria-labelledby={id}>
      <SectionHead title={t`Ports`} id={id} add={t`Add port`} onAdd={add}>
        {form.ports.length === 0 ? t`Nothing outside the server reaches it until you publish a port.` : null}
      </SectionHead>
      {form.ports.map((row, i) => {
        const udp = row.protocol === "udp";
        // The rows above and below hold their ports too.
        const others = form.ports.filter((p) => p.key !== row.key && validPort(p.host)).map((p) => Number(p.host));
        const check = ports.check(row.host, udp, holderName, others);
        const listens = validPort(row.container) ? Number(row.container) : undefined;
        const offer = check.state === "taken" ? ports.free(listens, udp, [...others, Number(row.host)]) : undefined;
        const refused = errors[`ports.${row.key}.container`] ?? errors[`ports.${row.key}.host`];
        const at =
          address && validPort(row.host) && check.state !== "taken" && !udp && listens !== undefined
            ? `${address.includes(":") ? `[${address}]` : address}:${Number(row.host)}`
            : undefined;
        return (
          <div key={row.key} className="app-row-group">
            <div className={cx("app-row app-port", i === 0 && "is-first")}>
              <TextField
                {...rowLabel(i, t`In the app`)}
                mono
                inputMode="numeric"
                placeholder="80"
                autoFocus={row.container === ""}
                value={row.container}
                invalid={Boolean(errors[`ports.${row.key}.container`])}
                aria-describedby={`${id}-${row.key}`}
                onChange={(event) => {
                  change(row, { container: event.target.value });
                  clear(`ports.${row.key}.container`);
                }}
              />
              <Icon name="arrow-right" size={14} className="app-port-arrow" />
              <PortField
                {...rowLabel(i, t`On the server`)}
                inRow
                value={row.host}
                check={check}
                invalid={Boolean(errors[`ports.${row.key}.host`])}
                aria-describedby={`${id}-${row.key}`}
                onChange={(next) => {
                  change(row, { host: next, auto: false });
                  clear(`ports.${row.key}.host`);
                }}
              />
              <SelectField
                {...rowLabel(i, t`Protocol`)}
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
            {refused ? (
              <RowError id={`${id}-${row.key}`} message={refused} />
            ) : check.state === "taken" ? (
              <div id={`${id}-${row.key}`} className="dw-field-error app-row-error">
                <Glyph shape="square" size={9} />
                <span>
                  {check.by ? strings.portField.takenBy(check.by) : strings.portField.taken}
                  {offer !== undefined ? (
                    <>
                      {" · "}
                      <button
                        type="button"
                        className="dw-port-use"
                        onClick={() => change(row, { host: String(offer), auto: false })}
                      >
                        {strings.portField.use(offer)}
                      </button>
                    </>
                  ) : null}
                </span>
              </div>
            ) : at ? (
              <p className="app-port-at app-row-note">
                <Icon name="globe" size={14} />
                <Trans>
                  Reachable at <code className="app-port-at-where">{at}</code>
                </Trans>
              </p>
            ) : null}
          </div>
        );
      })}
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
  // An .env file adds its variables to those here, and takes the place of one of the same name.
  const file = useRef<HTMLInputElement>(null);
  const importFile = async (chosen: File | undefined) => {
    const parsed = chosen ? parseEnv(await chosen.text()) : undefined;
    if (!parsed) return;
    const named = new Map(parsed.map((e) => [e.name, e.value]));
    const kept = form.env.filter((e) => !named.has(e.name.trim()) && (e.name.trim() !== "" || e.value !== ""));
    update({ env: [...kept, ...parsed.map((e) => ({ key: nextKey(), ...e }))] });
  };
  return (
    <fieldset className="app-section" aria-labelledby={id}>
      <SectionHead
        title={t`Environment variables`}
        id={id}
        add={t`Add variable`}
        onAdd={add}
        more={
          <>
            <input
              ref={file}
              type="file"
              className="dw-sr"
              tabIndex={-1}
              aria-hidden="true"
              onChange={(event) => {
                void importFile(event.target.files?.[0]);
                event.target.value = "";
              }}
            />
            <Button variant="ghost" size="sm" icon="upload" onClick={() => file.current?.click()}>
              <Trans>Import .env</Trans>
            </Button>
          </>
        }
      >
        {t`Plans say a value changed, and never show it.`}
      </SectionHead>
      {form.env.map((row, i) => (
        <div key={row.key} className="app-row-group">
          <div className={cx("app-row app-env", i === 0 && "is-first")}>
            <TextField
              {...rowLabel(i, t`Name`)}
              mono
              autoFocus={row.name === ""}
              value={row.name}
              invalid={Boolean(errors[`env.${row.key}.name`])}
              aria-describedby={`${id}-${row.key}`}
              onPaste={(event) => paste(row, event)}
              onChange={(event) => {
                change(row, { name: event.target.value });
                clear(`env.${row.key}.name`);
              }}
            />
            <ValueField
              {...rowLabel(i, t`Value`)}
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

function MountsSection({ form, errors, update, clear, volumes }: SectionProps & { volumes: readonly string[] }) {
  const { t } = useLingui();
  const id = useId();
  const listId = `${id}-volumes`;
  const add = () => update({ mounts: [...form.mounts, { key: nextKey(), source: "", target: "", readOnly: false }] });
  const change = (row: MountRow, next: Partial<MountRow>) =>
    update({ mounts: form.mounts.map((m) => (m.key === row.key ? { ...m, ...next } : m)) });
  return (
    <fieldset className="app-section" aria-labelledby={id}>
      <SectionHead title={t`Volumes`} id={id} add={t`Add volume`} onAdd={add}>
        {form.mounts.length === 0
          ? t`Without a volume, what it writes is lost when its container is replaced.`
          : t`A volume such as data at /data, or a server path.`}
      </SectionHead>
      {form.mounts.map((row, i) => {
        const refused = errors[`mounts.${row.key}.source`] ?? errors[`mounts.${row.key}.target`];
        // Once it is refused, what it would have reached is beside the point.
        const reach = refused ? undefined : pathReach(row.source);
        const source = row.source.trim();
        return (
          <div key={row.key} className="app-row-group">
            <div className={cx("app-row app-mount-row", i === 0 && "is-first")}>
              <TextField
                {...rowLabel(i, t`Volume or server path`)}
                mono
                list={volumes.length > 0 ? listId : undefined}
                autoComplete="off"
                autoFocus={row.source === ""}
                value={row.source}
                invalid={Boolean(errors[`mounts.${row.key}.source`])}
                aria-describedby={`${id}-${row.key}`}
                onChange={(event) => {
                  change(row, { source: event.target.value });
                  clear(`mounts.${row.key}.source`);
                }}
              />
              <TextField
                {...rowLabel(i, t`Path in the app`)}
                mono
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
                    <code>{source}</code> gives this app control of Docker on the server, the same as root. You approve
                    this in the plan; API tokens can't.
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
      {volumes.length > 0 ? (
        <datalist id={listId}>
          {volumes.map((v) => (
            <option key={v} value={v} />
          ))}
        </datalist>
      ) : null}
    </fieldset>
  );
}

/** When Docker restarts it, as the select offers it: its default first, which leaves restart unset. */
const RESTARTS = [RestartPolicy.UNSPECIFIED, RestartPolicy.ALWAYS, RestartPolicy.ON_FAILURE, RestartPolicy.NO];

/**
 * What the app starts at a good default, under Advanced, which says those
 * defaults in a line while it's closed, and how many were changed. The CLI
 * command that does what the whole form does is at its end.
 */
function MoreSection({
  form,
  errors,
  update,
  clear,
  open,
  onOpen,
  command,
}: SectionProps & { open: boolean; onOpen: (open: boolean) => void; command: string }) {
  const { t } = useLingui();
  const words = useAppWords();
  const more = form.more ?? DEFAULT_SETTINGS;
  const set = (next: Partial<MoreSettings>, key: keyof MoreSettings) => {
    update({ more: { ...more, ...next } });
    clear(`more.${key}`);
  };
  const port = checkPort(form);
  const changed = changedSettings(more);
  const restartWords: Record<RestartPolicy, string> = {
    [RestartPolicy.UNSPECIFIED]: t`Restarts unless stopped`,
    [RestartPolicy.UNLESS_STOPPED]: t`Restarts unless stopped`,
    [RestartPolicy.ALWAYS]: t`Always restarts`,
    [RestartPolicy.ON_FAILURE]: t`Restarts on errors`,
    [RestartPolicy.NO]: t`Never restarts`,
  };
  const cmd = splitWords(more.command.trim()) ?? [];
  const memory = more.memory.trim();
  const cpus = more.cpus.trim();
  const path = more.healthPath.trim();
  const summary = [
    cmd.length > 0 ? joinWords(cmd) : t`The image's command`,
    restartWords[more.restart],
    memory && cpus
      ? t`Up to ${memory} and ${cpus} CPUs`
      : memory
        ? t`Up to ${memory}`
        : cpus
          ? t`Up to ${cpus} CPUs`
          : t`No limits`,
    path && port ? t`Checks ${path}` : t`No health check`,
  ].join(" · ");
  return (
    <Advanced
      open={open}
      onOpenChange={onOpen}
      summary={summary}
      badge={changed > 0 ? <Tag words>{plural(changed, { one: "# changed", other: "# changed" })}</Tag> : undefined}
    >
      <div className="app-more">
        <TextField
          label={t`Command`}
          mono
          value={more.command}
          placeholder={t`The image's own`}
          error={errors["more.command"]}
          spellCheck={false}
          autoComplete="off"
          onChange={(event) => set({ command: event.target.value }, "command")}
        />
        <TextField
          label={t`User`}
          mono
          value={more.user}
          placeholder={t`The image's own`}
          spellCheck={false}
          autoComplete="off"
          onChange={(event) => set({ user: event.target.value }, "user")}
        />
        <SelectField
          label={t`Restarts`}
          value={String(more.restart)}
          options={RESTARTS.map((p) => ({ value: String(p), label: words.restart(p) }))}
          onChange={(event) => set({ restart: Number(event.target.value) as RestartPolicy }, "restart")}
        />
        <TextField
          label={t`Memory limit`}
          mono
          value={more.memory}
          placeholder={t`No limit`}
          error={errors["more.memory"]}
          autoComplete="off"
          onChange={(event) => set({ memory: event.target.value }, "memory")}
        />
        <TextField
          label={t`CPU limit`}
          mono
          inputMode="decimal"
          value={more.cpus}
          placeholder={t`No limit`}
          error={errors["more.cpus"]}
          autoComplete="off"
          onChange={(event) => set({ cpus: event.target.value }, "cpus")}
        />
        <TextField
          label={t`Health check path`}
          mono
          value={more.healthPath}
          placeholder="/healthz"
          disabled={port === undefined}
          error={errors["more.healthPath"]}
          hint={port === undefined ? t`Add a port first.` : t`Asked over HTTP on port ${port}.`}
          autoComplete="off"
          onChange={(event) => set({ healthPath: event.target.value }, "healthPath")}
        />
      </div>
      {command ? <CopyAsCommand command={command} /> : null}
    </Advanced>
  );
}

/**
 * A field's name in a row of fields: shown over the first row, and read out on
 * every row, so "Name / Value" doesn't repeat down the form.
 */
function rowLabel(index: number, label: string): { label: string } | { "aria-label": string } {
  return index === 0 ? { label } : { "aria-label": label };
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
function useComposeCheck(file: string, variables: Readonly<Record<string, string>>, env: string, directory: string) {
  const [settled, setSettled] = useState({ file, variables, env, directory });
  useEffect(() => {
    const timer = setTimeout(() => setSettled({ file, variables, env, directory }), CHECK_AFTER_MS);
    return () => clearTimeout(timer);
  }, [file, variables, env, directory]);
  const query = useQuery(
    ComposeService.method.checkCompose,
    {
      compose: settled.file,
      variables: settled.variables,
      env: settled.env,
      directory: settled.directory.trim(),
    },
    {
      enabled: settled.file.trim() !== "",
      placeholderData: (previous) => previous,
      staleTime: 60_000,
      // A query's key is what it asked, and here that holds an .env's values, so nothing keeps it once it's done with.
      gcTime: 0,
    },
  );
  // Until the file settles, what is shown is about an older one, so Review plan waits.
  const waiting =
    file !== settled.file ||
    variables !== settled.variables ||
    env !== settled.env ||
    directory !== settled.directory ||
    query.isFetching;
  return { data: file.trim() === "" ? undefined : query.data, error: query.error, isPending: waiting };
}

function ComposeFields({
  file,
  onFile,
  tooBig,
  variables,
  onVariables,
  envFile,
  onEnvFile,
  directory,
  onDirectory,
  check,
  fieldRef,
  project,
  onProject,
  projectError,
  projectKnown,
  projectRequired,
}: {
  fieldRef: Ref<HTMLInputElement & HTMLTextAreaElement>;
  file: string;
  onFile: (file: string) => void;
  /** How large the file is, when it's too large for the API to read. */
  tooBig: number | undefined;
  variables: Readonly<Record<string, string>>;
  onVariables: (variables: Readonly<Record<string, string>>) => void;
  envFile: string;
  onEnvFile: (text: string) => void;
  directory: string;
  onDirectory: (path: string) => void;
  check: ReturnType<typeof useComposeCheck>;
  project: string;
  onProject: (name: string) => void;
  projectError: string | undefined;
  projectKnown: boolean;
  projectRequired: boolean;
}) {
  const { t } = useLingui();
  const input = useRef<HTMLInputElement>(null);
  const [showOptional, setShowOptional] = useState(false);
  const refusal = check.data?.refusal;
  const allVars = check.data?.variables ?? [];
  const required = allVars.filter((v) => v.required);
  const optional = allVars.filter((v) => !v.required);
  const exposed = check.data?.exposedVariables ?? [];
  // A refusal of a variable that has no value yet is what the fields below already ask for.
  const refused: RefusedAt | undefined =
    refusal && !waitsForValues(refusal.message) ? composeRefusal(refusal) : undefined;
  const readFile = async (chosen: File | undefined) => {
    if (chosen) onFile(await chosen.text());
  };
  const onFileField = refused?.field === "file" ? refused : undefined;
  const onEnvField = refused?.field === "env" ? refused : undefined;
  const onDirectoryField = refused?.field === "directory" ? refused : undefined;
  const kib = tooBig === undefined ? 0 : Math.ceil(tooBig / 1024);
  const limit = MAX_FILE >> 10;
  const fileError =
    tooBig !== undefined
      ? t`This file is ${kib} KiB, and Dockwell reads files of up to ${limit} KiB. Leave out what Dockwell doesn't run, or import the services in more than one file.`
      : onFileField
        ? onFileField.line > 0
          ? onFileField.to > onFileField.line
            ? t`Near lines ${onFileField.line}–${onFileField.to}: ${onFileField.text}`
            : t`Line ${onFileField.line}: ${onFileField.text}`
          : onFileField.text
        : undefined;
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
          lines={FILE_LINES}
          value={file}
          onChange={(event) => onFile(event.target.value)}
          placeholder={'services:\n  web:\n    image: nginx:1.27\n    ports:\n      - "8080:80"'}
          error={fileError ? withCode(fileError) : undefined}
          markedLine={refused && refused.field !== "env" ? refused.line : undefined}
          markedTo={refused && refused.field !== "env" ? refused.to : undefined}
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
      <TextField
        data-field="project"
        label={t`Project name`}
        mono
        optional={!projectRequired && project === ""}
        value={project}
        error={projectError}
        hint={
          projectKnown ? (
            <Trans>
              <code>{project}</code> is here already, so the plan updates what's in it.
            </Trans>
          ) : projectRequired ? (
            <Trans>
              Other apps are here, so it needs a name of its own, such as <code>shop</code>. Its apps show as one row,
              on a network of their own.
            </Trans>
          ) : (
            <Trans>
              Its apps show as one row with this name, on a network of their own. Importing under this name again
              updates them.
            </Trans>
          )
        }
        onChange={(event) => onProject(event.target.value)}
      />
      <BesideTheFile
        envFile={envFile}
        onEnvFile={onEnvFile}
        directory={directory}
        onDirectory={onDirectory}
        envRefused={onEnvField}
        directoryRefused={onDirectoryField}
      />
      {required.length > 0 || optional.length > 0 ? (
        <fieldset className="app-section" aria-label={t`Variables`}>
          <div className="app-section-head">
            <div className="app-section-text">
              <span className="dw-field-label">
                <Trans>Variables</Trans>
              </span>
              {showOptional && optional.length > 0 ? (
                <span className="dw-field-hint">
                  <Trans>The file uses these. Leave one with a default empty to use its default.</Trans>
                </span>
              ) : required.length > 0 ? (
                <span className="dw-field-hint">
                  <Trans>The file uses these, and has no default for them.</Trans>
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
              <ValueField
                key={v.name}
                data-variable={v.name}
                label={
                  <span className="app-var-label">
                    <code className="app-var-name">{v.name}</code>
                    {v.required ? (
                      <span className="app-required">
                        <Trans>Required</Trans>
                      </span>
                    ) : null}
                  </span>
                }
                optional={!v.required}
                value={variables[v.name] ?? ""}
                onChange={(event) => onVariables({ ...variables, [v.name]: event.target.value })}
              />
            ))}
          </div>
        </fieldset>
      ) : null}
      {groupExposed(exposed).map((group) => (
        <ExposedNote key={`${group.kind}/${group.owner}/${group.field}`} group={group} />
      ))}
      {check.data?.moreExposed ? (
        <p className="app-reach-note">
          <Icon name="shield" size={14} />
          <span>
            <Trans>
              This file writes more variables in plain sight than these. Read it before you pass one a secret.
            </Trans>
          </span>
        </p>
      ) : null}
      <ComposePreview check={check.data} />
    </>
  );
}

/**
 * What a real file reads beside it: the .env it takes values from, and the directory
 * what it says of paths starts from. They wait behind a button each, as most files need
 * neither, and come out of hiding when a refusal is about one of them.
 */
function BesideTheFile({
  envFile,
  onEnvFile,
  directory,
  onDirectory,
  envRefused,
  directoryRefused,
}: {
  envFile: string;
  onEnvFile: (text: string) => void;
  directory: string;
  onDirectory: (path: string) => void;
  envRefused: RefusedAt | undefined;
  directoryRefused: RefusedAt | undefined;
}) {
  const { t } = useLingui();
  const [envOpen, setEnvOpen] = useState(false);
  const [directoryOpen, setDirectoryOpen] = useState(false);
  const [shown, setShown] = useState(false);
  const input = useRef<HTMLInputElement>(null);
  const envShows = envOpen || envFile !== "" || envRefused !== undefined;
  const directoryShows = directoryOpen || directory !== "" || directoryRefused !== undefined;
  const read = async (chosen: File | undefined) => {
    if (chosen) onEnvFile(await chosen.text());
  };
  return (
    <>
      {envShows && directoryShows ? null : (
        <div className="app-beside">
          {envShows ? null : (
            <Button variant="ghost" size="sm" icon="plus" onClick={() => setEnvOpen(true)}>
              <Trans>Add an .env file</Trans>
            </Button>
          )}
          {directoryShows ? null : (
            <Button variant="ghost" size="sm" icon="plus" onClick={() => setDirectoryOpen(true)}>
              <Trans>Say which directory it's in</Trans>
            </Button>
          )}
        </div>
      )}
      {envShows ? (
        <div className="app-field" data-field="env">
          <CodeField
            className={shown ? undefined : "app-env-hidden"}
            label={t`.env file`}
            lines={ENV_LINES}
            value={envFile}
            autoComplete="off"
            onChange={(event) => onEnvFile(event.target.value)}
            placeholder={"REDIS_PASSWORD=...\nTAG=1.27"}
            error={envRefused ? withCode(envRefused.text) : undefined}
            markedLine={envRefused?.line || undefined}
            hint={t`For the variables the file uses. It's sent to read the file and plan it, never kept in this browser, and a value goes on record only where the file writes it.`}
            actions={
              <>
                <input
                  ref={input}
                  type="file"
                  className="dw-sr"
                  tabIndex={-1}
                  aria-hidden="true"
                  onChange={(event) => {
                    void read(event.target.files?.[0]);
                    event.target.value = "";
                  }}
                />
                <Button variant="ghost" size="sm" icon="upload" onClick={() => input.current?.click()}>
                  <Trans>Choose a file</Trans>
                </Button>
                <Button
                  variant="ghost"
                  size="sm"
                  icon="eye"
                  aria-pressed={shown}
                  onClick={() => setShown((was) => !was)}
                >
                  {shown ? <Trans>Hide</Trans> : <Trans>Show</Trans>}
                </Button>
              </>
            }
          />
        </div>
      ) : null}
      {directoryShows ? (
        <TextField
          data-field="directory"
          label={t`Directory on the server`}
          mono
          optional
          value={directory}
          error={directoryRefused ? withCode(directoryRefused.text) : undefined}
          hint={
            <Trans>
              Where the file is, such as <code>/opt/shop</code>. A path relative to it, such as <code>./data</code>,
              starts there, and each is a server path you approve in the plan.
            </Trans>
          }
          onChange={(event) => onDirectory(event.target.value)}
        />
      ) : null}
    </>
  );
}

/**
 * A value that may be a secret, which the screen behind someone needn't read as
 * it is typed. It stays theirs to check, with the control beside it.
 */
function ValueField(props: TextFieldProps & { "data-variable"?: string }) {
  const { t } = useLingui();
  const [shown, setShown] = useState(false);
  return (
    <TextField
      {...props}
      mono
      type={shown ? "text" : "password"}
      autoComplete="off"
      action={
        <button
          type="button"
          className="app-reveal"
          aria-pressed={shown}
          aria-label={t`Show the value`}
          title={shown ? t`Hide the value` : t`Show the value`}
          onClick={() => setShown((was) => !was)}
        >
          <Icon name="eye" size={16} />
        </button>
      }
    />
  );
}

/** The variables one field of one service, volume or network holds. */
interface ExposedGroup {
  kind: string;
  owner: string;
  field: string;
  names: string[];
}

function groupExposed(exposed: readonly ExposedVariable[]): ExposedGroup[] {
  const groups = new Map<string, ExposedGroup>();
  for (const e of exposed) {
    const kind = e.resource?.kind ?? "";
    const owner = e.resource?.name ?? "";
    const key = `${kind}/${owner}/${e.field}`;
    const group = groups.get(key) ?? { kind, owner, field: e.field, names: [] };
    group.names.push(e.name);
    groups.set(key, group);
  }
  return [...groups.values()];
}

/**
 * Where a file writes a variable's value in plain sight, before anyone types a
 * secret into the field above. What to do about it depends on the field: what
 * runs keeps a value by name, as long as it is one word.
 */
function ExposedNote({ group }: { group: ExposedGroup }) {
  const { t } = useLingui();
  const { owner, field } = group;
  const fields: Readonly<Record<string, string>> = {
    command: t`command`,
    entrypoint: t`entrypoint`,
    labels: t`labels`,
    driver_opts: t`driver options`,
    environment: t`environment variable names`,
  };
  const where = fields[field] ?? field;
  const holder =
    group.kind === "volume" ? t`the volume ${owner}` : group.kind === "network" ? t`the network ${owner}` : owner;
  const names = group.names.map((name, i) => (
    <Fragment key={name}>
      {i > 0 ? ", " : null}
      <code>{name}</code>
    </Fragment>
  ));
  const fix = () => {
    switch (field) {
      case "command":
      case "entrypoint":
        return (
          <Trans>
            Its value has a space, a quote or another character Compose reads as part of the line, so one name can't
            stand for it. To keep it hidden, write the {where} as a list, with each word its own variable.
          </Trans>
        );
      case "environment":
        return <Trans>Only a variable's value is hidden, so don't pass a secret there.</Trans>;
      default:
        return <Trans>It can't be hidden there, so don't pass a secret there.</Trans>;
    }
  };
  return (
    <p className="app-reach-note">
      <Icon name="shield" size={14} />
      <span>
        <Trans>
          Anyone who can read {holder}'s {where} can see the value of {names}.
        </Trans>{" "}
        {fix()}
      </span>
    </p>
  );
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
        {plural(services.length, { one: "This makes # app", other: "This makes # apps" })}
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
                    <Tag icon="key" words>
                      {plural(secrets, { one: "# variable", other: "# variables" })}
                    </Tag>
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
export function RunsOnField({
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
            {server.connection === Connection.CONNECTED ? null : <Tag words>{t`Offline`}</Tag>}
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
