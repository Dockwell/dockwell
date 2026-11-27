// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { DeployOriginSchema } from "@dockwell/api";
import { screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";

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
});
