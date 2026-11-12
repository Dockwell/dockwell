// SPDX-License-Identifier: AGPL-3.0-only

import type { Resource, ServiceInfo } from "@dockwell/api";
import { Button, Panel } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import type { ReactNode } from "react";

import { nameOf } from "./app";
import { useDeleteApp } from "./deleteApp";
import "./Apps.css";

/**
 * Deleting the app, as the mock's danger zone has it: what goes, and a red button to go on. The last
 * app in a project takes the project with it; one of several offers the whole project instead (ADR 0074).
 */
export function DangerZone({
  service,
  title,
  after,
  alongside,
  stays,
}: {
  service: ServiceInfo;
  title: string;
  /** Where to go once it's deleted: the list it was on. */
  after?: "/apps" | "/databases";
  /** What goes with it, as a database's users go with its service. */
  alongside?: readonly Resource[];
  /** What deleting it alone keeps, in place of its volumes staying, as a database says of its data. */
  stays?: ReactNode;
}) {
  const { t } = useLingui();
  const remove = useDeleteApp(service, { ...(after ? { after } : {}), ...(alongside ? { alongside } : {}) });
  const name = nameOf(service);
  const project = remove.project;
  const withProject = project?.last && !project.refused;
  return (
    <Panel title={title} className="app-danger">
      <div className="app-danger-row">
        <div className="app-danger-text">
          <span className="app-danger-title">{t`Delete ${name}`}</span>
          <span className="app-danger-what">
            {withProject && project.left ? (
              <Trans>
                Stops it and removes its container. Its project {project.label} goes too, with {project.left}. You
                review a plan first.
              </Trans>
            ) : withProject ? (
              <Trans>
                Stops it and removes its container. Its project {project.label} goes too. You review a plan first.
              </Trans>
            ) : stays ? (
              stays
            ) : remove.routes > 0 ? (
              <Trans>
                Stops it and removes its container, and its domains go with it. Its volumes stay, and you review a plan
                first.
              </Trans>
            ) : (
              <Trans>Stops it and removes its container. Its volumes stay, and you review a plan first.</Trans>
            )}
          </span>
        </div>
        <Button variant="danger" icon="trash" onClick={remove.open}>
          {t`Delete ${name}`}
        </Button>
      </div>
      {project && !project.last ? (
        <div className="app-danger-row">
          <div className="app-danger-text">
            <span className="app-danger-title">{t`Delete the project ${project.label}`}</span>
            <span className="app-danger-what">
              <Trans>Takes {project.holds} in one plan, to review first.</Trans>
            </span>
          </div>
          <Button variant="danger" icon="trash" onClick={project.open} off={project.refused}>
            {t`Delete the project`}
          </Button>
        </div>
      ) : null}
    </Panel>
  );
}
