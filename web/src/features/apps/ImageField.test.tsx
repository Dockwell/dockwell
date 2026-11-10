// SPDX-License-Identifier: AGPL-3.0-only

import { Code, ConnectError } from "@connectrpc/connect";
import { ImageAnswer } from "@dockwell/api";
import { screen, waitFor } from "@testing-library/react";
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
