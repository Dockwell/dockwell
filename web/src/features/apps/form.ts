// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import { type EditSchema, Protocol } from "@dockwell/api";

/** One port someone is publishing. */
export interface PortRow {
  key: number;
  host: string;
  container: string;
  protocol: "tcp" | "udp";
}

/** One environment variable someone is setting. */
export interface EnvRow {
  key: number;
  name: string;
  value: string;
}

/** One volume or server path someone is mounting. */
export interface MountRow {
  key: number;
  source: string;
  target: string;
  readOnly: boolean;
}

/** New app's form, for an image. */
export interface ImageForm {
  image: string;
  /** What they typed. Empty takes the image's name. */
  name: string;
  ports: PortRow[];
  env: EnvRow[];
  mounts: MountRow[];
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
              image: folder ? "" : form.image.trim(),
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
            },
          },
        },
      },
    },
    servers,
  });
  return edits;
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
