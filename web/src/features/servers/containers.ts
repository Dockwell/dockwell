// SPDX-License-Identifier: AGPL-3.0-only

import { type ContainerMount, type ContainerPort, ContainerState, type ServerContainer } from "@dockwell/api";
import type { Status } from "@dockwell/ui";
import { msg } from "@lingui/core/macro";
import { useLingui } from "@lingui/react/macro";

/** How often a list of a server's containers is read again while it shows. */
export const READ_EVERY_MS = 15_000;

/** Where a container is, as the shape and colour every state has. */
export function statusOf(state: ContainerState): Status {
  switch (state) {
    case ContainerState.RUNNING:
      return "running";
    case ContainerState.RESTARTING:
      return "degraded";
    case ContainerState.REMOVING:
      return "deploying";
    case ContainerState.DEAD:
      return "down";
    case ContainerState.CREATED:
    case ContainerState.PAUSED:
    case ContainerState.EXITED:
      return "stopped";
  }
  return "unknown";
}

const STATES = {
  [ContainerState.CREATED]: msg`Not started`,
  [ContainerState.RUNNING]: msg`Running`,
  [ContainerState.PAUSED]: msg`Paused`,
  [ContainerState.RESTARTING]: msg`Restarting`,
  [ContainerState.REMOVING]: msg`Being removed`,
  [ContainerState.EXITED]: msg`Stopped`,
  [ContainerState.DEAD]: msg`Dead`,
} as const;

/** The word for where a container is. */
export function useContainerStateWord(): (state: ContainerState) => string {
  const { t, i18n } = useLingui();
  return (state) => {
    const word = STATES[state as keyof typeof STATES];
    return word ? i18n._(word) : t`Unknown`;
  };
}

/** A port as a person reads it: 8080→80, 127.0.0.1:5432→5432, or 53→53/udp. */
export function portWords(port: ContainerPort): string {
  const server = port.address ? `${port.address}:${port.serverPort}` : `${port.serverPort}`;
  return `${server}→${port.containerPort}${port.protocol === "tcp" ? "" : `/${port.protocol}`}`;
}

/** What a mount is: a volume's name, or a server path. An image's own volume has a name nobody chose. */
export function mountSource(mount: ContainerMount): string {
  if (mount.anonymous) return mount.volume.slice(0, 12);
  return mount.volume || mount.hostPath;
}

/** A name or an image that a filter finds, however it is written. */
export function matches(container: ServerContainer, filter: string): boolean {
  const wanted = filter.trim().toLowerCase();
  return [container.name, container.image, container.composeProject].some((text) =>
    text.toLowerCase().includes(wanted),
  );
}
