// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { type EditSchema, ImageBuild_Builder, type ImageConfig, Protocol, RestartPolicy } from "@dockwell/api";

import { toLabel } from "./app";
import { splitWords } from "./specEdit";

/** One port someone is publishing. */
export interface PortRow {
  key: number;
  host: string;
  container: string;
  protocol: "tcp" | "udp";
  /** The server's port follows a free one, until someone types their own. */
  auto?: boolean;
  /**
   * What a following port showed when someone went into its field: it holds still
   * there while the field has focus, as a free port landing under the cursor would
   * run into what they type.
   */
  held?: string;
  /** The image's EXPOSE filled it, and nobody has changed it since. */
  fromImage?: boolean;
}

/** One environment variable someone is setting. */
export interface EnvRow {
  key: number;
  name: string;
  value: string;
  /** The image's ENV filled it, and nobody has changed it since. */
  fromImage?: boolean;
  /** The value the image gives it: left as it is, the app takes it from the image, and the spec needn't say it. */
  imageDefault?: string;
}

/** One volume or server path someone is mounting. */
export interface MountRow {
  key: number;
  source: string;
  target: string;
  readOnly: boolean;
  /** The image's VOLUME filled it, and nobody has changed it since. */
  fromImage?: boolean;
}

/** What New app starts at a default, under Advanced: each empty takes the image's own, or none. */
export interface MoreSettings {
  /** The command, as a shell line. */
  command: string;
  user: string;
  restart: RestartPolicy;
  /** The most memory it may use, such as 512M. */
  memory: string;
  /** The most CPUs it may use, such as 0.5. */
  cpus: string;
  /** A path to ask over HTTP on its first port, such as /healthz, which makes it a health check. */
  healthPath: string;
}

export const DEFAULT_SETTINGS: MoreSettings = {
  command: "",
  user: "",
  restart: RestartPolicy.UNSPECIFIED,
  memory: "",
  cpus: "",
  healthPath: "",
};

/** New app's form, for an image. */
export interface ImageForm {
  image: string;
  /** What they typed. Empty takes the image's name. */
  name: string;
  ports: PortRow[];
  env: EnvRow[];
  mounts: MountRow[];
  /** What Advanced holds; leaving it out leaves each at its default. */
  more?: MoreSettings;
  /** The registry login its pull signs in with, by name, where its project may use several (ADR 0086). Empty takes the one there is. */
  registryLogin?: string;
}

/** How many of Advanced's settings differ from their defaults. */
export function changedSettings(more: MoreSettings | undefined): number {
  if (!more) return 0;
  return (Object.keys(DEFAULT_SETTINGS) as (keyof MoreSettings)[]).filter((k) => {
    const v = more[k];
    return typeof v === "string" ? v.trim() !== DEFAULT_SETTINGS[k] : v !== DEFAULT_SETTINGS[k];
  }).length;
}

/** The port in the app a health check over HTTP asks on: the first one the form publishes. */
export function checkPort(form: ImageForm): number | undefined {
  const row = form.ports.find((p) => validPort(p.container) && p.protocol === "tcp");
  return row ? Number(row.container) : undefined;
}

/** What New app asks of an app that builds from Git, beside what any app has (ADR 0046). */
export interface GitForm {
  /** The repository's URL, as typed, such as github.com/acme/web. */
  url: string;
  /** The branch or tag. Empty builds the default branch. */
  ref: string;
  /** The folder the build starts from. Empty is the repository's top. */
  context: string;
  /** The Dockerfile, as a path in the folder. Empty is Dockerfile. */
  dockerfile: string;
  /** Build arguments, a NAME=value a line. */
  args: string;
  /** What builds it: "" takes the Dockerfile when there is one, and Railpack when there isn't. */
  builder: "" | "dockerfile" | "railpack";
  /** With Railpack, the commands that build and start it, in place of what it finds. */
  buildCommand: string;
  startCommand: string;
  /** The registry logins its Dockerfile's images pull with, by name: none unless picked (ADR 0086). */
  registryLogins?: string[];
}

/** A Git form with nothing in it yet. */
export const emptyGit: GitForm = {
  url: "",
  ref: "",
  context: "",
  dockerfile: "",
  args: "",
  builder: "",
  buildCommand: "",
  startCommand: "",
};

