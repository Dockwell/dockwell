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

function call(socket: string, method: string, path: string, body?: unknown): Promise<string> {
  return new Promise((done, failed) => {
    const headers = body === undefined ? {} : { "Content-Type": "application/json" };
    const req = request({ socketPath: socket, path, method, headers }, (res) => {
      let body = "";
      res.on("data", (chunk: Buffer) => {
        body += chunk.toString();
      });
      res.on("end", () => {
        if ((res.statusCode ?? 500) >= 300) failed(new Error(`Docker answered ${res.statusCode} to ${path}: ${body}`));
        else done(body);
      });
    });
    req.on("error", failed);
    req.end(body === undefined ? undefined : JSON.stringify(body));
  });
}

async function get<T>(socket: string, path: string): Promise<T> {
  return JSON.parse(await call(socket, "GET", path)) as T;
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

/** Stops a container, as someone at the server would by hand. */
export async function dockerStop(socket: string, id: string): Promise<void> {
  await call(socket, "POST", `/containers/${id}/stop`);
}

/** Does to a container what a person at the server would by hand: docker stop, pause, kill and so on. */
export async function dockerAct(
  socket: string,
  id: string,
  action: "stop" | "start" | "pause" | "unpause" | "kill" | "restart",
): Promise<void> {
  await call(socket, "POST", `/containers/${id}/${action}`);
}

/** Runs a command in a container, as docker exec does, and leaves it to it. */
export async function dockerExec(socket: string, id: string, command: string[]): Promise<void> {
  const made = JSON.parse(await call(socket, "POST", `/containers/${id}/exec`, { Cmd: command })) as { Id: string };
  await call(socket, "POST", `/exec/${made.Id}/start`, { Detach: true });
}

/** Runs a container on Docker directly, as someone at the server would, so Dockwell has no part in it. */
export async function dockerRun(
  socket: string,
  name: string,
  run: { image: string; command?: string[]; env?: string[]; port?: number; mount?: string },
): Promise<string> {
  const [from, tag = "latest"] = run.image.split(":");
  await call(socket, "POST", `/images/create?${new URLSearchParams({ fromImage: from ?? "", tag })}`);
  const made = JSON.parse(
    await call(socket, "POST", `/containers/create?name=${name}`, {
      Image: run.image,
      Cmd: run.command,
      Env: run.env,
      ExposedPorts: run.port ? { "80/tcp": {} } : undefined,
      HostConfig: {
        PortBindings: run.port ? { "80/tcp": [{ HostPort: String(run.port) }] } : undefined,
        Binds: run.mount ? [run.mount] : undefined,
      },
    }),
  ) as { Id: string };
  await call(socket, "POST", `/containers/${made.Id}/start`);
  return made.Id;
}

/** Whether a container has been touched: anything that restarts or replaces it changes this. */
export async function dockerStarted(socket: string, id: string): Promise<string> {
  const seen = await get<{ Id: string; RestartCount: number; State: { StartedAt: string } }>(
    socket,
    `/containers/${id}/json`,
  );
  return `${seen.Id} ${seen.RestartCount} ${seen.State.StartedAt}`;
}

/** Removes a container and a volume it was the only one to use, as docker rm -f and docker volume rm do. */
export async function dockerRemove(socket: string, id: string, volume: string): Promise<void> {
  await call(socket, "DELETE", `/containers/${id}?force=1`);
  await call(socket, "DELETE", `/volumes/${volume}`);
}

/** Tells scripts/fakedocker how a container's check went, as it has none to run. */
export async function dockerHealth(socket: string, id: string, status: string, output: string): Promise<void> {
  const query = new URLSearchParams({ status, output });
  await call(socket, "POST", `/containers/${id}/x-dockwell-health?${query}`);
}

/** What a run's Dockwell makes on Docker, by the ID or name Docker removes it by. */
export interface DockerMade {
  containers: string[];
  networks: string[];
  volumes: string[];
}

interface Labelled {
  Labels?: Record<string, string> | null;
}

/**
 * What Docker has of an environment's Compose project. Each is labelled, so what
 * a person runs isn't counted, and a network or volume counts by itself: a run
 * that deleted its apps has no container left to say what they used.
 */
export async function dockerMade(socket: string): Promise<DockerMade> {
  const ours = (item: Labelled) => (item.Labels?.[PROJECT] ?? "").startsWith("env_");
  const containers = await get<(Summary & Labelled)[]>(socket, "/containers/json?all=1");
  const networks = await get<(Labelled & { Id: string })[]>(socket, "/networks");
  const volumes = await get<{ Volumes: (Labelled & { Name: string })[] | null }>(socket, "/volumes");
  return {
    containers: containers.filter(ours).map((c) => c.Id),
    networks: networks.filter(ours).map((n) => n.Id),
    volumes: (volumes.Volumes ?? []).filter(ours).map((v) => v.Name),
  };
}

/**
 * Removes what Docker has now that it didn't have at before, containers first, as
 * they hold the rest. It says what it couldn't: a dev machine has room for about
 * ten runs' networks before its address pools run out, so a run that leaves them
 * has failed.
 */
export async function removeMade(socket: string, before: DockerMade): Promise<string[]> {
  const now = await dockerMade(socket);
  const remove = async (list: string[], known: string[], path: (item: string) => string) => {
    for (const item of list.filter((i) => !known.includes(i))) await call(socket, "DELETE", path(item)).catch(() => {});
  };
  await remove(now.containers, before.containers, (id) => `/containers/${id}?force=true`);
  await remove(now.networks, before.networks, (id) => `/networks/${id}`);
  await remove(now.volumes, before.volumes, (name) => `/volumes/${name}`);
  const left = await dockerMade(socket);
  return [
    ...left.containers.filter((i) => !before.containers.includes(i)).map((i) => `container ${i}`),
    ...left.networks.filter((i) => !before.networks.includes(i)).map((i) => `network ${i}`),
    ...left.volumes.filter((i) => !before.volumes.includes(i)).map((i) => `volume ${i}`),
  ];
}
