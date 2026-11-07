// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { HealthcheckSchema } from "@dockwell/api";
import { describe, expect, it } from "vitest";

import {
  asGoodAsRoot,
  checkCommand,
  checkKindOf,
  durationWords,
  joinWords,
  parseMemory,
  splitWords,
  validCpus,
  validDuration,
  validMemory,
} from "./specEdit";

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
    expect(["512M", "2G", "6m", "6144k", "1.5G", "1Gi", "1T", "512MiB"].every(validMemory)).toBe(true);
    expect(["5M", "lots", "1P", "100", "1.G"].some(validMemory)).toBe(false);
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

describe("what a mount reaches, as the server reads it", () => {
  it("calls a system path, a socket, a home and an .ssh as good as root, and a site's folder not", () => {
    for (const p of [
      "/",
      "/etc",
      "/etc/nginx",
      "/var",
      "/var/run/docker.sock",
      "/run/containerd/containerd.sock",
      "/home",
      "/home/mira",
      "/home/mira/.bashrc",
      "/srv/x/.ssh",
    ]) {
      expect(asGoodAsRoot(p, false), p).toBe(true);
    }
    for (const p of ["/srv/site", "/home/mira/site", "/opt/app", "data"]) expect(asGoodAsRoot(p, false), p).toBe(false);
    // Reading the time zone is harmless; writing it isn't.
    expect(asGoodAsRoot("/etc/localtime", true)).toBe(false);
    expect(asGoodAsRoot("/etc/localtime", false)).toBe(true);
  });
});

describe("amounts and durations", () => {
  it("reads memory as the server's parseMemory does, and words a duration for a person", () => {
    expect(parseMemory("1.5G")).toBe(1.5 * 2 ** 30);
    expect(parseMemory("512MiB")).toBe(512 * 2 ** 20);
    expect(parseMemory("2048")).toBe(2048);
    expect(durationWords("10s")).toBe("10 s");
    expect(durationWords("1m30s")).toBe("1 min 30 s");
    expect(durationWords("500ms")).toBe("500 ms");
  });
});
