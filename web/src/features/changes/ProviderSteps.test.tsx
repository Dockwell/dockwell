// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { PlanSchema, ProviderAction, StepProgressSchema, StepState } from "@dockwell/api";
import { screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { renderWithApi } from "../../test/render";
import { PlanSteps } from "./PlanSteps";
import { CloudflareSteps } from "./ProviderSteps";

const plan = create(PlanSchema, {
  servers: [
    {
      serverId: "srv_1",
      serverName: "hz-1",
      steps: [
        {
          resourceId: "rte_1:cf:1",
          resource: { kind: "route", name: "wiki" },
          provider: {
            provider: "cloudflare",
            action: ProviderAction.CREATE_RECORD,
            target: "example.com",
            name: "wiki.example.com",
            to: "CNAME to web-1's tunnel, proxied",
          },
        },
        {
          resourceId: "rte_1:cf:2",
          resource: { kind: "route", name: "wiki" },
          provider: {
            provider: "cloudflare",
            action: ProviderAction.ADD_RULE,
            target: "web-1",
            name: "*.example.com",
            to: "http://127.0.0.1:80",
            routing: "Every other name in web-1, the panel's among them, goes where it went",
          },
        },
        { resourceId: "rte_1", resource: { kind: "route", name: "wiki" } },
      ],
    },
  ],
});

describe("steps at Cloudflare", () => {
  it("lists what a plan writes at Cloudflare, in the order it runs, with what it writes and what else it moves", () => {
    renderWithApi(<CloudflareSteps plan={plan} />, {});
    const section = screen.getByRole("region", { name: "At Cloudflare" });
    const rows = within(section).getAllByRole("listitem");
    expect(rows).toHaveLength(2);
    expect(rows[0]).toHaveTextContent("Write the DNS record for wiki.example.com at Cloudflare");
    expect(rows[0]).toHaveTextContent("Writes CNAME to web-1's tunnel, proxied");
    expect(rows[1]).toHaveTextContent("Add *.example.com to the tunnel web-1 at Cloudflare");
    expect(rows[1]).toHaveTextContent("Every other name in web-1, the panel's among them, goes where it went");
  });

  it("says nothing for a plan that writes nothing there", () => {
    const { container } = renderWithApi(<CloudflareSteps plan={create(PlanSchema, {})} />, {});
    expect(container).toBeEmptyDOMElement();
  });

  it("says what a step at Cloudflare did once it's done, in its own words rather than its route's", () => {
    renderWithApi(
      <PlanSteps
        plan={plan}
        progress={[create(StepProgressSchema, { serverId: "srv_1", resourceId: "rte_1:cf:1", state: StepState.DONE })]}
      />,
      {},
    );
    expect(screen.getByText("Wrote the DNS record for wiki.example.com at Cloudflare")).toBeVisible();
  });
});
