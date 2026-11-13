// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@connectrpc/connect-query";
import { ImageService, type RegistryLoginInfo } from "@dockwell/api";

/** The registries a person picks from, each with the host an image names it by. */
export type RegistryKind = "hub" | "ghcr" | "gitlab" | "ecr" | "other";

export const REGISTRY_ORDER: RegistryKind[] = ["hub", "ghcr", "gitlab", "ecr", "other"];

/** The hosts of the registries Dockwell knows by name. */
export const REGISTRY_HOSTS: Partial<Record<RegistryKind, string>> = {
  hub: "docker.io",
  ghcr: "ghcr.io",
  gitlab: "registry.gitlab.com",
};

/** Names a registry is known by, which no translation changes. */
export const REGISTRY_NAMES: Partial<Record<RegistryKind, string>> = {
  hub: "Docker Hub",
  ghcr: "GitHub Container Registry",
  gitlab: "GitLab",
  ecr: "Amazon ECR",
};

const ECR_HOST = /^[0-9]{12}\.dkr\.ecr\.([a-z0-9-]+)\.amazonaws\.com(\.cn)?$/;

/** The kind of registry a host is. */
export function registryKind(host: string): RegistryKind {
  for (const kind of REGISTRY_ORDER) if (REGISTRY_HOSTS[kind] === host) return kind;
  return ECR_HOST.test(host) ? "ecr" : "other";
}

/** The region of an Amazon ECR registry's host, or "" for another. */
export function ecrRegion(host: string): string {
  return ECR_HOST.exec(host)?.[1] ?? "";
}

/** The registry an image is on, as Docker reads its name: Docker Hub unless its first part is a host. */
export function registryOf(image: string): string {
  const name = image.trim().split("@")[0] ?? "";
  const slash = name.indexOf("/");
  if (slash < 0) return "docker.io";
  const first = name.slice(0, slash);
  if (!(first.includes(".") || first.includes(":") || first === "localhost")) return "docker.io";
  const host = first.toLowerCase();
  return host === "index.docker.io" || host === "registry-1.docker.io" ? "docker.io" : host;
}

/** A host as an image names it, from what was typed: no scheme, path or capitals. */
export function hostOf(typed: string): string {
  const host = typed
    .trim()
    .toLowerCase()
    .replace(/^https?:\/\//, "")
    .split("/")[0];
  return host ?? "";
}

/** A login's name, not yet taken: the registry's, then numbered. */
export function freeLoginName(base: string, taken: readonly RegistryLoginInfo[]): string {
  const clean =
    base
      .toLowerCase()
      .replace(/[^a-z0-9-]+/g, "-")
      .replace(/^-+|-+$/g, "")
      .slice(0, 40) || "registry";
  const start = /^[a-z]/.test(clean) ? clean : `r-${clean}`;
  const names = new Set(taken.map((l) => l.name));
  if (!names.has(start)) return start;
  for (let i = 2; ; i++) if (!names.has(`${start}-${i}`)) return `${start}-${i}`;
}

/** The base of a new login's name for a registry. */
export function loginBase(kind: RegistryKind, host: string): string {
  switch (kind) {
    case "hub":
      return "dockerhub";
    case "ghcr":
      return "ghcr";
    case "gitlab":
      return "gitlab";
    case "ecr":
      return `ecr-${ecrRegion(host) || "aws"}`;
  }
  return host.split(":")[0]?.split(".")[0] ?? "registry";
}

/**
 * The registry logins the signed-in person may see: every one for an admin, and
 * those their projects may use, by name and registry, below one. With a place,
 * those its project may use.
 */
export function useRegistryLogins(place?: { environment?: string; project?: string }, enabled = true) {
  return useQuery(
    ImageService.method.listRegistryLogins,
    { environment: place?.environment ?? "", project: place?.project ?? "" },
    { enabled, staleTime: 30_000 },
  );
}
