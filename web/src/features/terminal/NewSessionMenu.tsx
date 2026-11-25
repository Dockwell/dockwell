// SPDX-License-Identifier: AGPL-3.0-only

import { createClient } from "@connectrpc/connect";
import { useTransport } from "@connectrpc/connect-query";
import { ContainerState, DockerObjectKind, DockerService, type Server } from "@dockwell/api";
import { Button, Icon, Loading, Skeleton, TextField } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQuery } from "@tanstack/react-query";
import { type KeyboardEvent, useEffect, useMemo, useRef, useState } from "react";

import { useMay } from "../auth/may";

/** Show a search field inside the + picker once there are at least this many running containers. */
export const CONTAINER_SEARCH_THRESHOLD = 5;

const PICKER_PAGE_SIZE = 100;
const PICKER_MAX_PAGES = 10;

export interface ContainerShellTarget {
  id: string;
  name: string;
}

export interface NewSessionMenuProps {
  server: Server;
  open: boolean;
  opening: boolean;
  onOpenChange: (open: boolean) => void;
  onOpenHost: () => void;
  onOpenContainer: (target: ContainerShellTarget) => void;
  /** Align menu to the left (desktop +) or right (phone bar). */
  align?: "start" | "end";
}

interface ListedContainer {
  id: string;
  name: string;
  image: string;
}

/**
 * The + picker: host shell first, then this server's running containers with
 * name, image, and search when there are many. Selecting a container opens its
 * shell through the same exact containerId path as Docker -> Containers.
 */
