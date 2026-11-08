// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";
import {
  type Change,
  ChangeConfirmationSchema,
  ChangeState,
  type ConfirmChangeRequest,
  StepProgressSchema,
  StepState,
  type WatchChangeResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useEffect } from "react";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { authError } from "../../test/auth";
import { moved, watched, webPlanned } from "../../test/changes";
import { feed, renderWithApi } from "../../test/render";
import { ChangeSheetProvider, type Opening, useChangeSheet } from "./ChangeSheet";

function Opener({ opening }: { opening: Opening }) {
  const sheet = useChangeSheet();
  useEffect(() => {
    sheet.open(opening);
  }, [sheet, opening]);
  return null;
}

/** A change to a server's firewall, applying, whose step waits for its confirmation for some seconds yet. */
function waiting(change: Change, seconds: number): Change {
  return moved(change, {
    state: ChangeState.APPLYING,
    steps: [
      create(StepProgressSchema, {
        serverId: "srv_1",
        resourceId: "res_web",
        state: StepState.RUNNING,
        confirmation: create(ChangeConfirmationSchema, {
          serverId: "srv_1",
          deadline: timestampFromDate(new Date(Date.now() + seconds * 1000)),
          sshPorts: [22],
        }),
      }),
    ],
  });
}

function show(confirm: (req: ConfirmChangeRequest) => object) {
  const change = webPlanned();
  const watch = feed<WatchChangeResponse>();
  const confirms: ConfirmChangeRequest[] = [];
  renderWithApi(
    <ChangeSheetProvider>
      <Opener opening={{ kind: "plan", make: () => Promise.resolve({ change }) }} />
    </ChangeSheetProvider>,
    {
      changes: {
        watchChange: watch.stream,
        applyChange: () => ({ change: moved(change, { state: ChangeState.APPLYING }) }),
        confirmChange: (req) => {
          confirms.push(req);
          return confirm(req);
        },
      },
    },
  );
  return { change, watch, confirms };
}

describe("a change that waits for its confirmation", () => {
  it("says what to test and how long is left, and keeps the change when asked", async () => {
    const user = userEvent.setup();
    const { change, watch, confirms } = show(() => ({}));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    act(() => watch.push(watched(change)));
    await user.click(within(sheet).getByRole("button", { name: /Apply plan/ }));
    act(() => watch.push(watched(waiting(change, 45))));
    const confirm = await screen.findByText("Check you can still reach srv_1");
    const callout = confirm.closest(".dw-callout") as HTMLElement;
    expect(callout).toHaveTextContent(
      "Open a new SSH session to port 22 from where you are now, with your key. If it works, keep the change.",
    );
    expect(callout).toHaveTextContent(/srv_1 puts back its firewall as it was in 4\d s unless you keep it\./);
    expect(screen.getByText("If you close this without keeping it, the change goes back on its own.")).toBeVisible();
    await expectAccessible(document.body);
    await user.click(within(callout).getByRole("button", { name: "Keep the change" }));
    await waitFor(() => expect(confirms).toHaveLength(1));
    expect(confirms[0]?.id).toBe(change.id);
    expect(confirms[0]?.serverId).toBe("srv_1");
    expect(confirms[0]?.deadline).toBeDefined();
  });

  it("says a confirmation that isn't yours can't keep it, and doesn't offer to once time is up", async () => {
    const user = userEvent.setup();
    const { change, watch } = show(() => {
      throw authError(Code.PermissionDenied, "not_yours");
    });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    act(() => watch.push(watched(change)));
    await user.click(within(sheet).getByRole("button", { name: /Apply plan/ }));
    act(() => watch.push(watched(waiting(change, 45))));
    await user.click(await screen.findByRole("button", { name: "Keep the change" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Only the person who applied this change can keep it, from where they applied it.",
    );
    act(() => watch.push(watched(waiting(change, -1))));
    expect(await screen.findByText("srv_1 is putting back its firewall as it was.")).toBeVisible();
    expect(screen.getByRole("button", { name: "Keep the change" })).toBeDisabled();
  });
});
