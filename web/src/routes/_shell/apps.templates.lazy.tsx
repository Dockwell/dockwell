// SPDX-License-Identifier: AGPL-3.0-only

import { createLazyFileRoute } from "@tanstack/react-router";

import { ComposeTemplates } from "../../features/docker/ComposeTemplates";

export const Route = createLazyFileRoute("/_shell/apps/templates")({
  component: function TemplatesPage() {
    const { project, template } = Route.useSearch();
    return <ComposeTemplates projectId={project} templateId={template} key={template ?? project} />;
  },
});
