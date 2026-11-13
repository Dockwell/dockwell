// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  ChangeSchema,
  ChangeState,
  type CheckRegistryLoginRequest,
  type ProposeChangeRequest,
  RegistryLoginInfoSchema,
  RegistryLoginState,
  type WatchChangeResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { feed, renderAt } from "../../test/render";
import { freeLoginName, registryKind, registryOf } from "./registries";

const ghcr = create(RegistryLoginInfoSchema, {
  id: "rgl_1",
  name: "ghcr-ci",
  registry: "ghcr.io",
  username: "ci",
  tokenHost: "ghcr.io",
});

afterEach(async () => {
  await activateLocale("en");
});

function planned() {
  return vi.fn((req: ProposeChangeRequest) => {
    void req;
    return {
      change: create(ChangeSchema, { id: "chg_login", state: ChangeState.PLANNED, plan: { fingerprint: "abc" } }),
    };
  });
}

describe("Registry logins", () => {
  it("lists each login with its registry, and offers Docker Hub and GitHub's until they're connected", async () => {
    const { container } = renderAt("/settings/integrations", {
      backups: { listBackupDestinations: () => ({ destinations: [] }) },
      images: { listRegistryLogins: () => ({ logins: [ghcr] }) },
    });
    const section = await screen.findByRole("region", { name: "Registries" });
    expect(await within(section).findByText("1 login · for images a registry keeps private")).toBeVisible();
    const card = within(section).getByRole("article", { name: "GitHub Container Registry" });
    expect(within(card).getByText("Connected")).toBeVisible();
    expect(card).toHaveTextContent("ghcr-ci for every project");
    expect(within(section).getByRole("article", { name: "Docker Hub" })).toHaveTextContent("Not connected");
    expect(screen.getByText("Git providers and cloud providers come later.")).toBeVisible();
    await expectAccessible(container);
  });

  it("checks a login as it's typed, and plans it with the token service the check found", async () => {
    const user = userEvent.setup();
    const checks: CheckRegistryLoginRequest[] = [];
    const proposed = planned();
    renderAt("/settings/integrations", {
      backups: { listBackupDestinations: () => ({ destinations: [] }) },
      images: {
        listRegistryLogins: () => ({ logins: [] }),
        checkRegistryLogin: (req) => {
          checks.push(req);
          if (req.probe) return { state: RegistryLoginState.ASKS, tokenHost: "auth.docker.io", registry: "docker.io" };
          return req.password === "right-token"
            ? { state: RegistryLoginState.SIGNED_IN, tokenHost: "auth.docker.io", registry: "docker.io" }
            : { state: RegistryLoginState.REFUSED, registry: "docker.io" };
        },
      },
      changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
    });
    const hub = await screen.findByRole("article", { name: "Docker Hub" });
    await user.click(within(hub).getByRole("button", { name: "Connect" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a registry login" });
    expect(within(sheet).getByRole("combobox", { name: "Registry" })).toHaveValue("hub");
    const review = within(sheet).getByRole("button", { name: "Review plan" });
    expect(review).toBeDisabled();

    await user.type(within(sheet).getByRole("textbox", { name: "Username" }), "acme");
    await user.type(within(sheet).getByLabelText("Token"), "wrong");
    expect(await within(sheet).findByText("Wrong username or password", {}, { timeout: 3000 })).toBeVisible();
    expect(review).toBeDisabled();
    await user.clear(within(sheet).getByLabelText("Token"));
    await user.type(within(sheet).getByLabelText("Token"), "right-token");
    expect(await within(sheet).findByText("Signed in", {}, { timeout: 3000 })).toBeVisible();
    expect(checks.at(-1)).toMatchObject({ registry: "docker.io", username: "acme", password: "right-token" });
    // The registry was asked with no login first, and nothing that signs in went with that.
    expect(checks[0]).toMatchObject({ registry: "docker.io", probe: true, password: "", username: "" });

    await user.click(review);
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource).toMatchObject({ kind: "registry.login", name: "dockerhub" });
    expect(edit?.spec?.kind.value).toMatchObject({
      registry: "docker.io",
      username: "acme",
      password: "right-token",
      tokenHost: "auth.docker.io",
      projects: [],
    });
  });

  it("changes which projects use a login without asking for its token again", async () => {
    const user = userEvent.setup();
    const proposed = planned();
    const check = vi.fn((req: CheckRegistryLoginRequest) => ({
      state: req.probe ? RegistryLoginState.ASKS : RegistryLoginState.SIGNED_IN,
    }));
    renderAt("/settings/integrations", {
      backups: { listBackupDestinations: () => ({ destinations: [] }) },
      images: { listRegistryLogins: () => ({ logins: [ghcr] }), checkRegistryLogin: check },
      resources: {
        listResources: () => ({
          resources: [
            { id: "prj_shop", resource: { kind: "project", name: "shop" } },
            { id: "prj_blog", resource: { kind: "project", name: "blog" } },
          ],
        }),
      },
      changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
    });
    const card = await screen.findByRole("article", { name: "GitHub Container Registry" });
    await user.click(within(card).getByRole("button", { name: "Manage" }));
    const sheet = await screen.findByRole("dialog", { name: "Registry login ghcr-ci" });
    expect(within(sheet).getByLabelText("Token")).toHaveAttribute("placeholder", "Kept unless you type one");
    await user.click(within(sheet).getByText(/Named ghcr-ci · every project/));
    await user.click(within(sheet).getByRole("switch", { name: "Only some projects" }));
    await user.click(await within(sheet).findByRole("checkbox", { name: "shop" }));
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.spec?.kind.value).toMatchObject({
      registry: "ghcr.io",
      password: "",
      tokenHost: "ghcr.io",
      projects: ["prj_shop"],
    });
    // Nothing that signs in changed, so nothing that signs in was sent.
    expect(check.mock.calls.every(([req]) => req.probe && req.password === "")).toBe(true);
  });

  it("asks nothing of a host half typed, and nothing that signs in of one that isn't a registry", async () => {
    const user = userEvent.setup();
    const checks: CheckRegistryLoginRequest[] = [];
    renderAt("/settings/integrations", {
      backups: { listBackupDestinations: () => ({ destinations: [] }) },
      images: {
        listRegistryLogins: () => ({ logins: [] }),
        checkRegistryLogin: (req) => {
          checks.push(req);
          if (req.registry === "registry.example.com") return { state: RegistryLoginState.ASKS };
          return { state: RegistryLoginState.UNREACHABLE, detail: "it answered 404, as no registry does" };
        },
      },
    });
    await user.click(await screen.findByRole("button", { name: "Connect a registry" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a registry login" });
    await user.selectOptions(within(sheet).getByRole("combobox", { name: "Registry" }), "other");
    // Docker Hub, where the sheet opened, was asked with no login: what follows is the host's.
    await waitFor(() => expect(checks.every((c) => c.probe && c.password === "")).toBe(true));
    checks.length = 0;
    await user.type(within(sheet).getByRole("textbox", { name: "Username" }), "ci");
    await user.type(within(sheet).getByLabelText("Token"), "a-typed-token");
    // Typing the host asks nothing of what's typed so far.
    await user.type(within(sheet).getByRole("textbox", { name: "Host" }), "registry.ex");
    await new Promise((resolve) => setTimeout(resolve, 900));
    expect(checks).toHaveLength(0);
    // Left as a host that isn't a registry: it's asked with no login, and gets no token.
    await user.tab();
    expect(await within(sheet).findByText("Not a registry, or can't reach it")).toBeVisible();
    await new Promise((resolve) => setTimeout(resolve, 900));
    expect(checks.map((c) => [c.registry, c.probe, c.password])).toEqual([["registry.ex", true, ""]]);
    // Typed whole, it's a registry, and the token was typed for another: checked on asking.
    await user.type(within(sheet).getByRole("textbox", { name: "Host" }), "ample.com{Enter}");
    const check = await within(sheet).findByRole("button", { name: "Check with registry.example.com" });
    expect(checks.some((c) => c.password !== "")).toBe(false);
    await user.click(check);
    await waitFor(() =>
      expect(checks.at(-1)).toMatchObject({ registry: "registry.example.com", password: "a-typed-token" }),
    );
  });

  it("checks a token again only when asked once its registry changed", async () => {
    const user = userEvent.setup();
    const checks: CheckRegistryLoginRequest[] = [];
    renderAt("/settings/integrations", {
      backups: { listBackupDestinations: () => ({ destinations: [] }) },
      images: {
        listRegistryLogins: () => ({ logins: [] }),
        checkRegistryLogin: (req) => {
          checks.push(req);
          return { state: req.probe ? RegistryLoginState.ASKS : RegistryLoginState.SIGNED_IN };
        },
      },
    });
    const hub = await screen.findByRole("article", { name: "Docker Hub" });
    await user.click(within(hub).getByRole("button", { name: "Connect" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a registry login" });
    await user.type(within(sheet).getByRole("textbox", { name: "Username" }), "acme");
    await user.type(within(sheet).getByLabelText("Token"), "hub-token");
    expect(await within(sheet).findByText("Signed in", {}, { timeout: 3000 })).toBeVisible();
    await user.selectOptions(within(sheet).getByRole("combobox", { name: "Registry" }), "ghcr");
    expect(within(sheet).queryByText("Signed in")).toBeNull();
    await new Promise((resolve) => setTimeout(resolve, 900));
    expect(checks.some((c) => c.registry === "ghcr.io" && c.password !== "")).toBe(false);
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeDisabled();
    await user.click(within(sheet).getByRole("button", { name: "Check with ghcr.io" }));
    expect(await within(sheet).findByText("Signed in", {}, { timeout: 3000 })).toBeVisible();
    expect(checks.at(-1)).toMatchObject({ registry: "ghcr.io", password: "hub-token" });
  });

  it("speaks Simplified Chinese", async () => {
    await activateLocale("zh-CN");
    renderAt("/settings/integrations", {
      backups: { listBackupDestinations: () => ({ destinations: [] }) },
      images: { listRegistryLogins: () => ({ logins: [ghcr] }) },
    });
    expect(await screen.findByRole("heading", { level: 2, name: "镜像仓库" })).toBeVisible();
  });
});

describe("registries", () => {
  it("reads an image's registry as Docker does", () => {
    expect(registryOf("nginx:1.27")).toBe("docker.io");
    expect(registryOf("acme/web:1")).toBe("docker.io");
    expect(registryOf("ghcr.io/acme/web:1")).toBe("ghcr.io");
    expect(registryOf("localhost:5000/web")).toBe("localhost:5000");
    expect(registryOf("index.docker.io/acme/web")).toBe("docker.io");
    expect(registryKind("123456789012.dkr.ecr.eu-west-1.amazonaws.com")).toBe("ecr");
    expect(registryKind("registry.example.com")).toBe("other");
  });

  it("names a new login after its registry, and never one that's taken", () => {
    expect(freeLoginName("ghcr", [ghcr])).toBe("ghcr");
    expect(freeLoginName("ghcr-ci", [ghcr])).toBe("ghcr-ci-2");
    expect(freeLoginName("Registry.Example", [])).toBe("registry-example");
  });
});
