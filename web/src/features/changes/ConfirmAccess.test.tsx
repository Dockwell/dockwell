// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code } from "@connectrpc/connect";
import {
  ActorKind,
  ActorSchema,
  type Change,
  ChangeConfirmationSchema,
  ChangeSchema,
  ChangeState,
  type ConfirmChangeRequest,
  Connection,
  type JailPlanSchema,
  PlanAction,
  PlanWarning,
  type Server,
  StepProgressSchema,
  StepState,
  StepUpdateSchema,
  type WatchChangeResponse,
  WatchChangeResponseSchema,
  type WatchServersResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useEffect } from "react";
import { describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { authError, authState } from "../../test/auth";
import { applied, moved, watched, webPlanned } from "../../test/changes";
import { feed, renderWithApi } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { FleetProvider } from "../servers/useFleet";
import { ChangeSheetProvider, type Opening, useChangeSheet } from "./ChangeSheet";

function Opener({ opening }: { opening: Opening }) {
  const sheet = useChangeSheet();
  useEffect(() => {
    sheet.open(opening);
  }, [sheet, opening]);
  return null;
}

/** A change to a server's firewall, applying, whose step waits for its confirmation for some seconds yet. */
function waiting(change: Change, seconds: number, ports: number[] = [22]): Change {
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
          sshPorts: ports,
        }),
      }),
    ],
  });
}

function show(confirm: (req: ConfirmChangeRequest) => object, servers?: Server[]) {
  const change = webPlanned();
  const watch = feed<WatchChangeResponse>();
  const confirms: ConfirmChangeRequest[] = [];
  const fleet = feed<WatchServersResponse>();
  if (servers) fleet.push(...servers.map((s) => changed(s)), snapshotComplete);
  renderWithApi(
    <FleetProvider>
      <ChangeSheetProvider>
        <Opener opening={{ kind: "plan", make: () => Promise.resolve({ change }) }} />
      </ChangeSheetProvider>
    </FleetProvider>,
    {
      changes: {
        watchChange: watch.stream,
        applyChange: () => ({ change: moved(change, { state: ChangeState.APPLYING }) }),
        confirmChange: (req) => {
          confirms.push(req);
          return confirm(req);
        },
      },
      ...(servers ? { servers: { watchServers: fleet.stream } } : {}),
    },
  );
  return { change, watch, confirms };
}

/** Applies the plan the sheet opened with, and has its step wait as given. */
async function applyAndWait(change: Change, watch: ReturnType<typeof feed<WatchChangeResponse>>, waits: Change) {
  const user = userEvent.setup();
  const sheet = await screen.findByRole("dialog", { name: "Review plan" });
  act(() => watch.push(watched(change)));
  await user.click(within(sheet).getByRole("button", { name: /Apply plan/ }));
  act(() => watch.push(watched(waits)));
  return user;
}

