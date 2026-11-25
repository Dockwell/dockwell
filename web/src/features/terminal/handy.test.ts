// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { SavedCommandSchema, TerminalCursorStyle, TerminalPreferencesSchema } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { DEFAULT_SCROLLBACK, DESKTOP_FONT, PHONE_FONT, resolveDisplaySettings } from "./preferences";
import {
  commandLineCount,
  exportCommandsJson,
  hasDisallowedControlChars,
  looksLikeSecret,
  normalizeCommandBody,
  parseCommandsFile,
} from "./secrets";
import { parseOsc7Path } from "./TerminalScreen";

describe("terminal preferences", () => {
  it("uses mock defaults when preferences are unset", () => {
    const desktop = resolveDisplaySettings(undefined, false);
    expect(desktop.fontSize).toBe(DESKTOP_FONT);
    expect(desktop.fontFamilyKey).toBe("default");
    expect(desktop.cursorStyle).toBe("block");
    expect(desktop.cursorBlink).toBe(true);
    expect(desktop.scrollback).toBe(DEFAULT_SCROLLBACK);

    const phone = resolveDisplaySettings(undefined, true);
    expect(phone.fontSize).toBe(PHONE_FONT);
  });

  it("applies saved preferences consistently across desktop and phone", () => {
    const saved = create(TerminalPreferencesSchema, {
      fontSize: 16,
      fontFamily: "fira-code",
      lineHeight: 1.4,
      cursorStyle: TerminalCursorStyle.BAR,
      cursorBlink: false,
      scrollback: 25000,
    });
    const desktop = resolveDisplaySettings(saved, false);
    expect(desktop.fontSize).toBe(16);
    expect(desktop.fontFamilyKey).toBe("fira-code");
    expect(desktop.fontFamily).toContain("Fira Code");
    expect(desktop.lineHeight).toBe(1.4);
    expect(desktop.cursorStyle).toBe("bar");
    expect(desktop.cursorBlink).toBe(false);
    expect(desktop.scrollback).toBe(25000);

    expect(resolveDisplaySettings(saved, true).fontSize).toBe(16);
  });
});

describe("saved commands secret check, control chars, and import/export", () => {
  it("flags embedded secrets while ignoring shell variables and placeholders", () => {
    expect(looksLikeSecret("export API_KEY=sk-1234567890abcdefghij1234")).toBe(true);
    expect(looksLikeSecret("DATABASE_URL=postgres://app:hunter2@db:5432/app psql")).toBe(true);
    expect(looksLikeSecret("curl -H 'Authorization: Bearer ghp_1234567890abcdefghij1234567890abcd' https://x")).toBe(
      true,
    );
    expect(looksLikeSecret("export API_KEY=$API_KEY")).toBe(false);
    expect(looksLikeSecret("docker ps --format '{{.Names}}'")).toBe(false);
    expect(looksLikeSecret("df -h")).toBe(false);
  });

  it("normalizes CRLF and refuses control characters except LF and TAB in command bodies", () => {
    expect(normalizeCommandBody("cd /srv\r\n\tdocker ps\r\n\n")).toBe("cd /srv\n\tdocker ps");
    expect(commandLineCount("cd /srv\r\n\tdocker ps\r\n")).toBe(2);
    expect(hasDisallowedControlChars("cd /srv\n\tdocker ps", true)).toBe(false);
    expect(hasDisallowedControlChars("echo \x1b[31mhi", true)).toBe(true);
    expect(hasDisallowedControlChars("echo a\rb", true)).toBe(true);
    expect(hasDisallowedControlChars("echo \x03", true)).toBe(true);
    expect(hasDisallowedControlChars("echo \x7f", true)).toBe(true);
    expect(hasDisallowedControlChars("name\twith\ttab", false)).toBe(true);
    expect(hasDisallowedControlChars("name\nline", false)).toBe(true);
    expect(hasDisallowedControlChars("echo \u009b31m", true)).toBe(true);
    expect(hasDisallowedControlChars("echo safe \u202e# x\u202c", true)).toBe(true);
    expect(hasDisallowedControlChars("rm\u200b -rf /tmp/x", true)).toBe(true);
    expect(hasDisallowedControlChars("echo café 日本", true)).toBe(false);
  });

  it("round-trips exported JSON files", () => {
    const commands = [
      create(SavedCommandSchema, { id: "tcm_1", name: "Disk", command: "df -h", group: "Diagnostics", shared: false }),
      create(SavedCommandSchema, {
        id: "tcm_2",
        name: "Logs",
        command: "cd /srv\ndocker logs api",
        group: "Docker",
        shared: true,
      }),
    ];
    const json = exportCommandsJson(commands);
    const parsed = parseCommandsFile(json);
    expect(parsed.error).toBeUndefined();
    expect(parsed.commands).toEqual([
      { name: "Disk", command: "df -h", group: "Diagnostics", shared: false },
      { name: "Logs", command: "cd /srv\ndocker logs api", group: "Docker", shared: true },
    ]);
  });

  it("rejects invalid import files and control characters cleanly", () => {
    expect(parseCommandsFile("not json").error).toBe("invalid_json");
    expect(parseCommandsFile('{"version":1,"commands":[]}').error).toBe("empty");
    expect(parseCommandsFile('{"version":1,"commands":[{"name":"Only name"}]}').error).toBe("invalid_item");
    expect(
      parseCommandsFile(JSON.stringify({ version: 1, commands: [{ name: "Bad", command: "echo \x1b[31mhi" }] })).error,
    ).toBe("control_chars");
    expect(
      parseCommandsFile(JSON.stringify({ version: 1, commands: [{ name: "Bad", command: "echo a\rb" }] })).error,
    ).toBe("control_chars");
    expect(
      parseCommandsFile(JSON.stringify({ version: 1, commands: [{ name: "Bad", command: "echo \x7f" }] })).error,
    ).toBe("control_chars");
  });
});

describe("parseOsc7Path", () => {
  it("extracts absolute paths from OSC 7 file URIs", () => {
    expect(parseOsc7Path("file://hz-1/var/www/my%20site")).toBe("/var/www/my site");
    expect(parseOsc7Path("file:///etc/nginx")).toBe("/etc/nginx");
    expect(parseOsc7Path("https://example.com/a")).toBeUndefined();
    expect(parseOsc7Path("not-a-uri")).toBeUndefined();
  });
});
