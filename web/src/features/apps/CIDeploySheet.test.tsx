// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  GitLabConnectionState,
  GitLabProjectConnectionSchema,
  GitLabProjectSchema,
  GitRepositorySchema,
  ImageBuildSchema,
  ResourceRefSchema,
  ResourceSchema,
  Role,
  RoleGrantSchema,
  ServiceInfoSchema,
  ServiceSchema,
} from "@dockwell/api";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReactNode } from "react";
import { describe, expect, it, vi } from "vitest";

vi.mock("@tanstack/react-router", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@tanstack/react-router")>();
  return {
    ...actual,
    Link: ({ to, children, className }: { to: string; children?: ReactNode; className?: string }) => (
      <a href={to} className={className}>
        {children}
      </a>
    ),
  };
});

import { authState } from "../../test/auth";
import { renderWithApi } from "../../test/render";
import { CIDeploySheet } from "./CIDeploySheet";

const serviceBase = (gitUrl?: string) =>
  create(ServiceInfoSchema, {
    id: "svc_1",
    resource: create(ResourceRefSchema, { kind: "service", scope: "env_1", name: "web" }),
    spec: create(ServiceSchema, {
      ...(gitUrl
        ? {
            build: create(ImageBuildSchema, {
              source: { case: "git", value: { url: gitUrl, ref: "main" } },
            }),
          }
        : {}),
    }),
  });

const envResource = create(ResourceSchema, {
  id: "env_1",
  resource: create(ResourceRefSchema, { kind: "environment", scope: "prj_a", name: "staging" }),
});

describe("CIDeploySheet N6 and N8 repository picker", () => {
  it("disables other repositories with 'An admin chooses this' for a developer when the app builds from Git", async () => {
    renderWithApi(<CIDeploySheet service={serviceBase("https://github.com/acme/alpha")} onClose={() => {}} />, {
      auth: {
        getAuthState: () =>
          authState({ grants: [create(RoleGrantSchema, { projectId: "prj_a", role: Role.DEVELOPER })] }),
      },
      resources: { listResources: () => ({ resources: [envResource] }) },
      gitProviders: {
        listGitRepositories: () => ({
          repositories: [
            create(GitRepositorySchema, {
              fullName: "acme/alpha",
              url: "https://github.com/acme/alpha",
              claimedByOtherProject: false,
            }),
            create(GitRepositorySchema, {
              fullName: "acme/beta",
              url: "https://github.com/acme/beta",
              claimedByOtherProject: true,
            }),
            create(GitRepositorySchema, {
              fullName: "acme/unclaimed",
              url: "https://github.com/acme/unclaimed",
              claimedByOtherProject: false,
            }),
          ],
        }),
      },
    });

    const select = await screen.findByLabelText("Status repository");
    const options = Array.from(select.querySelectorAll("option"));
    const alpha = options.find((o) => o.value === "https://github.com/acme/alpha");
    const beta = options.find((o) => o.value === "https://github.com/acme/beta");
    const unclaimed = options.find((o) => o.value === "https://github.com/acme/unclaimed");

    expect(alpha).not.toBeDisabled();
    expect(alpha?.textContent).toBe("acme/alpha");
    expect(beta).toBeDisabled();
    expect(beta?.textContent).toContain("An admin chooses this");
    expect(unclaimed).toBeDisabled();
    expect(unclaimed?.textContent).toContain("An admin chooses this");
    expect((select as HTMLSelectElement).value).toBe("https://github.com/acme/alpha");
  });

  it("allows unclaimed repos for a developer on an image-only app, while disabling repos claimed by another project", async () => {
    renderWithApi(<CIDeploySheet service={serviceBase()} onClose={() => {}} />, {
      auth: {
        getAuthState: () =>
          authState({ grants: [create(RoleGrantSchema, { projectId: "prj_a", role: Role.DEVELOPER })] }),
      },
      resources: { listResources: () => ({ resources: [envResource] }) },
      gitProviders: {
        listGitRepositories: () => ({
          repositories: [
            create(GitRepositorySchema, {
              fullName: "acme/beta",
              url: "https://github.com/acme/beta",
              claimedByOtherProject: true,
            }),
            create(GitRepositorySchema, {
              fullName: "acme/unclaimed",
              url: "https://github.com/acme/unclaimed",
              claimedByOtherProject: false,
            }),
          ],
        }),
      },
    });

    const select = await screen.findByLabelText("Status repository");
    const options = Array.from(select.querySelectorAll("option"));
    expect(options.find((o) => o.value === "https://github.com/acme/beta")).toBeDisabled();
    expect(options.find((o) => o.value === "https://github.com/acme/unclaimed")).not.toBeDisabled();
    expect((select as HTMLSelectElement).value).toBe("https://github.com/acme/unclaimed");
  });

  it("shows GitLab connection error state with Check and server search when hasMore is true", async () => {
    const user = userEvent.setup();
    const listProjects = vi.fn(({ query }: { query: string }) => ({
      connections: [
        create(GitLabProjectConnectionSchema, {
          id: "glc_bad",
          name: "broken-gl",
          url: "https://broken.gitlab.example.com",
          state: GitLabConnectionState.AUTH_FAILED,
        }),
        create(GitLabProjectConnectionSchema, {
          id: "glc_big",
          name: "big-gl",
          url: "https://gitlab.example.com",
          state: GitLabConnectionState.CONNECTED,
          hasMore: !query,
        }),
      ],
      projects: query
        ? [
            create(GitLabProjectSchema, {
              pathWithNamespace: "acme/svc-105",
              url: "https://gitlab.example.com/acme/svc-105",
            }),
          ]
        : [
            create(GitLabProjectSchema, {
              pathWithNamespace: "acme/svc-001",
              url: "https://gitlab.example.com/acme/svc-001",
            }),
          ],
    }));
    const checkConn = vi.fn(() => ({ state: GitLabConnectionState.AUTH_FAILED }));

    renderWithApi(<CIDeploySheet service={serviceBase()} onClose={() => {}} />, {
      auth: {
        getAuthState: () => authState({ grants: [create(RoleGrantSchema, { projectId: "", role: Role.ADMIN })] }),
      },
      resources: { listResources: () => ({ resources: [envResource] }) },
      gitProviders: {
        listGitLabProjects: listProjects,
        checkGitLabConnection: checkConn,
      },
    });

    await user.click(screen.getByRole("radio", { name: "GitLab CI" }));
    expect(await screen.findByText("broken-gl")).toBeInTheDocument();
    expect(screen.getByText("Token refused")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Check" }));
    await waitFor(() =>
      expect(checkConn).toHaveBeenCalledWith(expect.objectContaining({ name: "broken-gl" }), expect.anything()),
    );

    const search = screen.getByLabelText("Search GitLab projects");
    await user.type(search, "svc-105");
    await waitFor(() =>
      expect(listProjects).toHaveBeenCalledWith(expect.objectContaining({ query: "svc-105" }), expect.anything()),
    );
    expect(await screen.findByRole("option", { name: "acme/svc-105" })).toBeInTheDocument();
  });
});
