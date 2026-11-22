// SPDX-License-Identifier: AGPL-3.0-only

import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it, vi } from "vitest";

import { renderAt } from "../../test/render";

const project = { id: "prj_templates", resource: { kind: "project", name: "templates" } };
const template = { id: "cpt_web", name: "example-web", projectId: project.id, revision: 2n };

it("uses a compact search and the shared row link in the template library", async () => {
  renderAt("/apps/templates?project=prj_templates", {
    resources: { listResources: () => ({ resources: [project] }) },
    compose: { listComposeTemplates: () => ({ templates: [template] }) },
  });
  expect(await screen.findByRole("link", { name: "example-web" })).toHaveClass("compose-row-title");
  expect(screen.getByRole("textbox", { name: "Find a template" })).toBeVisible();
  expect(screen.queryByRole("combobox", { name: "Project" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "New template" })).toBeVisible();
});

it("opens the exact historical revision from the History table", async () => {
  renderAt("/apps/templates?project=prj_templates&template=cpt_web", {
    resources: { listResources: () => ({ resources: [project] }) },
    compose: {
      getComposeTemplate: ({ revision }) => ({
        template: { ...template, revision: revision || template.revision },
        spec: { composeYaml: "services: {}" },
        history: [{ revision: 2n }, { revision: 1n }],
      }),
    },
  });
  expect(await screen.findByRole("heading", { name: "example-web" })).toBeVisible();
  expect(screen.getByRole("navigation", { name: "Breadcrumb" })).toHaveTextContent("templates/Compose templates");
  expect(screen.getByRole("button", { name: "Template actions" })).toBeVisible();
  const old = screen.getByRole("button", { name: "Open revision 1" }).closest("tr");
  if (!old) throw new Error("Revision 1 is missing from History");
  await userEvent.click(within(old).getByRole("button", { name: "Use this revision" }));
  expect(await screen.findByRole("dialog", { name: "Use Compose template" })).toBeVisible();
  expect(screen.getByLabelText("Revision")).toHaveValue("1");
});

it("reviews deletion of the selected template at its current revision", async () => {
  const propose = vi.fn();
  renderAt("/apps/templates?project=prj_templates&template=cpt_web", {
    resources: { listResources: () => ({ resources: [project] }) },
    compose: {
      getComposeTemplate: () => ({ template, spec: { composeYaml: "services: {}" } }),
    },
    changes: {
      proposeChange: (request) => {
        propose(request.edits[0]);
        return {};
      },
    },
  });
  await userEvent.click(await screen.findByRole("button", { name: "Template actions" }));
  await userEvent.click(screen.getByRole("menuitem", { name: "Delete template" }));
  await waitFor(() =>
    expect(propose).toHaveBeenCalledWith(
      expect.objectContaining({
        resource: expect.objectContaining({ kind: "compose.template", scope: project.id, name: template.name }),
        baseRevision: template.revision,
        delete: true,
      }),
    ),
  );
});
