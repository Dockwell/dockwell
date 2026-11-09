// SPDX-License-Identifier: AGPL-3.0-only

import { clone, create, type MessageInitShape } from "@bufbuild/protobuf";
import {
  type Container,
  type EditSchema,
  type Healthcheck,
  type Service,
  type ServiceInfo,
  ServiceSchema as SpecServiceSchema,
} from "@dockwell/api";

/**
 * The edit that plans an app as it was read, with what mutate changes: from the
 * revision it was read at, so a change that landed meanwhile makes it stale
 * rather than undone. No response carries the values of the app's own
 * environment, so the edit keeps every one of them as it is, and sets none.
 */
export function serviceEdit(
  service: ServiceInfo,
  mutate: (app: Container, spec: Service) => void,
): MessageInitShape<typeof EditSchema> {
  const spec = service.spec ? clone(SpecServiceSchema, service.spec) : create(SpecServiceSchema);
  if (spec.runs.case !== "app") throw new Error("the app runs no image, so there's nothing to edit");
  const app = spec.runs.value;
  const keepEnv = Object.keys(app.env).sort();
  app.env = {};
  mutate(app, spec);
  return {
    resource: service.resource,
    spec: { kind: { case: "service", value: spec } },
    baseRevision: service.revision,
    keepEnv,
  };
}

/**
 * Splits a command line into its words, as a shell would without running
 * anything: spaces part them, and quotes and backslashes keep them together.
 * Undefined for a quote that isn't closed.
 */
export function splitWords(line: string): string[] | undefined {
  const words: string[] = [];
  let word = "";
  let started = false;
  let quote: '"' | "'" | undefined;
  for (let i = 0; i < line.length; i++) {
    const ch = line[i] ?? "";
    if (quote) {
      if (ch === quote) quote = undefined;
      else if (ch === "\\" && quote === '"' && (line[i + 1] === '"' || line[i + 1] === "\\")) word += line[++i];
      else word += ch;
      continue;
    }
    if (ch === '"' || ch === "'") {
      quote = ch;
      started = true;
    } else if (ch === "\\" && i + 1 < line.length) {
      word += line[++i];
      started = true;
    } else if (/\s/.test(ch)) {
      if (started) words.push(word);
      word = "";
      started = false;
    } else {
      word += ch;
      started = true;
    }
  }
  if (quote) return undefined;
  if (started) words.push(word);
  return words;
}

/** A command's words as one line splitWords reads back the same, quoting those that need it. */
export function joinWords(words: readonly string[]): string {
  return words.map((w) => (w !== "" && /^[\w@%+=:,./-]+$/.test(w) ? w : `'${w.replaceAll("'", `'"'"'`)}'`)).join(" ");
}

/** A duration as Docker and Go take one, such as "10s", "1m30s" or "500ms". */
export function validDuration(value: string): boolean {
  return /^(\d+(\.\d+)?(ns|us|µs|ms|s|m|h))+$/.test(value.trim());
}

/** CPUs as Docker takes them, such as "0.5" or "2", to three decimal places. */
export function validCpus(value: string): boolean {
  const v = value.trim();
  return /^\d+(\.\d{1,3})?$/.test(v) && Number(v) > 0;
}

const UNITS: Record<string, number> = { "": 1, k: 2 ** 10, m: 2 ** 20, g: 2 ** 30, t: 2 ** 40 };

/**
 * Memory in bytes, as the control plane reads an amount: a number, perhaps with
 * a fraction, then k, m, g or t, perhaps with i and b after, in units of 1024,
 * such as "512M", "1.5G", "1Gi" or "1T". Undefined for anything else.
 */
export function parseMemory(value: string): number | undefined {
  const match = /^(\d+)(?:\.(\d+))?\s*([kmgt]?)i?b?$/i.exec(value.trim());
  if (!match) return undefined;
  const scale = UNITS[(match[3] ?? "").toLowerCase()] ?? 1;
  return Math.floor((Number(match[1]) + Number(`0.${match[2] ?? "0"}`)) * scale);
}

/** Memory as Docker takes it, from 6M (parseMemory). */
export function validMemory(value: string): boolean {
  return (parseMemory(value) ?? 0) >= 6 * 2 ** 20;
}

const SYSTEM_PATHS = [
  "/bin",
  "/boot",
  "/dev",
  "/etc",
  "/lib",
  "/lib32",
  "/lib64",
  "/proc",
  "/root",
  "/run",
  "/sbin",
  "/sys",
  "/usr",
  "/nix",
  "/snap",
  "/var/run",
  "/var/spool/anacron",
  "/var/spool/at",
  "/var/spool/cron",
  "/var/lib/dpkg",
  "/var/lib/rpm",
  "/var/lib/snapd",
  "/var/lib/systemd",
  "/var/lib/containerd",
  "/var/lib/containers",
  "/var/lib/docker",
  "/var/lib/kubelet",
  "/var/lib/libvirt",
  "/var/lib/lxc",
  "/var/lib/lxd",
  "/var/lib/rancher",
  "/var/snap/docker",
  "/var/snap/lxd",
];
const RUNTIME_SOCKETS = [
  "docker.sock",
  "containerd.sock",
  "cri-dockerd.sock",
  "crio.sock",
  "libvirt-sock",
  "podman.sock",
  "snapd.socket",
];
const HARMLESS_TO_READ = ["/etc/localtime", "/etc/timezone", "/usr/share/zoneinfo"];

const within = (p: string, base: string) => p === base || base === "/" || p.startsWith(`${base}/`);

/**
 * Whether a server path is as good as root to mount, as the control plane's
 * SystemPath reads it, so a form can say a passkey is coming before it plans: a
 * system path, or one that holds one, a runtime's socket, a person's home or its
 * dotfiles, and any .ssh. A guess for words alone; the plan decides, and reads
 * where a path leads too.
 */
export function asGoodAsRoot(source: string, readOnly: boolean): boolean {
  const p = source.trim().replace(/\/+$/, "") || "/";
  if (!p.startsWith("/")) return false;
  if (readOnly && HARMLESS_TO_READ.some((h) => within(p, h))) return false;
  const parts = p.split("/");
  if (RUNTIME_SOCKETS.includes(parts[parts.length - 1] ?? "") || parts.includes(".ssh")) return true;
  if (p === "/home" || (p.startsWith("/home/") && (parts.length === 3 || (parts[3] ?? "").startsWith("."))))
    return true;
  return SYSTEM_PATHS.some((s) => within(p, s) || within(s, p));
}

/** A duration as a person reads one: "10s" as "10 s", and "1m30s" as "1 min 30 s". */
export function durationWords(value: string): string {
  return value
    .replace(
      /(\d+(?:\.\d+)?)(ms|us|µs|ns|s|m|h)/g,
      (_, n: string, unit: string) => `${n} ${unit === "m" ? "min" : unit} `,
    )
    .trim();
}

/** How an app's health is checked, as the Settings tab chooses it. */
export type CheckKind = "http" | "tcp" | "command" | "image" | "none";

export function checkKindOf(check: Healthcheck | undefined): CheckKind {
  if (!check) return "image";
  if (check.http) return "http";
  if (check.tcp) return "tcp";
  if (check.test[0] === "NONE") return "none";
  if (check.test.length > 0) return "command";
  return "image";
}

/** A check's command as a person writes it: what runs, without CMD or CMD-SHELL. */
export function checkCommand(check: Healthcheck | undefined): string {
  const test = check?.test ?? [];
  if (test[0] === "CMD-SHELL") return test.slice(1).join(" ");
  if (test[0] === "CMD") return joinWords(test.slice(1));
  return "";
}
