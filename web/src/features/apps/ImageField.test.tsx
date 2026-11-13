// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { type GetImageRequest, ImageAnswer, RegistryLoginInfoSchema } from "@dockwell/api";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it } from "vitest";

import { ENV, places } from "../../test/apps";
import { renderWithApi } from "../../test/render";
import { ImageField } from "./ImageField";

const field = <ImageField value="nginx:1.27" onChange={() => {}} environment={ENV} server="srv_1" />;
const busy = () =>
  new ConnectError("this server is reading two images from their registries already", Code.Unavailable);

describe("the image field, when the server is busy", () => {
  it("asks once more while it says Checking, and then says what it found", async () => {
    let asked = 0;
    renderWithApi(field, {
      resources: places(),
      images: {
        getImage: () => {
          asked += 1;
          if (asked === 1) throw busy();
          return { answer: ImageAnswer.FOUND, digest: `sha256:${"1".repeat(64)}` };
        },
      },
    });
    const box = await screen.findByRole("combobox", { name: "Image" });
    await waitFor(() => expect(box).toHaveAccessibleDescription("Checking"));
    await waitFor(() => expect(box).toHaveAccessibleDescription("Found"), { timeout: 5000 });
    expect(asked).toBe(2);
  });

  it("says it can't reach the registry when asking again didn't help, rather than nothing", async () => {
    let asked = 0;
    renderWithApi(field, {
      resources: places(),
      images: {
        getImage: () => {
          asked += 1;
          throw busy();
        },
      },
    });
    const box = await screen.findByRole("combobox", { name: "Image" });
    await waitFor(() => expect(box).toHaveAccessibleDescription("Can't reach the registry"), { timeout: 5000 });
    expect(asked).toBe(2);
  });
});

const login = (name: string) => create(RegistryLoginInfoSchema, { id: `rgl_${name}`, name, registry: "ghcr.io" });
const privateImage = "ghcr.io/acme/private:1.0";

/** The field as New app holds it, with the login a person picks. */
function Picking({ onPick }: { onPick?: (name: string) => void }) {
  const [picked, setPicked] = useState("");
  return (
    <ImageField
      value={privateImage}
      onChange={() => {}}
      environment={ENV}
      server="srv_1"
      registryLogin={picked}
      onRegistryLogin={(name) => {
        setPicked(name);
        onPick?.(name);
      }}
    />
  );
}

describe("the image field, for an image its registry keeps private", () => {
  it("says which login found it, where its project may use one", async () => {
    renderWithApi(<Picking />, {
      resources: places(),
      images: {
        getImage: () => ({ answer: ImageAnswer.FOUND, digest: `sha256:${"2".repeat(64)}`, registryLogin: "ghcr-ci" }),
        listRegistryLogins: () => ({ logins: [login("ghcr-ci")] }),
      },
    });
    const box = await screen.findByRole("combobox", { name: "Image" });
    await waitFor(() => expect(box).toHaveAccessibleDescription("Found, with ghcr-ci"));
    // One login is the default: nothing to pick.
    expect(screen.queryByRole("combobox", { name: "Pull with" })).toBeNull();
  });

  it("asks which login to pull with, where its project may use several, and reads with the one picked", async () => {
    const user = userEvent.setup();
    const asked: GetImageRequest[] = [];
    renderWithApi(<Picking />, {
      resources: places(),
      images: {
        getImage: (req) => {
          asked.push(req);
          return req.registryLogin === "ghcr-shop"
            ? { answer: ImageAnswer.FOUND, digest: `sha256:${"3".repeat(64)}`, registryLogin: "ghcr-shop" }
            : { answer: ImageAnswer.NOT_FOUND };
        },
        listRegistryLogins: () => ({ logins: [login("ghcr-ci"), login("ghcr-shop")] }),
      },
    });
    const box = await screen.findByRole("combobox", { name: "Image" });
    await waitFor(() => expect(box).toHaveAccessibleDescription("Not found, or private"));
    await user.selectOptions(await screen.findByRole("combobox", { name: "Pull with" }), "ghcr-shop");
    await waitFor(() => expect(box).toHaveAccessibleDescription("Found, with ghcr-shop"));
    expect(asked.at(-1)?.registryLogin).toBe("ghcr-shop");
  });

  it("offers an admin a login for the registry where its project may use none", async () => {
    const user = userEvent.setup();
    renderWithApi(<Picking />, {
      resources: places(),
      images: {
        getImage: () => ({ answer: ImageAnswer.NOT_FOUND }),
        listRegistryLogins: () => ({ logins: [] }),
      },
    });
    await user.click(await screen.findByRole("button", { name: "Add a login for ghcr.io" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a registry login" });
    expect(sheet).toBeVisible();
    expect(screen.getByRole("combobox", { name: "Registry" })).toHaveValue("ghcr");
  });
});
