// SPDX-License-Identifier: AGPL-3.0-only

import { Protocol, RestartPolicy } from "@dockwell/api";
import { describe, expect, it } from "vitest";
import { parseDockerRun } from "./dockerRun";

describe("docker run import", () => {
  it("preserves literal arguments, mapped port ranges and external intent", () => {
    const command = [
      "docker container run -dit --name demo --publish '[::1]:8080-8081:80-81/tcp'",
      "-e 'FIRST=a b=$literal' --env=SECOND=b=c -l org.demo=blue -u 42:12 -w /app",
      "-v data:/data:ro --mount 'type=bind,src=/srv/demo,dst=/config,readonly'",
      `--restart=on-failure:4 --network clients busybox:1 sh -c 'printf %s "$FIRST"' ''`,
    ].join(" \\\n");
    const result = parseDockerRun(command);
    expect(result.issues).toEqual([]);
    expect(result.name).toBe("demo");
    expect(result.app).toMatchObject({
      image: "busybox:1",
      command: ["sh", "-c", 'printf %s "$FIRST"', ""],
      env: { FIRST: "a b=$literal", SECOND: "b=c" },
      ports: [
        { hostIp: "::1", hostPort: 8080, containerPort: 80, protocol: Protocol.TCP },
        { hostIp: "::1", hostPort: 8081, containerPort: 81, protocol: Protocol.TCP },
      ],
      labels: { "org.demo": "blue" },
      restart: RestartPolicy.ON_FAILURE,
      restartRetries: 4,
      externalNetworks: ["clients"],
      user: "42:12",
      workingDir: "/app",
      stdinOpen: true,
      tty: true,
      mounts: [
        { source: { case: "volume", value: "data" }, target: "/data", readOnly: true },
        { source: { case: "hostPath", value: "/srv/demo" }, target: "/config", readOnly: true },
      ],
    });
  });

  it("lists unsupported flags, client environment lookups and shell expansions", () => {
    const result = parseDockerRun(
      'docker run --rm --env-file ./app.env -e TOKEN -p 80 --gpus=all busybox echo "$TOKEN"',
    );
    expect(result.issues).toEqual(["shell", "--rm", "--env-file", "-e", "-p", "--gpus"]);
    expect(result.app?.image).toBe("busybox");
    expect(parseDockerRun("docker run image '\\").syntax).toBe("quote");
    expect(parseDockerRun("docker run --network=host nginx").app?.hostNetwork).toBe(true);
  });

  it("accepts a full local image ID and uses Docker's restart default", () => {
    const id = `sha256:${"a".repeat(64)}`;
    expect(parseDockerRun(`docker run ${id}`).app).toMatchObject({ image: id, restart: RestartPolicy.NO });
  });
});
