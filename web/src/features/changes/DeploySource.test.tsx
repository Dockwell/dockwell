// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  DeployOriginSchema,
  DeployProvider,
  DeployStatusReason,
  DeployStatusSchema,
  DeployStatusState,
  DeployStatusTransition,
} from "@dockwell/api";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { renderWithApi } from "../../test/render";
import { DeploySource } from "./DeploySource";

describe("CI deploy provenance", () => {
  it.each(["javascript:alert(1)", "http://example.com/commit", "https://user@example.com/commit"])(
    "renders %s without a link",
    (url) => {
      renderWithApi(
        <DeploySource
          origin={create(DeployOriginSchema, {
            commit: "abcdef123",
            ref: "refs/heads/main",
            commitUrl: url,
            runUrl: url,
          })}
        />,
        {},
      );
      expect(screen.getByText("From abcdef1 on main")).toBeInTheDocument();
      expect(screen.queryByRole("link")).toBeNull();
    },
  );

  it("links to the commit and run over HTTPS", () => {
    const commitUrl = "https://github.com/acme/web/commit/abcdef123";
    const runUrl = "https://github.com/acme/web/actions/runs/42";
    renderWithApi(
      <DeploySource
        origin={create(DeployOriginSchema, {
          commit: "abcdef123",
          ref: "refs/heads/main",
          commitUrl,
          runUrl,
        })}
      />,
      {},
    );
    expect(screen.getByRole("link", { name: "abcdef1" })).toHaveAttribute("href", commitUrl);
    expect(screen.getByRole("link", { name: "CI run" })).toHaveAttribute("href", runUrl);
  });

  it("shows a delivered GitHub status link and retries a failed status", async () => {
    const user = userEvent.setup();
    const retried = vi.fn(() => ({}));
    const onRetried = vi.fn();
    const origin = create(DeployOriginSchema, {
      commit: "abcdef123",
      ref: "refs/heads/main",
      provider: DeployProvider.GITHUB_ACTIONS,
    });
    const { rerender } = renderWithApi(
      <DeploySource
        origin={origin}
        changeId="chg_1"
        status={create(DeployStatusSchema, {
          provider: DeployProvider.GITHUB_ACTIONS,
          state: DeployStatusState.DELIVERED,
          transition: DeployStatusTransition.HEALTHY,
          remoteUrl: "https://github.com/acme/web/runs/501",
        })}
      />,
      { changes: { retryDeployStatus: retried } },
    );
    expect(screen.getByRole("link", { name: "Reported to GitHub" })).toHaveAttribute(
      "href",
      "https://github.com/acme/web/runs/501",
    );

    rerender(
      <DeploySource
        origin={origin}
        changeId="chg_1"
        onRetried={onRetried}
        status={create(DeployStatusSchema, {
          provider: DeployProvider.GITHUB_ACTIONS,
          state: DeployStatusState.FAILED,
          transition: DeployStatusTransition.HEALTHY,
          reason: DeployStatusReason.MISSING_CHECKS_PERMISSION,
          actionUrl: "https://github.com/organizations/acme/settings/apps/dockwell/permissions",
          error: "GitHub answered 403",
          canRetry: true,
        })}
      />,
    );
    expect(screen.getByText("Not reported: GitHub App needs Checks permission")).toBeVisible();
    expect(screen.getByRole("link", { name: "Grant on GitHub" })).toHaveAttribute(
      "href",
      "https://github.com/organizations/acme/settings/apps/dockwell/permissions",
    );
    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() =>
      expect(retried).toHaveBeenCalledWith(expect.objectContaining({ id: "chg_1" }), expect.anything()),
    );
    expect(onRetried).toHaveBeenCalledTimes(1);

    // H1: Repository mismatch shows token_repository only when populated by server.
    rerender(
      <DeploySource
        origin={origin}
        changeId="chg_2"
        status={create(DeployStatusSchema, {
          provider: DeployProvider.GITHUB_ACTIONS,
          state: DeployStatusState.SKIPPED,
          transition: DeployStatusTransition.HEALTHY,
          reason: DeployStatusReason.REPOSITORY_MISMATCH,
          tokenRepository: "https://github.com/acme/web",
        })}
      />,
    );
    expect(screen.getByText("Not reported: this token reports to github.com/acme/web")).toBeVisible();

    rerender(
      <DeploySource
        origin={origin}
        changeId="chg_2"
        status={create(DeployStatusSchema, {
          provider: DeployProvider.GITHUB_ACTIONS,
          state: DeployStatusState.SKIPPED,
          transition: DeployStatusTransition.HEALTHY,
          reason: DeployStatusReason.REPOSITORY_MISMATCH,
        })}
      />,
    );
    expect(screen.getByText("Not reported: repository doesn't match token")).toBeVisible();
  });
});
