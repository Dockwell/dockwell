// SPDX-License-Identifier: AGPL-3.0-only

import { createFileRoute } from "@tanstack/react-router";

import { ProjectPage } from "../../features/projects/ProjectPage";

/** One project: what's in it, its variables and secrets, who may reach it, and its settings (ADR 0074). */
export const Route = createFileRoute("/_shell/projects/$projectId")({
  component: function Project() {
    const { projectId } = Route.useParams();
    return <ProjectPage projectId={projectId} />;
  },
});
