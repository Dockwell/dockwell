// SPDX-License-Identifier: AGPL-3.0-only

import type { MessageInitShape } from "@bufbuild/protobuf";
import {
  type ContainerSchema,
  type MountSchema,
  type PortMappingSchema,
  Protocol,
  Relabel,
  RestartPolicy,
} from "@dockwell/api";

type Mount = MessageInitShape<typeof MountSchema>;
type Port = MessageInitShape<typeof PortMappingSchema>;
export type RunApp = Exclude<MessageInitShape<typeof ContainerSchema>, { $typeName: string }>;
type Token = { value: string; shell: boolean };
type Result = { name: string; app?: RunApp; issues: string[]; syntax?: "command" | "quote" | "image" | "length" };

function words(input: string): Token[] | undefined {
  const list: Token[] = [];
  let value = "";
  let quote = "";
  let started = false;
  let shell = false;
  for (let i = 0; i < input.length; i++) {
    const ch = input.charAt(i);
    if (ch === "\\" && quote !== "'") {
      const next = input.charAt(++i);
      if (!next) return undefined;
      if (next === "\n" || (next === "\r" && input.charAt(i + 1) === "\n")) {
        if (next === "\r") i++;
        continue;
      }
      value += quote === '"' && !'\\`"$'.includes(next) ? `\\${next}` : next;
      started = true;
    } else if (quote) {
      if (ch === quote) quote = "";
      else {
        shell ||= quote === '"' && (ch === "$" || ch === "`");
        value += ch;
      }
    } else if (ch === "'" || ch === '"') {
      quote = ch;
      started = true;
    } else if (/\s/.test(ch)) {
      if (ch === "\n" && input.slice(i).trim()) {
        if (started) shell = true;
        else {
          const previous = list.at(-1);
          if (previous) previous.shell = true;
        }
      }
      if (started) list.push({ value, shell });
      started = false;
      shell = false;
      value = "";
    } else {
      shell ||= /[$`;&|<>*?~(){}]/.test(ch) || (ch === "#" && !started);
      value += ch;
      started = true;
    }
  }
  if (quote) return undefined;
  if (started) list.push({ value, shell });
  return list;
}

const aliases: Readonly<Record<string, string>> = {
  "-p": "--publish",
  "-e": "--env",
  "-v": "--volume",
  "-l": "--label",
  "-u": "--user",
  "-w": "--workdir",
  "--net": "--network",
};
const valued = new Set([
  "--name",
  "--publish",
  "--env",
  "--volume",
  "--mount",
  "--label",
  "--user",
  "--workdir",
  "--restart",
  "--network",
  "--entrypoint",
  "--expose",
]);
const unsupportedWithValue = new Set([
  "--env-file",
  "--label-file",
  "--platform",
  "--pull",
  "--hostname",
  "-h",
  "--add-host",
  "--cap-add",
  "--cap-drop",
  "--security-opt",
  "--pid",
  "--ipc",
  "--uts",
  "--userns",
  "--cpus",
  "--memory",
  "-m",
  "--memory-swap",
  "--gpus",
  "--device",
  "--network-alias",
  "--dns",
  "--ip",
  "--ip6",
  "--mac-address",
  "--health-cmd",
  "--log-driver",
  "--log-opt",
  "--tmpfs",
  "--volumes-from",
  "--cidfile",
]);

function ports(value: string): Port[] | undefined {
  const match = /^(?:(\[[\da-fA-F:.]+\]|[^:]+):)?(\d+)(?:-(\d+))?:(\d+)(?:-(\d+))?(?:\/(tcp|udp))?$/.exec(value);
  if (!match) return undefined;
  const [, ip = "", host, hostEnd = host, target, targetEnd = target, protocol] = match;
  const count = Number(hostEnd) - Number(host) + 1;
  if (
    count < 1 ||
    count > 32 ||
    Number(targetEnd) - Number(target) + 1 !== count ||
    Number(host) < 1 ||
    Number(target) < 1 ||
    Number(hostEnd) > 65535 ||
    Number(targetEnd) > 65535
  )
    return undefined;
  return Array.from({ length: count }, (_, i) => ({
    hostPort: Number(host) + i,
    containerPort: Number(target) + i,
    protocol: protocol === "udp" ? Protocol.UDP : Protocol.TCP,
    hostIp: ip.replace(/^\[|\]$/g, ""),
  }));
}

function mount(source: string, target: string, options: string[]): Mount | undefined {
  const bind = source.startsWith("/");
  if (
    !target.startsWith("/") ||
    options.some((option) => !["ro", "rw", "z", "Z"].includes(option)) ||
    (!bind && (options.includes("z") || options.includes("Z"))) ||
    (!bind && source !== "" && !/^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/.test(source))
  )
    return undefined;
  return {
    source: bind ? { case: "hostPath", value: source } : { case: "volume", value: source },
    target,
    readOnly: options.includes("ro"),
    relabel: options.includes("Z") ? Relabel.PRIVATE : options.includes("z") ? Relabel.SHARED : Relabel.UNSPECIFIED,
  };
}

function longMount(value: string): Mount | undefined {
  const fields: Record<string, string> = {};
  for (const field of value.split(",")) {
    const [key = "", v = ""] = field.split(/=(.*)/s, 2);
    const actual =
      key === "src" ? "source" : key === "dst" || key === "destination" ? "target" : key === "ro" ? "readonly" : key;
    if (!["type", "source", "target", "readonly"].includes(actual) || Object.hasOwn(fields, actual)) return undefined;
    fields[actual] = v;
  }
  const source = fields["source"] ?? "";
  const kind = fields["type"] ?? "volume";
  const read = fields["readonly"];
  if (
    !["volume", "bind"].includes(kind) ||
    (kind === "bind") !== source.startsWith("/") ||
    ![undefined, "", "true", "false"].includes(read)
  )
    return undefined;
  return mount(source, fields["target"] ?? "", read === "" || read === "true" ? ["ro"] : []);
}

/** Parses literal Docker arguments. Shell evaluation, client files and unhandled options block review. */
export function parseDockerRun(input: string): Result {
  const empty = { name: "", issues: [] };
  if (input.length > 32_768) return { ...empty, syntax: "length" };
  const tokens = words(input.trim());
  if (!tokens) return { ...empty, syntax: "quote" };
  const command = tokens[1]?.value === "container" ? 2 : 1;
  if (tokens[0]?.value !== "docker" || tokens[command]?.value !== "run") return { ...empty, syntax: "command" };
  const issues = new Set<string>();
  if (tokens.some((token) => token.shell)) issues.add("shell");
  const published: Port[] = [];
  const mounts: Mount[] = [];
  const env: Record<string, string> = Object.create(null);
  const labels: Record<string, string> = Object.create(null);
  const networks = new Set<string>();
  const expose: string[] = [];
  const app: RunApp = { ports: published, mounts, env, labels, expose, restart: RestartPolicy.NO };
  let name = "";
  let i = command + 1;
  for (; i < tokens.length; i++) {
    const text = tokens[i]?.value ?? "";
    if (text === "--") {
      i++;
      break;
    }
    if (!text.startsWith("-")) break;
    if (/^-[dit]+$/.test(text)) {
      if (text.includes("i")) app.stdinOpen = true;
      if (text.includes("t")) app.tty = true;
      continue;
    }
    const flag = text.match(/^--[^=]+|^-[a-zA-Z]/)?.[0] ?? text;
    const option = aliases[flag] ?? flag;
    const tail = text.slice(flag.length);
    const attached = tail.startsWith("=") ? tail.slice(1) : !flag.startsWith("--") && tail ? tail : undefined;
    if (["--detach", "--interactive", "--tty"].includes(option)) {
      if (attached !== undefined && attached !== "true" && attached !== "false") issues.add(flag);
      if (option === "--interactive") app.stdinOpen = attached !== "false";
      if (option === "--tty") app.tty = attached !== "false";
      continue;
    }
    if (!valued.has(option)) {
      issues.add(/^--?[A-Za-z][A-Za-z0-9-]*$/.test(flag) ? flag : "shell");
      if (unsupportedWithValue.has(option) && attached === undefined && !tokens[i + 1]?.value.startsWith("-")) i++;
      continue;
    }
    const value = attached ?? tokens[++i]?.value;
    if (value === undefined || (value.startsWith("-") && attached === undefined)) {
      issues.add(flag);
      if (value !== undefined) i--;
      continue;
    }
    switch (option) {
      case "--name":
        name = value;
        break;
      case "--publish": {
        const mappings = ports(value);
        if (mappings) published.push(...mappings);
        else issues.add(flag);
        break;
      }
      case "--env":
      case "--label": {
        const [key = "", val] = value.split(/=(.*)/s, 2);
        if (!key || (option === "--env" && val === undefined)) issues.add(flag);
        else (option === "--env" ? env : labels)[key] = val ?? "";
        break;
      }
      case "--volume":
      case "--mount": {
        const [first = "", second, mode, ...rest] = value.split(":");
        const entry =
          option === "--mount"
            ? longMount(value)
            : rest.length
              ? undefined
              : second === undefined
                ? mount("", first, [])
                : first
                  ? mount(first, second, mode?.split(",") ?? [])
                  : undefined;
        if (entry) mounts.push(entry);
        else issues.add(flag);
        break;
      }
      case "--user":
        app.user = value;
        break;
      case "--workdir":
        app.workingDir = value;
        break;
      case "--entrypoint":
        app.entrypoint = value ? [value] : [];
        app.noEntrypoint = !value;
        break;
      case "--expose":
        if (/^\d{1,5}(?:\/(?:tcp|udp))?$/.test(value)) expose.push(value.includes("/") ? value : `${value}/tcp`);
        else issues.add(flag);
        break;
      case "--network":
        if (/^[a-zA-Z0-9][a-zA-Z0-9_.-]*$/.test(value) && value !== "none")
          networks.add(value === "default" ? "bridge" : value);
        else issues.add(flag);
        break;
      case "--restart": {
        const [policy = "", retries] = value.split(":", 2);
        const known = {
          no: RestartPolicy.NO,
          always: RestartPolicy.ALWAYS,
          "unless-stopped": RestartPolicy.UNLESS_STOPPED,
          "on-failure": RestartPolicy.ON_FAILURE,
        };
        const restart = Object.hasOwn(known, policy) ? known[policy as keyof typeof known] : undefined;
        if (
          restart === undefined ||
          (retries !== undefined && (policy !== "on-failure" || !/^\d+$/.test(retries) || Number(retries) > 4294967295))
        ) {
          issues.add(flag);
        } else {
          app.restart = restart;
          app.restartRetries = Number(retries ?? "0");
        }
        break;
      }
    }
  }
  const image = tokens[i]?.value ?? "";
  if (!image || image.startsWith("-")) return { name, issues: [...issues], syntax: "image" };
  if (networks.size > 1) issues.add("--network");
  if (networks.has("host")) {
    if (published.length > 0) issues.add("--network");
    app.hostNetwork = true;
  } else if (networks.size) app.externalNetworks = [...networks];
  app.image = image;
  app.command = tokens.slice(i + 1).map(({ value }) => value);
  return { name, app, issues: [...issues] };
}
