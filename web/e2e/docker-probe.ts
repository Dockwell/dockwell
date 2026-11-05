// SPDX-License-Identifier: AGPL-3.0-only

// What Docker itself says runs, as a person checking with docker ps would see
// it, so a flow proves more than what the API reports. It talks to the Engine
// API on a socket, the real one's or scripts/fakedocker's.

import { request } from "node:http";

export interface Running {
  id: string;
  /** Its name, such as "env_01k…-web-1". */
  name: string;
  /** The image it was asked to run. */
  image: string;
  /** Its Compose project, and its service in it. */
  project: string;
  service: string;
  /** The networks it is on. */
  networks: string[];
  running: boolean;
}

function get<T>(socket: string, path: string): Promise<T> {
  return new Promise((done, failed) => {
    const req = request({ socketPath: socket, path, method: "GET" }, (res) => {
      let body = "";
      res.on("data", (chunk: Buffer) => {
        body += chunk.toString();
      });
      res.on("end", () => {
        if ((res.statusCode ?? 500) >= 300) failed(new Error(`Docker answered ${res.statusCode} to ${path}: ${body}`));
        else done(JSON.parse(body) as T);
      });
    });
    req.on("error", failed);
    req.end();
  });
}

interface Summary {
  Id: string;
  Labels?: Record<string, string>;
}

interface Inspected {
  Name: string;
  Config: { Image: string; Labels?: Record<string, string> };
  State: { Running: boolean };
  NetworkSettings: { Networks?: Record<string, unknown> };
}

const PROJECT = "com.docker.compose.project";
const SERVICE = "com.docker.compose.service";

/** Every container of a Compose project of an environment, as Docker has it. */
export function dockerProbe(socket: string) {
  return async function containers(): Promise<Running[]> {
    const list = await get<Summary[]>(socket, "/containers/json?all=1");
    const ours = list.filter((c) => (c.Labels?.[PROJECT] ?? "").startsWith("env_"));
    return Promise.all(
      ours.map(async (c) => {
        const seen = await get<Inspected>(socket, `/containers/${c.Id}/json`);
        return {
          id: c.Id,
          name: seen.Name.replace(/^\//, ""),
          image: seen.Config.Image,
          project: seen.Config.Labels?.[PROJECT] ?? "",
          service: seen.Config.Labels?.[SERVICE] ?? "",
          networks: Object.keys(seen.NetworkSettings.Networks ?? {}),
          running: seen.State.Running,
        };
      }),
    );
  };
}

export type DockerProbe = ReturnType<typeof dockerProbe>;
