// SPDX-License-Identifier: AGPL-3.0-only

import { Connection, HealthCheckKind } from "@dockwell/api";
import { StatusBadge } from "@dockwell/ui";
import { useLingui } from "@lingui/react/macro";
import { useMemo } from "react";

import { useTimeWords } from "../../lib/time";
import { MachineWords } from "../changes/FailureNotice";
import { useAgoNow, useFleetView } from "../servers/useFleet";
import type { AppHealth, HealthKind, RunsOnServer, Unread } from "./health";

/** How to see whether Docker runs on a server. */
export const DOCKER_STATUS = "systemctl status docker";

/** What a check said, as a sentence: the agent writes it to go inside one. */
function asSentence(said: string): string {
  const out = said.charAt(0).toUpperCase() + said.slice(1);
  return /[.!?]$/.test(out) ? out : `${out}.`;
}

/** How something runs, in the active locale. */
export interface HealthWords {
  /** In a word: "Healthy", or "Running" where nothing checks it. */
  word(health: Pick<AppHealth, "kind" | "read">): string;
  kind(kind: HealthKind): string;
  /** What is known of it on a server, for someone reading why it reads as it does. */
  says(server: RunsOnServer, now: number): string;
  /** Why the panel can't say how it runs, and when it last could. */
  unread(health: Pick<AppHealth, "why" | "heard">, now: number): string;
  /** The same, of every app at once, for a list to say once above it. */
  unreadAll(why: Unread, heard: Date | undefined, now: number): string;
  /** What nothing checking it comes to. */
  noCheck: string;
}

export function useHealthWords(): HealthWords {
  const { t } = useLingui();
  const time = useTimeWords();
  const { servers } = useFleetView();
  return useMemo<HealthWords>(() => {
    // An agent the fleet has gone quiet from: what it last said is all there is.
    const gone = (id: string) => {
      const server = servers?.find((s) => s.id === id);
      return server !== undefined && server.connection !== Connection.CONNECTED;
    };
    const kind = (k: HealthKind): string => {
      switch (k) {
        case "healthy":
          return t`Healthy`;
        case "running":
          return t`Running`;
        case "starting":
          return t`Starting`;
        case "unhealthy":
          return t`Unhealthy`;
        case "not-running":
          return t`Not running`;
        case "paused":
          return t`Paused`;
        case "unknown":
          return t`Unknown`;
        case "finished":
          return t`Finished`;
      }
    };
    const noCheck = t`Nothing checks it, so this only means it started.`;
    return {
      kind,
      noCheck,
      unread(health, now) {
        const ago = health.heard ? time.ago(health.heard, now) : "";
        if (health.why === "offline") {
          return ago
            ? t`This browser is offline, so the panel can't say how this runs. It last heard ${ago}. Check your connection.`
            : t`This browser is offline, so the panel can't say how this runs. Check your connection.`;
        }
        return ago
          ? t`The panel can't reach the control plane, so it can't say how this runs. It last heard ${ago}, and keeps trying.`
          : t`The panel can't reach the control plane, so it can't say how this runs. It keeps trying.`;
      },
      unreadAll(why, heard, now) {
        const ago = heard ? time.ago(heard, now) : "";
        if (why === "offline") {
          return ago
            ? t`This browser is offline, so the panel can't say how your apps run, and they read Unknown. It last heard ${ago}. Check your connection.`
            : t`This browser is offline, so the panel can't say how your apps run, and they read Unknown. Check your connection.`;
        }
        return ago
          ? t`The panel can't reach the control plane, so it can't say how your apps run, and they read Unknown. It last heard ${ago}, and keeps trying.`
          : t`The panel can't reach the control plane, so it can't say how your apps run, and they read Unknown. It keeps trying.`;
      },
      word: (health) => (health.read === "reading" ? t`Checking` : kind(health.kind)),
      says(server, now) {
        const said = server.last?.output ?? "";
        switch (server.kind) {
          case "healthy":
            return said ? asSentence(said) : t`Its check passed.`;
          case "running":
            return noCheck;
          case "starting":
            return said ? t`Its check hasn't passed yet: ${said}.` : t`Its check hasn't finished yet.`;
          case "unhealthy":
            return said ? asSentence(said) : t`Its check keeps failing.`;
          case "not-running":
            return said ? asSentence(said) : t`It has no container on this server.`;
          case "finished":
            return t`It ran to its end and exited 0, as a one-shot job does, so nothing restarts it.`;
          case "paused": {
            const command = `docker unpause ${server.container}`;
            return t`Someone paused it, so it serves nothing and no check runs. Unpause it with ${command}.`;
          }
          case "unknown": {
            const name = server.serverName;
            if (server.dockerUnreadableSince) {
              const since = time.ago(server.dockerUnreadableSince, now);
              const command = DOCKER_STATUS;
              return t`The agent on ${name} can't read Docker, since ${since}, so it can't say how this runs. Check that Docker is running there, with ${command}.`;
            }
            const word = server.lastSaid ? kind(server.lastSaid) : "";
            const ago = server.lastKnown ? time.ago(server.lastKnown, now) : "";
            if (gone(server.serverId)) {
              return word && ago
                ? t`The agent on ${name} isn't connected, so it can't say how this runs now. It last read ${word}, ${ago}.`
                : t`The agent on ${name} isn't connected, so it can't say how this runs now.`;
            }
            if (server.stale) {
              return word && ago
                ? t`The agent on ${name} hasn't said how this runs since ${ago}, when it read ${word}.`
                : t`The agent on ${name} hasn't said how this runs lately.`;
            }
            return t`The agent on ${name} hasn't read how this runs yet.`;
          }
        }
      },
    };
  }, [t, time, servers]);
}