/** The app a repository makes, by default: its last part, without .git, as a name, such as web. */
export function nameFromRepository(url: string): string {
  const path = url
    .trim()
    .replace(/[?#].*$/, "")
    .replace(/\/+$/, "");
  const last = path.split("/").pop() ?? "";
  return toLabel(last.replace(/\.git$/, ""));
}

/** A build's settings as the spec has them. */
export function buildOf(git: GitForm) {
  const args: Record<string, string> = {};
  for (const { name, value } of parseEnv(git.args) ?? []) args[name] = value;
  const builder =
    git.builder === "dockerfile"
      ? ImageBuild_Builder.DOCKERFILE
      : git.builder === "railpack"
        ? ImageBuild_Builder.RAILPACK
        : ImageBuild_Builder.UNSPECIFIED;
  return {
    source: { case: "git" as const, value: { url: git.url.trim(), ref: git.ref.trim() } },
    context: git.context.trim(),
    // Railpack takes no Dockerfile, and a Dockerfile no commands.
    dockerfile: git.builder === "railpack" ? "" : git.dockerfile.trim(),
    args,
    builder,
    buildCommand: git.builder === "dockerfile" ? "" : git.buildCommand.trim(),
    startCommand: git.builder === "dockerfile" ? "" : git.startCommand.trim(),
    // Railpack's images are Railpack's: a login goes with a Dockerfile's alone.
    registryLogins: git.builder === "railpack" ? [] : (git.registryLogins ?? []),
  };
}

/** The field of the Git form a refusal is about, by what it names, or "" for none of them. */
export function gitFieldOf(message: string): string {
  const text = message.toLowerCase();
  // A build's secrets are the environment's, which New app doesn't set.
  if (/build's secret|as a secret/.test(text)) return "";
  if (/start command/.test(text)) return "git.startCommand";
  if (/build command/.test(text)) return "git.buildCommand";
  if (/\bdockerfile\b/.test(text)) return "git.dockerfile";
  if (/\bcontext\b|\bno folder\b/.test(text)) return "git.context";
  if (/\bbranch\b|\btag\b/.test(text)) return "git.ref";
  if (/build argument/.test(text)) return "git.args";
  if (/repositor|url|fetch|reach|credentials|https/.test(text)) return "git.url";
  return "";
}

/** New app's form, for an app from a folder on the server (ADR 0058): what its runtime says. */
export interface FolderForm {
  /** The language as a spec names it, such as "node". */
  language: string;
  /** Its version, such as "24". Empty takes the newest. */
  version: string;
  folder: string;
  /** Empty takes the language's. */
  install: string;
  start: string;
  port: string;
  /** Who it runs as, "uid:gid". Empty is filled from the folder's owner, or the server's first user. */
  user: string;
}

/** The form's fields, by a path a refusal can point at, such as "ports.3.host", with the row's key. */
export type FormErrors = Readonly<Record<string, string>>;

/** What a person sees a source as: a path on the server, or else a volume's name. */
export function isServerPath(source: string): boolean {
  return source.trim().startsWith("/");
}

/**
 * What a server path reaches, as ADR 0021 has it, to say at the field. The
 * API decides for itself, from the spec: this only warns before the plan.
 */
export function pathReach(source: string): "socket" | "path" | undefined {
  const path = source.trim().replace(/\/+$/, "");
  if (!isServerPath(source)) return undefined;
  if (/(^|\/)(docker|containerd)\.sock$/.test(path) || path === "/run" || path === "/var/run") return "socket";
  return "path";
}

/** The edits that make an app from the form: the volumes it needs that don't exist yet, then the service. */
export function appEdits(
  form: ImageForm,
  name: string,
  scope: { environment: string; servers: readonly string[]; volumes: readonly string[] },
  folder?: FolderForm,
  git?: GitForm,
): MessageInitShape<typeof EditSchema>[] {
  const servers = [...scope.servers];
  const edits: MessageInitShape<typeof EditSchema>[] = [];
  const made = new Set(scope.volumes);
  for (const row of form.mounts) {
    const source = row.source.trim();
    if (source === "" || isServerPath(source) || made.has(source)) continue;
    made.add(source);
    edits.push({
      resource: { kind: "volume", scope: scope.environment, name: source },
      spec: { kind: { case: "volume", value: {} } },
      servers,
    });
  }
  const env: Record<string, string> = {};
  for (const row of form.env) {
    // A value the image gives it already needn't be written again.
    if (row.imageDefault !== undefined && row.value === row.imageDefault) continue;
    if (row.name.trim() !== "") env[row.name.trim()] = row.value;
  }
  edits.push({
    resource: { kind: "service", scope: scope.environment, name },
    spec: {
      kind: {
        case: "service",
        value: {
          // An app from a folder: the server writes its image, command and folder from this.
          ...(folder
            ? {
                runtime: {
                  language: folder.language,
                  version: folder.version,
                  folder: folder.folder.trim(),
                  install: folder.install.trim(),
                  start: folder.start.trim(),
                  port: folder.port.trim() === "" ? 0 : Number(folder.port.trim()),
                  user: folder.user.trim(),
                },
              }
            : {}),
          runs: {
            case: "app",
            value: {
              // An app that builds runs what its build makes, which the plan names, and one
              // from a folder runs its runtime's image.
              image: git || folder ? "" : form.image.trim(),
              registryLogin: git || folder ? "" : (form.registryLogin ?? ""),
              env,
              ports: form.ports
                .filter((p) => p.host.trim() !== "" || p.container.trim() !== "")
                .map((p) => ({
                  hostPort: Number(p.host),
                  containerPort: Number(p.container),
                  protocol: p.protocol === "udp" ? Protocol.UDP : Protocol.TCP,
                })),
              mounts: form.mounts
                .filter((m) => m.source.trim() !== "" || m.target.trim() !== "")
                .map((m) => ({
                  source: isServerPath(m.source)
                    ? { case: "hostPath" as const, value: m.source.trim() }
                    : { case: "volume" as const, value: m.source.trim() },
                  target: m.target.trim(),
                  readOnly: m.readOnly,
                })),
              ...moreFields(form),
            },
          },
          ...(git ? { build: buildOf(git) } : {}),
        },
      },
    },
    servers,
  });
  return edits;
}

/** What Advanced sets in the app's spec: only what someone changed from its default. */
function moreFields(form: ImageForm) {
  const more = form.more;
  if (!more) return {};
  const command = splitWords(more.command.trim()) ?? [];
  const port = checkPort(form);
  const path = more.healthPath.trim();
  return {
    ...(command.length > 0 ? { command } : {}),
    ...(more.user.trim() ? { user: more.user.trim() } : {}),
    ...(more.restart !== RestartPolicy.UNSPECIFIED ? { restart: more.restart } : {}),
    ...(more.memory.trim() || more.cpus.trim()
      ? { resources: { memory: more.memory.trim(), cpus: more.cpus.trim() } }
      : {}),
    ...(path && port ? { healthcheck: { http: { port, path } } } : {}),
  };
}

/** What an image's ENV sets for itself, which nobody running it sets: its search path, locale and versions. */
const IMAGE_NOISE =
  /^(PATH|HOME|HOSTNAME|LANG|LANGUAGE|LC_[A-Z]+|TERM|SHLVL|GPG_KEYS?|.*_(VERSION|MAJOR|SHA\d*|SHA256|CHECKSUM|DOWNLOAD_URL|DOWNLOAD_SHA\d*))$/;

/** Whether an image's variable is one a person may want to change, rather than one it sets for itself. */
export function settable(name: string): boolean {
  return !IMAGE_NOISE.test(name);
}

/**
 * The form filled from what an image declares, where nobody has changed it:
 * the port its first EXPOSE names, a volume for each path its VOLUME keeps, and
 * the variables it sets with their values. What the image filled before goes,
 * and what someone added or changed stays.
 */
export function fillFromImage(
  form: ImageForm,
  config: ImageConfig | undefined,
  app: string,
  key: () => number,
): ImageForm {
  if (!config) return form;
  const ownPorts = form.ports.filter((p) => !p.fromImage);
  // A database's or a broker's port isn't published by default: other apps reach it by name.
  const tcp = config.ports.find((p) => !p.udp && !KEPT_IN.has(p.port));
  const ports: PortRow[] =
    ownPorts.length > 0 || !tcp
      ? ownPorts
      : [{ key: key(), host: "", container: String(tcp.port), protocol: "tcp", auto: true, fromImage: true }];
  const ownEnv = form.env.filter((e) => !e.fromImage);
  const env: EnvRow[] = [
    ...ownEnv,
    ...config.env
      .filter((v) => settable(v.name) && !ownEnv.some((e) => e.name.trim() === v.name))
      .map((v) => ({ key: key(), name: v.name, value: v.value, fromImage: true, imageDefault: v.value })),
  ];
  const ownMounts = form.mounts.filter((m) => !m.fromImage);
  const prefix = app || "app";
  const mounts: MountRow[] = [
    ...ownMounts,
    ...config.volumes
      .filter((path) => !ownMounts.some((m) => m.target.trim() === path))
      .map((path) => ({
        key: key(),
        source: volumeName(prefix, path),
        target: path,
        readOnly: false,
        fromImage: true,
      })),
  ];
  return { ...form, ports, env, mounts };
}

/**
 * The ports of databases, caches and brokers, which New app doesn't publish on
 * the server for an image that exposes one, as what uses them is another app on
 * its network: PostgreSQL, MySQL and MariaDB, Redis, MongoDB, SQL Server,
 * Elasticsearch, Memcached, RabbitMQ and NATS. A port also used for a web UI,
 * such as 9000 or 8086, isn't one of them.
 */
const KEPT_IN = new Set([5432, 3306, 6379, 27017, 1433, 9200, 11211, 5672, 4222]);

/** A volume's name for a path an image keeps, such as postgres-data for /var/lib/postgresql/data. */
export function volumeName(app: string, path: string): string {
  const last = path.split("/").filter(Boolean).pop() ?? "data";
  const slug =
    last
      .toLowerCase()
      .replace(/[^a-z0-9]+/g, "-")
      .replace(/^-+|-+$/g, "") || "data";
  return `${app}-${slug}`.slice(0, 63);
}

/** A name a process's environment takes. */
const ENV_NAME = /^[A-Za-z_][A-Za-z0-9_]*$/;

/**
 * Reads pasted text as environment variables, KEY=value a line, as an .env
 * file has them, skipping blanks and comments. Undefined for text that isn't
 * that, which goes in the field as it is.
 */
export function parseEnv(text: string): { name: string; value: string }[] | undefined {
  const lines = text.split(/\r?\n/).filter((line) => line.trim() !== "" && !line.trim().startsWith("#"));
  if (lines.length < 1) return undefined;
  const out: { name: string; value: string }[] = [];
  for (const line of lines) {
    const at = line.indexOf("=");
    if (at < 1) return undefined;
    const name = line
      .slice(0, at)
      .trim()
      .replace(/^export\s+/, "");
    if (!ENV_NAME.test(name)) return undefined;
    let value = line.slice(at + 1).trim();
    if (/^".*"$/.test(value) || /^'.*'$/.test(value)) value = value.slice(1, -1);
    out.push({ name, value });
  }
  return out;
}

export function validEnvName(name: string): boolean {
  return ENV_NAME.test(name.trim());
}

/** A port number, as a person types it. */
export function validPort(value: string): boolean {
  const n = Number(value.trim());
  return /^\d+$/.test(value.trim()) && n >= 1 && n <= 65_535;
}

/**
 * Which field of the form a refusal is about, from what it names, for a
 * message the API wrote for the whole change. "" when it is about none of
 * them, so it shows above them.
 */
export function fieldOf(message: string, form: ImageForm, name: string): string {
  const text = message.toLowerCase();
  for (const row of form.mounts) {
    const source = row.source.trim().toLowerCase();
    if (source !== "" && text.includes(source)) return `mounts.${row.key}.source`;
    const target = row.target.trim().toLowerCase();
    if (target !== "" && text.includes(target)) return `mounts.${row.key}.target`;
  }
  for (const row of form.ports) {
    const host = row.host.trim();
    if (host !== "" && new RegExp(`\\b${host}\\b`).test(text)) return `ports.${row.key}.host`;
  }
  if (/\bports?\b/.test(text) && form.ports[0]) return `ports.${form.ports[0].key}.host`;
  const image = form.image.trim().toLowerCase();
  if (image !== "" && text.includes(image)) return "image";
  if (/\bimage\b/.test(text)) return "image";
  if (name !== "" && (/\bname\b/.test(text) || text.includes(` ${name} `))) return "name";
  return "";
}
