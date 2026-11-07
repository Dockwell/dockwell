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

/** Memory as Docker takes it, such as "512M" or "2G", from 6M. */
export function validMemory(value: string): boolean {
  const match = /^(\d+)([kmg])?b?$/i.exec(value.trim());
  if (!match) return false;
  const unit = { k: 1 / 1024, m: 1, g: 1024 }[(match[2] ?? "").toLowerCase() as "k" | "m" | "g"] ?? 1 / (1024 * 1024);
  return Number(match[1]) * unit >= 6;
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
