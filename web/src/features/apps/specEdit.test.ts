// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { HealthcheckSchema } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import { checkCommand, checkKindOf, joinWords, splitWords, validCpus, validDuration, validMemory } from "./specEdit";

describe("a command line", () => {
  it("splits as a shell would, keeping quoted words whole", () => {
    expect(splitWords(`node server.js --name "my app" --path='/a b' a\\ b`)).toEqual([
      "node",
      "server.js",
      "--name",
      "my app",
      "--path=/a b",
      "a b",
    ]);
    expect(splitWords("  ")).toEqual([]);
    expect(splitWords(`echo ""`)).toEqual(["echo", ""]);
    expect(splitWords(`echo "unclosed`)).toBeUndefined();
  });

  it("joins so it splits back the same", () => {
    for (const words of [
      ["serve", "--port", "80"],
      ["sh", "-c", "echo 'hi' && sleep 1"],
      ["printf", ""],
    ]) {
      expect(splitWords(joinWords(words))).toEqual(words);
    }
    expect(joinWords(["serve", "--port", "80"])).toBe("serve --port 80");
  });
});

describe("what Docker takes", () => {
  it("reads durations, CPUs and memory as Docker does", () => {
    expect(["10s", "1m30s", "500ms", "2h"].every(validDuration)).toBe(true);
    expect(["10", "ten s", "-1s", ""].some(validDuration)).toBe(false);
    expect(["0.5", "2", "1.125"].every(validCpus)).toBe(true);
    expect(["0", "1.2345", "-1", "two"].some(validCpus)).toBe(false);
    expect(["512M", "2G", "6m", "6144k"].every(validMemory)).toBe(true);
    expect(["5M", "lots", "1T", "100"].some(validMemory)).toBe(false);
  });
});

describe("a health check's kind", () => {
  it("reads what the spec says, and what a person writes for a command", () => {
    expect(checkKindOf(undefined)).toBe("image");
    expect(checkKindOf(create(HealthcheckSchema, { http: { port: 80 } }))).toBe("http");
    expect(checkKindOf(create(HealthcheckSchema, { tcp: { port: 5432 } }))).toBe("tcp");
    expect(checkKindOf(create(HealthcheckSchema, { test: ["NONE"] }))).toBe("none");
    expect(checkKindOf(create(HealthcheckSchema, { interval: "5s" }))).toBe("image");
    const shell = create(HealthcheckSchema, { test: ["CMD-SHELL", "curl -f http://localhost || exit 1"] });
    expect(checkKindOf(shell)).toBe("command");
    expect(checkCommand(shell)).toBe("curl -f http://localhost || exit 1");
    expect(checkCommand(create(HealthcheckSchema, { test: ["CMD", "pg_isready", "-U", "app user"] }))).toBe(
      "pg_isready -U 'app user'",
    );
  });
});
