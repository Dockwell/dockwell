// SPDX-License-Identifier: AGPL-3.0-only

import type { CatalogInstall, Domain, Resource, ServiceInfo } from "@dockwell/api";

/** The install's first project, where apps with no project live. */
export const DEFAULT_PROJECT = "default";

/** Whether a project is proven running, proven idle, still loading, or unknown because a read failed. */
export type ProjectRunningState = "running" | "idle" | "loading" | "unknown";

/** The identity and running state a project carries across Projects and Variables. */
export interface ProjectSummary {
  project: Resource;
  id: string;
  name: string;
  /** Its display name, or its name; a catalog install goes by the name the person gave it (#796). */
  label: string;
  /** Where apps with no project go: the install's first, named default. */
  isDefault: boolean;
  /** Running state derived from the project's services and routes. */
  runningState: ProjectRunningState;
  /**
   * Holds no service and no route: true when confirmed idle, false when something runs,
   * undefined while loading or when a read failed.
   */
  idle: boolean | undefined;
}

/** How a project is named on screen: an install keeps the name given to it, others use displayName first (#796). */
export function projectLabel(
  project: Resource,
  installs?: readonly Pick<CatalogInstall, "projectId">[] | boolean | undefined,
): string {
  const name = project.resource?.name || project.id;
  const isInstall =
    typeof installs === "boolean" ? installs : (installs?.some((item) => item.projectId === project.id) ?? false);
  return isInstall ? name : project.displayName || name;
}

/** Whether this is the hidden default project where apps without a project live. */
export function isDefaultProject(project: Resource): boolean {
  return (project.resource?.name ?? project.id) === DEFAULT_PROJECT;
}

/** Evidence of what runs in a project, either from its listed resources or from live services and domains queries. */
export type ProjectEvidence =
  | {
      /** Resources inside the project's environments (from ListResources). */
      resources: readonly Resource[];
    }
  | {
      /** The project's own environment IDs. */
      environmentIds: ReadonlySet<string> | readonly string[];
      /** Services from useServices(), or undefined while loading or failed. */
      services: readonly Pick<ServiceInfo, "resource">[] | undefined;
      servicesFailed?: boolean;
      /** Domains from listDomains(), or undefined while loading or failed. */
      domains: readonly Pick<Domain, "environmentId" | "routes">[] | undefined;
      domainsFailed?: boolean;
    };

/**
 * Classifies whether a project has anything running.
 * Any confirmed service or route makes it running; confirming both absent makes it idle;
 * a failed read leaves it unknown; otherwise it is still loading.
 */
export function projectRunningState(evidence: {
  hasService: boolean | undefined;
  hasRoute: boolean | undefined;
  failed?: boolean;
}): ProjectRunningState {
  if (evidence.hasService === true || evidence.hasRoute === true) return "running";
  if (evidence.hasService === false && evidence.hasRoute === false) return "idle";
  return evidence.failed ? "unknown" : "loading";
}

function resolveEvidence(evidence: ProjectEvidence): ProjectRunningState {
  if ("resources" in evidence) {
    return projectRunningState({
      hasService: evidence.resources.some((r) => r.resource?.kind === "service"),
      hasRoute: evidence.resources.some((r) => r.resource?.kind === "route"),
    });
  }
  const envIds = evidence.environmentIds instanceof Set ? evidence.environmentIds : new Set(evidence.environmentIds);
  const hasService =
    evidence.services !== undefined && !evidence.servicesFailed
      ? evidence.services.some((service) => envIds.has(service.resource?.scope ?? ""))
      : undefined;
  const hasRoute =
    evidence.domains !== undefined && !evidence.domainsFailed
      ? evidence.domains.some((domain) => domain.routes.length > 0 && envIds.has(domain.environmentId))
      : undefined;
  return projectRunningState({
    hasService,
    hasRoute,
    failed: Boolean(evidence.servicesFailed || evidence.domainsFailed),
  });
}

export function projectSummary(
  project: Resource,
  options: {
    installs?: readonly Pick<CatalogInstall, "projectId">[] | undefined;
    resources: readonly Resource[];
  },
): ProjectSummary & { idle: boolean; runningState: "running" | "idle" };
export function projectSummary(
  project: Resource,
  options: {
    installs?: readonly Pick<CatalogInstall, "projectId">[] | undefined;
  } & ProjectEvidence,
): ProjectSummary;
export function projectSummary(
  project: Resource,
  options: {
    installs?: readonly Pick<CatalogInstall, "projectId">[] | undefined;
  } & ProjectEvidence,
): ProjectSummary {
  const name = project.resource?.name ?? project.id;
  const runningState = resolveEvidence(options);
  return {
    project,
    id: project.id,
    name,
    label: projectLabel(project, options.installs),
    isDefault: isDefaultProject(project),
    runningState,
    idle: runningState === "idle" ? true : runningState === "running" ? false : undefined,
  };
}

const collator = new Intl.Collator(undefined, { sensitivity: "base", numeric: true });

/** Default project first, then natural alphabetical order by label. */
export function compareProjects(
  a: Pick<ProjectSummary, "isDefault" | "label">,
  b: Pick<ProjectSummary, "isDefault" | "label">,
): number {
  return a.isDefault !== b.isDefault ? (a.isDefault ? -1 : 1) : collator.compare(a.label, b.label);
}
