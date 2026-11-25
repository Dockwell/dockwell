// SPDX-License-Identifier: AGPL-3.0-only

import type { SavedCommand } from "@dockwell/api";

const RE_PEM_KEY = /-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----/;
const RE_TOKEN_PREFIX =
  /\b(ghp_[A-Za-z0-9]{20,}|gho_[A-Za-z0-9]{20,}|ghu_[A-Za-z0-9]{20,}|ghs_[A-Za-z0-9]{20,}|ghr_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|sk-[A-Za-z0-9_-]{20,}|sk_(?:live|test)_[A-Za-z0-9]{16,}|rk_(?:live|test)_[A-Za-z0-9]{16,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,}|SG\.[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}|hf_[A-Za-z0-9]{20,}|npm_[A-Za-z0-9]{20,}|AIza[0-9A-Za-z_-]{35})\b/;
const RE_JWT = /\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b/;
const RE_URL_CREDS =
  /\b(?:https?|postgres(?:ql)?|mysql|mariadb|mongodb(?:\+srv)?|rediss?|amqps?|s?ftp):\/\/([^/\s:@]+):([^/\s@]+)@/i;
const RE_AUTH_HEADER = /Authorization\s*:\s*(?:Bearer|Basic|Token)\s+([^\s'"]+)/i;
const RE_ENV_ASSIGN =
  /\b[A-Z0-9_]*(?:PASSWORD|PASSWD|SECRET|API_?KEY|ACCESS_?KEY|PRIVATE_?KEY|AUTH_?TOKEN|CLIENT_?SECRET|TOKEN)\s*=\s*['"]?([^\s'"]+)/i;
const RE_SECRET_FLAG = /(?:--password|--passwd|--token|--api-key|--secret|--auth-token)(?:=|\s+)['"]?([^\s'"]+)/i;
const RE_CURL_AUTH = /\bcurl\b[^\n]*?\s(?:-u|--user)\s+['"]?[^:\s'"]+:([^\s'"]+)/i;
const RE_DB_PASS_FLAG = /\b(?:mysql|mariadb|psql|redis-cli)\b[^\n]*?\s(?:-p|-a)\s*['"]?([^\s'"-]+)/i;

const PLACEHOLDERS = new Set([
  "...",
  "xxx",
  "xxxx",
  "changeme",
  "change_me",
  "example",
  "placeholder",
  "redacted",
  "none",
  "null",
  "true",
  "false",
]);

function isPlaceholder(raw: string | undefined): boolean {
  if (!raw) return true;
  const v = raw.trim().replace(/^['"]+|['"]+$/g, "");
  if (!v) return true;
  if (v.startsWith("$") || (v.startsWith("<") && v.endsWith(">")) || (v.startsWith("{{") && v.endsWith("}}"))) {
    return true;
  }
  const lower = v.toLowerCase();
  return PLACEHOLDERS.has(lower) || lower.startsWith("your_") || lower.startsWith("your-");
}

/**
 * Normalizes a saved command body: folds CRLF to LF and strips trailing blank
 * newlines so a saved command never ends with an accidental Enter.
 */
export function normalizeCommandBody(raw: string): string {
  return raw.replaceAll("\r\n", "\n").replace(/\n+$/, "");
}

/**
 * Reports whether text contains disallowed ASCII control characters (< 0x20 or
 * DEL 0x7f). Command bodies allow explicit LF (0x0a) and TAB (0x09); names and
 * groups allow no control characters at all.
 */
export function hasDisallowedControlChars(text: string, allowMultiline: boolean): boolean {
  for (let i = 0; i < text.length; i++) {
    const code = text.charCodeAt(i);
    if (allowMultiline && (code === 0x0a || code === 0x09)) continue;
    if (code < 0x20 || code === 0x7f || hiddenCode(code)) return true;
  }
  return false;
}

/**
 * A character that changes what a command does without showing: C1 controls,
 * marks that reorder how text is drawn ("Trojan Source") and zero-width
 * characters, refused as the server refuses them.
 */
function hiddenCode(code: number): boolean {
  return (
    (code >= 0x80 && code <= 0x9f) ||
    code === 0x061c ||
    code === 0x200e ||
    code === 0x200f ||
    (code >= 0x202a && code <= 0x202e) ||
    (code >= 0x2066 && code <= 0x2069) ||
    (code >= 0x200b && code <= 0x200d) ||
    code === 0x2060 ||
    code === 0xfeff
  );
}

/** Counts the explicit lines in a normalized command body. */
export function commandLineCount(raw: string): number {
  const cleaned = normalizeCommandBody(raw);
  if (!cleaned) return 0;
  return cleaned.split("\n").length;
}

/**
 * Checks whether a command appears to embed a literal secret, API key, token,
 * or private key. Kept aligned with the control plane's validation.
 */
export function looksLikeSecret(command: string): boolean {
  if (RE_PEM_KEY.test(command) || RE_TOKEN_PREFIX.test(command) || RE_JWT.test(command)) return true;
  const urlMatch = RE_URL_CREDS.exec(command);
  if (urlMatch && !isPlaceholder(urlMatch[2])) return true;
  const authMatch = RE_AUTH_HEADER.exec(command);
  if (authMatch && !isPlaceholder(authMatch[1])) return true;
  const envMatch = RE_ENV_ASSIGN.exec(command);
  if (envMatch && !isPlaceholder(envMatch[1])) return true;
  const flagMatch = RE_SECRET_FLAG.exec(command);
  if (flagMatch && !isPlaceholder(flagMatch[1])) return true;
  const curlMatch = RE_CURL_AUTH.exec(command);
  if (curlMatch && !isPlaceholder(curlMatch[1])) return true;
  const dbMatch = RE_DB_PASS_FLAG.exec(command);
  if (dbMatch && !isPlaceholder(dbMatch[1])) return true;
  return false;
}

export interface ExportedCommand {
  name: string;
  command: string;
  group: string;
  shared: boolean;
}

export interface ExportedCommandsFile {
  version: 1;
  commands: ExportedCommand[];
}

/** Serializes saved commands into a clean, versioned JSON file for export. */
export function exportCommandsJson(commands: readonly SavedCommand[]): string {
  const payload: ExportedCommandsFile = {
    version: 1,
    commands: commands.map((c) => ({
      name: c.name,
      command: c.command,
      group: c.group,
      shared: c.shared,
    })),
  };
  return `${JSON.stringify(payload, null, 2)}\n`;
}

/** Parses and validates an imported JSON file of saved commands. */
export function parseCommandsFile(text: string): { commands: ExportedCommand[]; error?: string } {
  let raw: unknown;
  try {
    raw = JSON.parse(text);
  } catch {
    return { commands: [], error: "invalid_json" };
  }
  const list = Array.isArray(raw)
    ? raw
    : typeof raw === "object" && raw !== null && Array.isArray((raw as { commands?: unknown }).commands)
      ? (raw as { commands: unknown[] }).commands
      : undefined;
  if (!list) return { commands: [], error: "invalid_shape" };
  if (list.length === 0) return { commands: [], error: "empty" };
  if (list.length > 200) return { commands: [], error: "too_many" };

  const out: ExportedCommand[] = [];
  for (const item of list) {
    if (typeof item !== "object" || item === null) return { commands: [], error: "invalid_item" };
    const rec = item as Record<string, unknown>;
    const name = typeof rec["name"] === "string" ? rec["name"].trim() : "";
    const command = typeof rec["command"] === "string" ? normalizeCommandBody(rec["command"]) : "";
    const group = typeof rec["group"] === "string" ? rec["group"].trim() : "";
    const shared = Boolean(rec["shared"]);
    if (!name || !command.trim()) return { commands: [], error: "invalid_item" };
    if (
      hasDisallowedControlChars(name, false) ||
      hasDisallowedControlChars(group, false) ||
      hasDisallowedControlChars(command, true)
    ) {
      return { commands: [], error: "control_chars" };
    }
    out.push({ name, command, group, shared });
  }
  return { commands: out };
}