// What the watch said a change waits for, which its apply's answer, read before the step began
// to wait and delivered after, mustn't take away: the server says it once, and the CI flake on
// #415 and #505 was this sheet losing it, while the Firewall tab listed the rule as waiting.
it("keeps a wait the watch said, when the apply's own answer comes after it without it", async () => {
  const user = userEvent.setup();
  const change = webPlanned();
  const watch = feed<WatchChangeResponse>();
  let answer: (value: object) => void = () => {};
  const running = (confirmation?: MessageInitShape<typeof ChangeConfirmationSchema>) =>
    create(StepProgressSchema, {
      serverId: "srv_1",
      resourceId: "res_web",
      state: StepState.RUNNING,
      ...(confirmation ? { confirmation: create(ChangeConfirmationSchema, confirmation) } : {}),
    });
  renderWithApi(
    <FleetProvider>
      <ChangeSheetProvider>
        <Opener opening={{ kind: "plan", make: () => Promise.resolve({ change }) }} />
      </ChangeSheetProvider>
    </FleetProvider>,
    {
      changes: {
        watchChange: watch.stream,
        applyChange: () =>
          new Promise<object>((resolve) => {
            answer = resolve;
          }),
      },
    },
  );
  const sheet = await screen.findByRole("dialog", { name: "Review plan" });
  act(() => watch.push(watched(change)));
  await user.click(within(sheet).getByRole("button", { name: /Apply plan/ }));
  // The run moves on while the apply's answer is still on its way: the step starts, then waits.
  act(() => watch.push(watched(moved(change, { state: ChangeState.APPLYING, steps: [running()] }))));
  act(() =>
    watch.push(
      create(WatchChangeResponseSchema, {
        event: {
          case: "step",
          value: create(StepUpdateSchema, {
            index: 0,
            step: running({
              serverId: "srv_1",
              deadline: timestampFromDate(new Date(Date.now() + 45_000)),
              sshPorts: [22],
            }),
          }),
        },
      }),
    ),
  );
  expect(await screen.findByRole("button", { name: "Keep the change" })).toBeVisible();
  // The apply's answer, as it was read, before the step waited.
  await act(async () => answer({ change: moved(change, { state: ChangeState.APPLYING, steps: [running()] }) }));
  expect(screen.getByRole("button", { name: "Keep the change" })).toBeVisible();
});

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
      "Open a new SSH session to port 22 from here with your key, and keep the change if it works.",
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

  it("names and keeps the server that waits now, as the steps move on, not the one the change said first", async () => {
    const user = userEvent.setup();
    const { change, watch, confirms } = show(() => ({}));
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    act(() => watch.push(watched(change)));
    await user.click(within(sheet).getByRole("button", { name: /Apply plan/ }));
    const first = create(ChangeConfirmationSchema, {
      serverId: "srv_1",
      deadline: timestampFromDate(new Date(Date.now() + 40_000)),
      sshPorts: [22],
    });
    // The watch starts again while srv_1 waits: its first message is the change, with what it waits for.
    act(() =>
      watch.push(
        watched(
          moved(change, {
            state: ChangeState.APPLYING,
            confirmation: first,
            steps: [
              create(StepProgressSchema, {
                serverId: "srv_1",
                resourceId: "res_a",
                state: StepState.RUNNING,
                confirmation: first,
              }),
              create(StepProgressSchema, { serverId: "srv_2", resourceId: "res_b", state: StepState.WAITING }),
            ],
          }),
        ),
      ),
    );
    await screen.findByText("Check you can still reach srv_1");
    // srv_1 is kept, and srv_2 waits now: both come as steps.
    const second = create(ChangeConfirmationSchema, {
      serverId: "srv_2",
      deadline: timestampFromDate(new Date(Date.now() + 58_000)),
      sshPorts: [2222],
    });
    act(() =>
      watch.push(
        create(WatchChangeResponseSchema, {
          event: {
            case: "step",
            value: create(StepUpdateSchema, {
              index: 0,
              step: create(StepProgressSchema, { serverId: "srv_1", resourceId: "res_a", state: StepState.DONE }),
            }),
          },
        }),
        create(WatchChangeResponseSchema, {
          event: {
            case: "step",
            value: create(StepUpdateSchema, {
              index: 1,
              step: create(StepProgressSchema, {
                serverId: "srv_2",
                resourceId: "res_b",
                state: StepState.RUNNING,
                confirmation: second,
              }),
            }),
          },
        }),
      ),
    );
    expect(await screen.findByText("Check you can still reach srv_2")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Keep the change" }));
    await waitFor(() => expect(confirms).toHaveLength(1));
    expect(confirms[0]?.serverId).toBe("srv_2");
  });

  it("counts down in whole seconds, never more than are left, and stops offering Keep just before the end", async () => {
    const { change, watch } = show(() => ({}));
    await applyAndWait(change, watch, waiting(change, 60.5));
    expect(await screen.findByText(/puts back its firewall as it was in 60 s unless you keep it\./)).toBeVisible();
    expect(screen.getByRole("button", { name: "Keep the change" })).toBeEnabled();
    act(() => watch.push(watched(waiting(change, 1.5))));
    expect(await screen.findByText("srv_1 is putting back its firewall as it was.")).toBeVisible();
    expect(screen.getByRole("button", { name: "Keep the change" })).toBeDisabled();
  });

  it("names each SSH port, or none when the server found no SSH", async () => {
    const { change, watch } = show(() => ({}));
    await applyAndWait(change, watch, waiting(change, 45, [22, 2222]));
    expect(
      await screen.findByText(
        "Open a new SSH session to port 22 or 2222 from here with your key, and keep the change if it works.",
      ),
    ).toBeVisible();
    act(() => watch.push(watched(waiting(change, 45, []))));
    expect(
      await screen.findByText("Open a new SSH session from here with your key, and keep the change if it works."),
    ).toBeVisible();
  });

  it("says a server whose agent has gone puts its firewall back on its own, and doesn't offer Keep", async () => {
    const gone = aServer({ id: "srv_1", name: "hz-1", connection: Connection.UNREACHABLE });
    const { change, watch } = show(() => ({}), [gone]);
    await applyAndWait(change, watch, waiting(change, 45));
    expect(
      await screen.findByText("hz-1's agent isn't connected, so it puts back its firewall when its time runs out."),
    ).toBeVisible();
    expect(screen.getByRole("button", { name: "Keep the change" })).toBeDisabled();
  });

  it("says a wait that's over is over, rather than to watch the change", async () => {
    const { change, watch } = show(() => {
      throw authError(Code.FailedPrecondition, "not_waiting", "nothing waits");
    });
    const user = await applyAndWait(change, watch, waiting(change, 45));
    await user.click(await screen.findByRole("button", { name: "Keep the change" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "That wait is over: this sheet shows whether the server kept the change.",
    );
  });

  it("says a change the server couldn't keep still waits, with what to free first, and offers Keep again", async () => {
    let tries = 0;
    const { change, watch } = show(() => {
      tries++;
      throw authError(Code.FailedPrecondition, "not_kept", "the server couldn't keep it yet");
    });
    const user = await applyAndWait(change, watch, waiting(change, 45));
    await user.click(await screen.findByRole("button", { name: "Keep the change" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "The server couldn't keep it yet, so it still waits. Free space on its disk, then keep it again.",
    );
    const keep = screen.getByRole("button", { name: "Keep the change" });
    expect(keep).toBeEnabled();
    await user.click(keep);
    await waitFor(() => expect(tries).toBe(2));
  });
});

/** A planned change that turns deny everything else on, on hz-1, where it closes two ports. */
function policyPlanned(): Change {
  return create(ChangeSchema, {
    id: "chg_policy",
    summary: "Deny everything else on hz-1",
    state: ChangeState.PLANNED,
    createTime: timestampFromDate(new Date()),
    plan: {
      fingerprint: "b1c2d3e4f5a60718293a4b5c6d7e8f90",
      summary: { create: 1 },
      destructive: true,
      resources: [
        {
          id: "res_policy",
          resource: { kind: "firewall.policy", scope: "srv_1", name: "default" },
          action: PlanAction.CREATE,
          servers: ["srv_1"],
        },
      ],
      servers: [
        {
          serverId: "srv_1",
          serverName: "hz-1",
          steps: [
            {
              resourceId: "res_policy",
              resource: { kind: "firewall.policy", scope: "srv_1", name: "default" },
              action: PlanAction.CREATE,
              warnings: [PlanWarning.MAY_LOCK_OUT],
              closes: ["5432/tcp", "8080/tcp (shop-web-1)"],
              stays: {
                sshPorts: [22],
                pinned: ["203.0.113.7/32"],
                allowed: ["198.51.100.0/24"],
                tunnels: ["wg0 (51820/udp)"],
                panelPorts: [7600],
              },
            },
          ],
        },
      ],
    },
  });
}

describe("a change to a server's firewall policy", () => {
  it("lists what it closes, says what it does once applied, and drops the warning to check first", async () => {
    const change = policyPlanned();
    const watch = feed<WatchChangeResponse>();
    renderWithApi(
      <FleetProvider>
        <ChangeSheetProvider>
          <Opener opening={{ kind: "plan", make: () => Promise.resolve({ change }) }} />
        </ChangeSheetProvider>
      </FleetProvider>,
      { changes: { watchChange: watch.stream } },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    act(() => watch.push(watched(change)));
    expect(await within(sheet).findByText("Deny everything else closes what no rule lets in on hz-1")).toBeVisible();
    // What it closes and what to do are in sight; why is under How this works.
    expect(sheet).toHaveTextContent(
      "It closes 5432/tcp, 8080/tcp (shop-web-1): add a rule for any you mean to keep open.",
    );
    expect(sheet).toHaveTextContent(
      "SSH stays open as this plan says, and on the control server its own ports stay open.",
    );
    expect(within(sheet).getByText("This may cut off access to hz-1")).toBeVisible();
    // Who SSH stays open from, by name, and what happens when that address changes.
    expect(sheet).toHaveTextContent("SSH on hz-1 stays open only from 203.0.113.7/32, 198.51.100.0/24");
    expect(sheet).toHaveTextContent("To keep it open from more, add a rule that allows port 22 first.");
    expect(sheet).toHaveTextContent(
      "If your address isn't one of these, or stops being one, SSH closes to you, and no timer catches that later.",
    );
    expect(sheet).toHaveTextContent("No rule allows the WireGuard tunnel wg0 (51820/udp) on hz-1");
    act(() => watch.push(watched(applied(change))));
    expect(await screen.findByRole("dialog", { name: "hz-1 denies everything else" })).toBeVisible();
    expect(screen.queryByText("This may cut off access to hz-1")).toBeNull();
  });
});

describe("the confirmation of a change that narrows SSH", () => {
  const open = async (init: Parameters<typeof moved>[1] = {}, me?: string) => {
    const change = policyPlanned();
    const watch = feed<WatchChangeResponse>();
    renderWithApi(
      <FleetProvider>
        <ChangeSheetProvider>
          <Opener opening={{ kind: "plan", make: () => Promise.resolve({ change }) }} />
        </ChangeSheetProvider>
      </FleetProvider>,
      {
        changes: {
          watchChange: watch.stream,
          applyChange: () => ({ change: moved(change, { state: ChangeState.APPLYING }) }),
        },
        ...(me ? { auth: { getAuthState: () => authState({ user: { id: me, name: "Sam Okafor" } }) } } : {}),
      },
    );
    const waits = moved(change, {
      state: ChangeState.APPLYING,
      appliedBy: create(ActorSchema, { kind: ActorKind.USER, id: "usr_01JD6Z3R8M9QF2K7T4XW5BNC0V", name: "Mira Chen" }),
      steps: [
        create(StepProgressSchema, {
          serverId: "srv_1",
          resourceId: "res_policy",
          state: StepState.RUNNING,
          confirmation: create(ChangeConfirmationSchema, {
            serverId: "srv_1",
            deadline: timestampFromDate(new Date(Date.now() + 45_000)),
            sshPorts: [22],
          }),
        }),
      ],
      ...init,
    });
    await applyAndWait(change, watch, waits);
    const confirm = await screen.findByText(/^Check you can still reach /);
    return confirm.closest(".dw-callout") as HTMLElement;
  };

  it("names who SSH stays open from, and asks for the panel in a new tab where its own server closes the rest", async () => {
    const callout = await open();
    expect(callout).toHaveTextContent("Once kept, SSH stays open only from 203.0.113.7/32, 198.51.100.0/24.");
    expect(callout).toHaveTextContent("Then open the panel at its own address in a new tab.");
    expect(within(callout).getByRole("button", { name: "Keep the change" })).toBeEnabled();
  });

  it("doesn't offer Keep to someone else watching, and says who can", async () => {
    const callout = await open({}, "usr_01JD6Z3R8M9QF2K7T4XWOTHER");
    expect(within(callout).queryByRole("button", { name: "Keep the change" })).toBeNull();
    expect(callout).toHaveTextContent(
      "Only the person who applied this change can keep it, from where they applied it.",
    );
  });
});

/** A planned change that sets the SSH jail on hz-1, or stops writing it. */
function jailPlanned(action: PlanAction, jail?: MessageInitShape<typeof JailPlanSchema>): Change {
  const resource = () => ({ kind: "fail2ban.jail", scope: "srv_1", name: "sshd" });
  return create(ChangeSchema, {
    id: "chg_jail",
    summary: "Protect SSH on hz-1 with fail2ban",
    state: ChangeState.PLANNED,
    createTime: timestampFromDate(new Date()),
    plan: {
      fingerprint: "c1c2d3e4f5a60718293a4b5c6d7e8f90",
      summary: { create: 1 },
      resources: [{ id: "res_jail", resource: resource(), action, servers: ["srv_1"] }],
      servers: [
        {
          serverId: "srv_1",
          serverName: "hz-1",
          steps: [{ resourceId: "res_jail", resource: resource(), action, ...(jail ? { jail } : {}) }],
        },
      ],
    },
  });
}

describe("a change to a fail2ban jail", () => {
  const open = async (change: Change) => {
    const watch = feed<WatchChangeResponse>();
    renderWithApi(
      <FleetProvider>
        <ChangeSheetProvider>
          <Opener opening={{ kind: "plan", make: () => Promise.resolve({ change }) }} />
        </ChangeSheetProvider>
      </FleetProvider>,
      { changes: { watchChange: watch.stream } },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    act(() => watch.push(watched(change)));
    return sheet;
  };

  const lifting =
    "Undoing this, or removing the jail, doesn't lift a ban already made: sign in from another address or your provider's console, and run fail2ban-client unban with the address.";

  it("names whom the jail never bans, and says undo lifts no ban", async () => {
    const sheet = await open(
      jailPlanned(PlanAction.CREATE, { neverBans: ["127.0.0.1/8", "::1", "192.0.2.10", "203.0.113.7/32"] }),
    );
    expect(sheet).toHaveTextContent("This jail never bans 127.0.0.1/8, ::1, 192.0.2.10, 203.0.113.7/32 on hz-1");
    expect(sheet).toHaveTextContent("It bans any other address that keeps failing to sign in, yours included.");
    // Why, and that undo lifts no ban, is one step away.
    expect(sheet).toHaveTextContent(
      `How this worksfail2ban counts the failures from just before this applies, so a ban can land as it does. ${lifting}`,
    );
  });

  it("says which address signed in over SSH it could ban as it applies", async () => {
    const sheet = await open(
      jailPlanned(PlanAction.CREATE, { neverBans: ["127.0.0.1/8", "::1"], signedInNotIgnored: ["203.0.113.7"] }),
    );
    expect(sheet).toHaveTextContent("This jail can ban 203.0.113.7, signed in to hz-1 over SSH now");
    expect(sheet).toHaveTextContent("Add it to what the jail never bans first, or apply this knowing it.");
    expect(sheet).toHaveTextContent(
      `It never bans 127.0.0.1/8, ::1. fail2ban counts the failures from just before this applies, so a ban can land as it does. ${lifting}`,
    );
  });

  it("says a jail that ignores every address bans nobody", async () => {
    const sheet = await open(
      jailPlanned(PlanAction.CREATE, { neverBans: ["127.0.0.1/8", "::1", "0.0.0.0/0"], ignoresEverything: true }),
    );
    expect(sheet).toHaveTextContent("This jail ignores every address on hz-1, so it bans nobody");
  });

  it("says it can ban your own address where an older agent says nothing of whom it ignores", async () => {
    const sheet = await open(jailPlanned(PlanAction.CREATE));
    expect(sheet).toHaveTextContent("This jail can ban your own address on hz-1");
    expect(sheet).toHaveTextContent(lifting);
  });

  it("says nothing of bans when it stops writing the jail", async () => {
    const sheet = await open(jailPlanned(PlanAction.DELETE));
    await within(sheet).findByRole("button", { name: /Delete sshd/ });
    expect(sheet).not.toHaveTextContent("This jail");
  });
});