/** How it runs now, for what the record has to say, where that's known. Nothing, where it isn't. */
export function useRunsNow(health: AppHealth | undefined, name: string): string {
  const { t } = useLingui();
  if (health?.read !== "read") return "";
  switch (health.kind) {
    case "not-running":
      return t`${name} isn't running now.`;
    case "paused":
      return t`${name} is paused now, so it serves nothing.`;
    case "unhealthy":
      return t`${name} runs now, and fails its health check.`;
    case "unknown":
      return t`Nobody can say how ${name} runs now.`;
    case "healthy":
    case "running":
    case "starting":
    case "finished":
      return "";
  }
}

/** How something runs, as a shape, a word and a colour. */
export function HealthBadge({
  health,
  title,
  className,
  variant,
}: {
  health: Pick<AppHealth, "kind" | "read" | "status">;
  title?: string;
  className?: string;
  variant?: "dot" | "pill";
}) {
  const words = useHealthWords();
  const word = words.word(health);
  return (
    <StatusBadge
      status={health.status}
      label={word}
      labelIsState
      {...(variant ? { variant } : {})}
      {...(className ? { className } : {})}
      title={title ?? word}
    />
  );
}

/** A line for what's read of an app, for a tooltip: what it reads as, and why, where it says one thing. */
export function useHealthTitle(health: AppHealth): string {
  const words = useHealthWords();
  const now = useAgoNow(undefined);
  if (health.read === "failed") return words.unread(health, now);
  const [only] = health.servers;
  if (!only || health.servers.length > 1) return words.word(health);
  return `${words.word(health)}: ${words.says(only, now)}`;
}

/**
 * How it runs on each server: what it reads as and for how long, what its check last
 * said and when. With more than one server, each says for itself.
 */
export function RunsOnServers({ health, fleet }: { health: AppHealth; fleet: boolean }) {
  const { t } = useLingui();
  const words = useHealthWords();
  const time = useTimeWords();
  const newest = health.servers
    .map((s) => s.last?.at)
    .filter((at): at is Date => at !== undefined)
    .sort((a, b) => b.getTime() - a.getTime())[0];
  const now = useAgoNow(newest);
  if (health.read === "failed") {
    return <p className="app-runs-unread">{words.unread(health, now)}</p>;
  }
  if (health.read === "reading" || health.servers.length === 0) {
    return (
      <p className="app-runs-unread" aria-busy={health.read === "reading"}>
        {health.read === "reading" ? t`Reading how it runs.` : t`It isn't placed on a server.`}
      </p>
    );
  }
  return (
    <ul className="app-runs">
      {health.servers.map((server) => {
        const span = server.since ? time.span(Math.max(now - server.since.getTime(), 0)) : "";
        const checked = server.last?.at && server.check !== HealthCheckKind.NONE ? time.ago(server.last.at, now) : "";
        const showTime = server.kind !== "not-running" && server.kind !== "paused" && server.kind !== "unknown";
        return (
          <li key={server.serverId} className="app-runs-row">
            {fleet ? <code className="app-runs-server">{server.serverName}</code> : null}
            <StatusBadge
              status={server.status}
              label={words.kind(server.kind)}
              labelIsState
              {...(span ? { meta: t`for ${span}` } : {})}
            />
            <p className="app-runs-said">
              <MachineWords names={[server.container, server.serverName, DOCKER_STATUS]}>
                {words.says(server, now)}
              </MachineWords>
              {checked && showTime ? <span className="app-muted"> {checked}</span> : null}
            </p>
          </li>
        );
      })}
    </ul>
  );
}

/**
 * The class of a state's word in a phone's list. Healthy stays quiet, as M-Apps
 * has it: only what needs a look, or is moving, takes its colour.
 */
export function phoneWordClass(tone: string): string {
  return tone === "healthy" ? "apps-phone-word" : `apps-phone-word dw-tone-${tone}`;
}