export function NewSessionMenu({
  server,
  open,
  opening,
  onOpenChange,
  onOpenHost,
  onOpenContainer,
  align = "start",
}: NewSessionMenuProps) {
  const { t } = useLingui();
  const may = useMay();
  const transport = useTransport();
  const dockerClient = useMemo(() => createClient(DockerService, transport), [transport]);
  const canListDocker = may(DockerService.method.listDockerObjects).allowed;
  const [search, setSearch] = useState("");
  const root = useRef<HTMLSpanElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const trimmedSearch = search.trim();

  // Page through running containers and forward search to the server so servers
  // with more than 100 running containers remain completely searchable.
  const containersQuery = useQuery({
    queryKey: ["terminal-picker-containers", server.id, trimmedSearch],
    enabled: open && canListDocker,
    staleTime: 0,
    refetchOnMount: "always",
    queryFn: async ({ signal }) => {
      const seen = new Set<string>();
      const collected: ListedContainer[] = [];
      let unavailable = false;
      let pageToken = "";
      let hasMore = false;
      for (let page = 0; page < PICKER_MAX_PAGES; page++) {
        const res = await dockerClient.listDockerObjects(
          {
            serverIds: [server.id],
            kind: DockerObjectKind.CONTAINER,
            filter: {
              state: ContainerState.RUNNING,
              ...(trimmedSearch ? { search: trimmedSearch } : {}),
            },
            pageSize: PICKER_PAGE_SIZE,
            ...(pageToken ? { pageToken } : {}),
          },
          { signal },
        );
        if ((res.unavailable?.length ?? 0) > 0) unavailable = true;
        for (const obj of res.objects ?? []) {
          const engineId = obj.key?.engineId ?? "";
          if (obj.state !== ContainerState.RUNNING || !engineId || seen.has(engineId)) continue;
          seen.add(engineId);
          collected.push({
            id: engineId,
            name: obj.name || engineId.slice(0, 12),
            image: obj.detail.case === "container" ? obj.detail.value.image : "",
          });
        }
        if (!res.nextPageToken) {
          hasMore = false;
          break;
        }
        pageToken = res.nextPageToken;
        hasMore = true;
      }
      collected.sort((a, b) => a.name.localeCompare(b.name));
      return { containers: collected, unavailable, hasMore };
    },
  });

  useEffect(() => {
    if (!open) {
      setSearch("");
      return;
    }
    const first = root.current?.querySelector<HTMLButtonElement>("[data-picker-item]");
    first?.focus();
    const away = (e: PointerEvent) => {
      if (!root.current?.contains(e.target as Node)) onOpenChange(false);
    };
    document.addEventListener("pointerdown", away);
    return () => document.removeEventListener("pointerdown", away);
  }, [open, onOpenChange]);

  const containers = containersQuery.data?.containers ?? [];
  const hasMore = containersQuery.data?.hasMore ?? false;
  const showSearch = containers.length >= CONTAINER_SEARCH_THRESHOLD || hasMore || search.length > 0;
  const filtered = useMemo(() => {
    const q = trimmedSearch.toLowerCase();
    if (!q) return containers;
    return containers.filter((c) => c.name.toLowerCase().includes(q) || c.image.toLowerCase().includes(q));
  }, [containers, trimmedSearch]);

  const unavailable = containersQuery.data?.unavailable ?? false;

  const onMenuKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (e.key === "Escape") {
      e.preventDefault();
      e.stopPropagation();
      onOpenChange(false);
      trigger.current?.focus();
      return;
    }
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    if ((e.target as HTMLElement)?.tagName === "INPUT" && e.key === "ArrowUp") return;
    const items = [...(root.current?.querySelectorAll<HTMLButtonElement>("[data-picker-item]") ?? [])];
    if (items.length === 0) return;
    e.preventDefault();
    const at = items.indexOf(document.activeElement as HTMLButtonElement);
    const step = e.key === "ArrowDown" ? 1 : -1;
    const next = at < 0 ? 0 : (at + step + items.length) % items.length;
    items[next]?.focus();
  };

  return (
    <span className="terminal-menu-anchor" ref={root}>
      <Button
        ref={trigger}
        variant="ghost"
        size="sm"
        icon="plus"
        aria-label={t`New session`}
        aria-haspopup="dialog"
        aria-expanded={open}
        loading={opening}
        onClick={() => onOpenChange(!open)}
      />
      {open ? (
        <div
          className={
            align === "end"
              ? "terminal-menu terminal-picker terminal-picker-end"
              : "terminal-menu terminal-picker terminal-picker-start"
          }
          role="dialog"
          aria-label={t`New session`}
          onKeyDown={onMenuKeyDown}
        >
          <button
            type="button"
            data-picker-item=""
            className="terminal-picker-item"
            onClick={() => {
              onOpenChange(false);
              onOpenHost();
            }}
          >
            <span className="terminal-picker-icon">
              <Icon name="server" size={14} />
            </span>
            <span className="terminal-picker-copy">
              <span className="terminal-picker-title">
                <Trans>Host shell</Trans>
              </span>
              <span className="terminal-picker-sub">{server.name}</span>
            </span>
          </button>

          {canListDocker ? (
            <>
              <hr className="terminal-picker-sep" />
              <div className="terminal-picker-head">
                <span>
                  <Trans>Running containers</Trans>
                </span>
                {containers.length > 0 ? (
                  <span className="terminal-picker-count">{hasMore ? `${containers.length}+` : containers.length}</span>
                ) : null}
              </div>
              {showSearch ? (
                <div className="terminal-picker-search">
                  <TextField
                    size="sm"
                    value={search}
                    aria-label={t`Filter containers`}
                    placeholder={t`Filter by name or image`}
                    onChange={(e) => setSearch(e.target.value)}
                  />
                </div>
              ) : null}
              <div className="terminal-picker-list">
                {containersQuery.isLoading ? (
                  <Loading label={t`Loading running containers…`} className="terminal-picker-note">
                    <Skeleton width="65%" />
                    <Skeleton width="45%" text="code-sm" />
                  </Loading>
                ) : unavailable || containersQuery.isError ? (
                  <div className="terminal-picker-note">
                    <Trans>Docker isn't reachable on {server.name}.</Trans>
                  </div>
                ) : filtered.length === 0 ? (
                  <div className="terminal-picker-note">
                    {trimmedSearch ? (
                      <Trans>No running container matches "{trimmedSearch}".</Trans>
                    ) : (
                      <Trans>No running containers on {server.name}.</Trans>
                    )}
                  </div>
                ) : (
                  filtered.map((ctr) => (
                    <button
                      key={ctr.id}
                      type="button"
                      data-picker-item=""
                      className="terminal-picker-item"
                      onClick={() => {
                        onOpenChange(false);
                        onOpenContainer({ id: ctr.id, name: ctr.name });
                      }}
                    >
                      <span className="terminal-picker-icon">
                        <Icon name="service" size={14} />
                      </span>
                      <span className="terminal-picker-copy">
                        <span className="terminal-picker-title">{ctr.name}</span>
                        {ctr.image ? <code className="terminal-picker-sub">{ctr.image}</code> : null}
                      </span>
                    </button>
                  ))
                )}
              </div>
            </>
          ) : null}
        </div>
      ) : null}
    </span>
  );
}
