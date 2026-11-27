// SPDX-License-Identifier: AGPL-3.0-only

import type { CatalogInstall, Resource } from "@dockwell/api";
import { DEFAULT_PROJECT } from "../apps/useServices";

/** The identity and running state a project carries across Projects and Variables. */
export interface ProjectSummary {
  project: Resource;
  id: string;
  name: string;
  /** Its display name, or its name; a catalog install goes by the name the person gave it (#796). */
  label: string;
  /** Where apps with no project go: the install's first, named default. */
  isDefault: boolean;
  /**
   * Holds no service and no route: true when confirmed idle, false when something runs,
   * undefined when what runs isn't known (such as while services or domains haven't loaded or failed).
   */
  idle: boolean | undefined;
}

/** How a project is named on screen: an install keeps the name given to it, others use displayName first. */
export function projectLabel(project: Resource, isInstall: boolean): string {
  const name = project.resource?.name ?? project.id;
  return isInstall ? name : project.displayName || name;
}

/** Whether this is the hidden default project where apps without a project live. */
export function isDefaultProject(project: Resource): boolean {
  return (project.resource?.name ?? project.id) === DEFAULT_PROJECT;
}

/**
 * Whether a project has nothing running in it. Any confirmed service or route makes it running (false),
 * confirming both absent makes it idle (true), and missing/failed evidence leaves it unknown (undefined).
 */
export function projectIdle(hasService: boolean, hasRoute: boolean): boolean;
export function projectIdle(hasService: boolean | undefined, hasRoute: boolean | undefined): boolean | undefined;
export function projectIdle(hasService: boolean | undefined, hasRoute: boolean | undefined): boolean | undefined {
  if (hasService === true || hasRoute === true) return false;
  if (hasService === false && hasRoute === false) return true;
  return undefined;
}

export function projectSummary(
  project: Resource,
  options: {
    installs?: readonly Pick<CatalogInstall, "projectId">[] | undefined;
    hasService: boolean;
    hasRoute: boolean;
  },
): ProjectSummary & { idle: boolean };
export function projectSummary(
  project: Resource,
  options: {
    installs?: readonly Pick<CatalogInstall, "projectId">[] | undefined;
    hasService: boolean | undefined;
    hasRoute: boolean | undefined;
  },
): ProjectSummary;
export function projectSummary(
  project: Resource,
  options: {
    installs?: readonly Pick<CatalogInstall, "projectId">[] | undefined;
    hasService: boolean | undefined;
    hasRoute: boolean | undefined;
  },
): ProjectSummary {
  const name = project.resource?.name ?? project.id;
  const isInstall = options.installs?.some((item) => item.projectId === project.id) ?? false;
  return {
    project,
    id: project.id,
    name,
    label: projectLabel(project, isInstall),
    isDefault: isDefaultProject(project),
    idle: projectIdle(options.hasService, options.hasRoute),
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
