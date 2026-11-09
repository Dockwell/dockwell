// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, type HandlerContext } from "@connectrpc/connect";
import {
  type AttachTerminalRequest,
  AttachTerminalResponseSchema,
  StepUpAction,
  TerminalEndReason,
  TerminalEndSchema,
  TerminalSchema,
  type WriteTerminalRequest,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { authError, authState, requestOptions, stubPasskeys } from "../../test/auth";
import { type Api, onPhone, renderAt, untilAborted } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";

/** xterm.js draws on a canvas jsdom hasn't, so a stand-in takes its place: what it's sent, and what's "typed". */
const screens = vi.hoisted(() => ({ all: [] as { written: string[]; type: (s: string) => void; selected: string }[] }));

vi.mock("@xterm/xterm", () => {
  class Terminal {
    cols = 100;
    rows = 30;
    modes = { applicationCursorKeysMode: false };
    options: Record<string, unknown>;
    private onDataFn: (d: string) => void = () => {};
    private selection = "";
    written: string[] = [];
    constructor(options: Record<string, unknown>) {
      this.options = { ...options };
      screens.all.push({
        written: this.written,
        type: (s: string) => this.onDataFn(s),
        get selected() {
          return "";
        },
      });
    }
    loadAddon() {}
    open() {}
    onData(fn: (d: string) => void) {
      this.onDataFn = fn;
    }
    onBinary() {}
    onResize() {}
    onSelectionChange() {}
    attachCustomKeyEventHandler() {}
    focus() {}
    write(d: Uint8Array) {
      this.written.push(new TextDecoder().decode(d));
    }
    hasSelection() {
      return this.selection !== "";
    }
    getSelection() {
      return this.selection;
    }
    clear() {}
    clearSelection() {}
    paste() {}
    dispose() {}
  }
  return { Terminal };
});
vi.mock("@xterm/addon-fit", () => ({
  FitAddon: class {
    fit() {}
  },
}));
vi.mock("@xterm/addon-search", () => ({
  SearchAddon: class {
    findNext() {
      return false;
    }
    findPrevious() {
      return false;
    }
    clearDecorations() {}
  },
}));

const hz1 = aServer({ id: "srv_1", name: "hz-1" });
const term = (id = "trm_1", title = "root@hz-1") =>
  create(TerminalSchema, {
    id,
    serverId: "srv_1",
    title,
    columns: 100,
    rows: 30,
    openTime: timestampFromDate(new Date(2026, 10, 7)),
  });

async function* watchServers(_req: unknown, ctx: HandlerContext) {
  yield changed(hz1);
  yield snapshotComplete;
  await untilAborted(ctx.signal);
}

/** A shell that answers each attachment with what it printed, and takes what's typed. */
function shell(printed = "root@hz-1:~# ", end?: { reason: TerminalEndReason; exitCode?: number }) {
  const writes: WriteTerminalRequest[] = [];
  const attaches: AttachTerminalRequest[] = [];
  let taken = 0n;
  return {
    writes,
    attaches,
    async *attachTerminal(req: AttachTerminalRequest, ctx: HandlerContext) {
      attaches.push(req);
      yield create(AttachTerminalResponseSchema, { attached: true, offset: 0n, inputOffset: taken });
      yield create(AttachTerminalResponseSchema, { offset: 0n, data: new TextEncoder().encode(printed) });
      if (end) {
        yield create(AttachTerminalResponseSchema, { end: create(TerminalEndSchema, end) });
        return;
      }
      await untilAborted(ctx.signal);
    },
    writeTerminal(req: WriteTerminalRequest) {
      writes.push(req);
      taken = req.offset + BigInt(req.data.length);
      return { offset: taken };
    },
  };
}

function renderTerminal(terminals: NonNullable<Api["terminals"]>, auth?: Api["auth"]) {
  return renderAt("/servers/srv_1/terminal", { servers: { watchServers }, terminals, auth });
}

afterEach(() => {
  vi.unstubAllGlobals();
  screens.all.length = 0;
});

describe("TerminalPage", () => {
  it("is a tab of the server's page, and says what a terminal is before one opens", async () => {
    renderTerminal({ listTerminals: () => ({}) });
    expect(await screen.findByRole("link", { name: "Terminal" })).toHaveAttribute("aria-current", "page");
    expect(await screen.findByText("No terminal open", { selector: "h2" })).toBeInTheDocument();
    expect(screen.getByText(/is a root shell on/)).toHaveTextContent(
      "A terminal here is a root shell on hz-1, through its agent, so it needs no SSH port. Opening one asks for your passkey.",
    );
    expect(screen.getByRole("button", { name: "Open a terminal" })).toBeEnabled();
    await expectAccessible(document.body);
  });

  it("opens a terminal with the passkey, attaches it, and types into it", async () => {
    stubPasskeys();
    const sh = shell();
    const openTerminal = vi
      .fn()
      .mockRejectedValueOnce(authError(Code.PermissionDenied, "needs_step_up"))
      .mockResolvedValueOnce({ terminal: term() });
    const beginStepUp = vi.fn(() => ({ ceremonyId: "cer_1", optionsJson: requestOptions }));
    renderTerminal(
      { listTerminals: () => ({}), openTerminal, attachTerminal: sh.attachTerminal, writeTerminal: sh.writeTerminal },
      { getAuthState: () => authState(), beginStepUp, finishStepUp: () => ({ proof: "proof_1" }) },
    );
    await userEvent.click(await screen.findByRole("button", { name: "Open a terminal" }));
    expect(await screen.findByRole("tab", { name: /root@hz-1/, selected: true })).toBeInTheDocument();
    expect(beginStepUp).toHaveBeenCalledWith(
      expect.objectContaining({ action: StepUpAction.OPEN_TERMINAL, target: "srv_1" }),
      expect.anything(),
    );
    expect(openTerminal.mock.calls.map((c) => c[0].proof)).toEqual(["", "proof_1"]);
    expect(await screen.findByText("Connected through the agent")).toBeInTheDocument();
    await waitFor(() => expect(screens.all[0]?.written.join("")).toBe("root@hz-1:~# "));
    screens.all[0]?.type("uptime\r");
    await waitFor(() => expect(sh.writes.some((w) => new TextDecoder().decode(w.data) === "uptime\r")).toBe(true));
    expect(screen.getByText("100 × 30")).toBeInTheDocument();
    await expectAccessible(document.body);
  });

  it("points to SSH when there's no passkey for this address", async () => {
    renderTerminal(
      {
        listTerminals: () => ({}),
        openTerminal: () => Promise.reject(authError(Code.PermissionDenied, "needs_step_up")),
      },
      {
        getAuthState: () => authState(),
        beginStepUp: () => Promise.reject(authError(Code.FailedPrecondition, "no_passkey_here")),
      },
    );
    await userEvent.click(await screen.findByRole("button", { name: "Open a terminal" }));
    expect(await screen.findByText(/you have none for this address/)).toHaveTextContent(
      "Opening a terminal takes a passkey, and you have none for this address. Add one under Profile and security, or connect to hz-1 over SSH.",
    );
    expect(screen.queryByRole("tab")).not.toBeInTheDocument();
  });

  it("says when the passkey prompt closed", async () => {
    stubPasskeys({ get: "NotAllowedError" });
    renderTerminal(
      {
        listTerminals: () => ({}),
        openTerminal: () => Promise.reject(authError(Code.PermissionDenied, "needs_step_up")),
      },
      { getAuthState: () => authState(), beginStepUp: () => ({ ceremonyId: "cer_1", optionsJson: requestOptions }) },
    );
    await userEvent.click(await screen.findByRole("button", { name: "Open a terminal" }));
    expect(
      await screen.findByText("The passkey prompt closed, so no terminal opened. Open one when you're ready."),
    ).toBeInTheDocument();
  });

  it("finds the terminals this session has open, and attaches only the one on screen", async () => {
    const sh = shell();
    renderTerminal({
      listTerminals: () => ({ terminals: [term("trm_1"), term("trm_2")] }),
      attachTerminal: sh.attachTerminal,
      writeTerminal: sh.writeTerminal,
    });
    const tabs = within(await screen.findByRole("tablist", { name: "Sessions" }));
    await waitFor(() => expect(tabs.getAllByRole("tab")).toHaveLength(2));
    expect(tabs.getAllByRole("tab").map((t) => t.textContent)).toEqual(["root@hz-1", "root@hz-1 (2)"]);
    expect(tabs.getByRole("tab", { name: "root@hz-1 (2)" })).toHaveAttribute("aria-selected", "true");
    await waitFor(() => expect(sh.attaches.map((a) => a.terminalId)).toEqual(["trm_2"]));
    await userEvent.click(tabs.getByRole("tab", { name: "root@hz-1" }));
    await waitFor(() => expect(sh.attaches.map((a) => a.terminalId)).toEqual(["trm_2", "trm_1"]));
  });

  it("says how a terminal ended, and offers a new one", async () => {
    const sh = shell("logout\r\n", { reason: TerminalEndReason.EXITED, exitCode: 3 });
    renderTerminal({ listTerminals: () => ({ terminals: [term()] }), attachTerminal: sh.attachTerminal });
    expect(await screen.findByText("The shell exited")).toBeInTheDocument();
    expect(screen.getByText("It exited with code 3.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Open a new terminal" })).toBeEnabled();
    await userEvent.click(screen.getByRole("button", { name: "Close this tab" }));
    expect(await screen.findByText("No terminal open", { selector: "h2" })).toBeInTheDocument();
  });

  it("closes a terminal from its options", async () => {
    const sh = shell();
    const closeTerminal = vi.fn(() => ({}));
    renderTerminal({
      listTerminals: () => ({ terminals: [term()] }),
      attachTerminal: sh.attachTerminal,
      writeTerminal: sh.writeTerminal,
      closeTerminal,
    });
    await screen.findByText("Connected through the agent");
    await userEvent.click(screen.getByRole("button", { name: "Session options" }));
    const menu = await screen.findByRole("menu", { name: "Session options" });
    expect(
      within(menu)
        .getAllByRole("menuitem")
        .map((m) => m.textContent),
    ).toEqual(["Paste", "Clear scrollback", "Close terminal"]);
    await userEvent.click(within(menu).getByRole("menuitem", { name: "Close terminal" }));
    await waitFor(() =>
      expect(closeTerminal).toHaveBeenCalledWith(expect.objectContaining({ terminalId: "trm_1" }), expect.anything()),
    );
    expect(await screen.findByText("No terminal open", { selector: "h2" })).toBeInTheDocument();
  });

  it("offers a shell in the container the page was asked for, and opens one with no passkey where none is needed", async () => {
    const sh = shell("$ ");
    const openTerminal = vi.fn(() => ({ terminal: term("trm_9", "node@web-1") }));
    renderAt("/servers/srv_1/terminal?container=web-1", {
      servers: { watchServers },
      terminals: { listTerminals: () => ({ terminals: [term()] }), openTerminal, attachTerminal: sh.attachTerminal },
    });
    expect(await screen.findByRole("heading", { name: "No shell open in web-1" })).toBeInTheDocument();
    expect(screen.getByText(/runs in the container, as its own user/)).toHaveTextContent(
      "A shell in web-1 runs in the container, as its own user, with the first of bash, ash and sh its image has. One in a container that reaches as far as root on hz-1 asks for your passkey.",
    );
    // The server's own terminal is there to go back to, and nothing is attached meanwhile.
    expect(screen.getByRole("tab", { name: "root@hz-1" })).toHaveAttribute("aria-selected", "false");
    expect(sh.attaches).toHaveLength(0);
    await userEvent.click(screen.getByRole("button", { name: "Open a shell" }));
    expect(await screen.findByRole("tab", { name: "node@web-1", selected: true })).toBeInTheDocument();
    expect(openTerminal).toHaveBeenCalledTimes(1);
    expect(openTerminal).toHaveBeenCalledWith(
      expect.objectContaining({ container: "web-1", proof: "" }),
      expect.anything(),
    );
    await expectAccessible(document.body);
  });

  it("asks for the passkey for that container where it's as good as root", async () => {
    stubPasskeys();
    const sh = shell("# ");
    const openTerminal = vi
      .fn()
      .mockRejectedValueOnce(authError(Code.PermissionDenied, "needs_step_up"))
      .mockResolvedValueOnce({ terminal: term("trm_9", "root@admin-1") });
    const beginStepUp = vi.fn(() => ({ ceremonyId: "cer_1", optionsJson: requestOptions }));
    renderAt("/servers/srv_1/terminal?container=admin-1", {
      servers: { watchServers },
      terminals: { listTerminals: () => ({}), openTerminal, attachTerminal: sh.attachTerminal },
      auth: { getAuthState: () => authState(), beginStepUp, finishStepUp: () => ({ proof: "proof_1" }) },
    });
    await userEvent.click(await screen.findByRole("button", { name: "Open a shell" }));
    // It says why before the browser asks.
    const why = await screen.findByText("This shell takes your passkey");
    expect(why.closest(".dw-callout")).toHaveTextContent(
      "admin-1 reaches as far as root on hz-1: it's privileged, holds Docker's socket, shares the server's network, or mounts a system path, through a volume or a link too.",
    );
    expect(beginStepUp).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Confirm with your passkey" }));
    expect(await screen.findByRole("tab", { name: "root@admin-1", selected: true })).toBeInTheDocument();
    expect(beginStepUp).toHaveBeenCalledWith(
      expect.objectContaining({ action: StepUpAction.OPEN_TERMINAL, target: "srv_1:admin-1" }),
      expect.anything(),
    );
    expect(openTerminal.mock.calls.map((c) => [c[0].container, c[0].proof])).toEqual([
      ["admin-1", ""],
      ["admin-1", "proof_1"],
    ]);
  });

  it("asks nothing of the passkey when you'd rather not, once it's said why", async () => {
    const openTerminal = vi.fn().mockRejectedValue(authError(Code.PermissionDenied, "needs_step_up"));
    const beginStepUp = vi.fn(() => ({ ceremonyId: "cer_1", optionsJson: requestOptions }));
    renderAt("/servers/srv_1/terminal?container=admin-1", {
      servers: { watchServers },
      terminals: { listTerminals: () => ({}), openTerminal },
      auth: { getAuthState: () => authState(), beginStepUp },
    });
    await userEvent.click(await screen.findByRole("button", { name: "Open a shell" }));
    await userEvent.click(await screen.findByRole("button", { name: "Cancel" }));
    expect(screen.queryByText("This shell takes your passkey")).toBeNull();
    expect(screen.queryByText("No terminal opened")).toBeNull();
    expect(beginStepUp).not.toHaveBeenCalled();
    expect(openTerminal).toHaveBeenCalledTimes(1);
  });

  it("says a shell that wasn't root hung up, not a root shell", async () => {
    const sh = shell("$ ", { reason: TerminalEndReason.IDLE });
    renderTerminal({
      listTerminals: () => ({ terminals: [{ ...term("trm_1", "node@web-1"), container: "web-1" }] }),
      attachTerminal: sh.attachTerminal,
    });
    expect(
      await screen.findByText(
        "Nothing was typed into it for an hour, so its shell hung up, rather than leave it open.",
      ),
    ).toBeInTheDocument();
  });

  it("has the keys a phone's keyboard hasn't, and Ctrl held goes with the next key", async () => {
    onPhone();
    const sh = shell();
    renderTerminal({
      listTerminals: () => ({ terminals: [term()] }),
      attachTerminal: sh.attachTerminal,
      writeTerminal: sh.writeTerminal,
    });
    const keys = within(await screen.findByRole("toolbar", { name: "Terminal keys, scroll for more" }));
    await screen.findByText("root@hz-1 · through the agent");
    await waitFor(() => expect(screens.all[0]?.written.join("")).toBe("root@hz-1:~# "));
    expect(keys.getAllByRole("button").map((b) => b.getAttribute("aria-label"))).toEqual([
      "Escape",
      "Tab",
      "Control",
      "Left arrow",
      "Up arrow",
      "Down arrow",
      "Right arrow",
      "Pipe",
      "Tilde",
    ]);
    const typed = () => sh.writes.map((w) => new TextDecoder().decode(w.data)).join("");
    await userEvent.click(keys.getByRole("button", { name: "Up arrow" }));
    await waitFor(() => expect(typed()).toBe("\x1b[A"));
    const control = keys.getByRole("button", { name: "Control" });
    await userEvent.click(control);
    expect(control).toHaveAttribute("aria-pressed", "true");
    screens.all[0]?.type("c");
    await waitFor(() => expect(typed()).toBe("\x1b[A\x03"));
    expect(control).toHaveAttribute("aria-pressed", "false");
    await userEvent.click(keys.getByRole("button", { name: "Tilde" }));
    await waitFor(() => expect(typed()).toBe("\x1b[A\x03~"));
    expect(screen.getByRole("button", { name: "Text size" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Paste" })).toBeInTheDocument();
    await expectAccessible(document.body);
  });

  it("takes the phone's screen under a header of its own", async () => {
    onPhone();
    const sh = shell();
    renderTerminal({
      listTerminals: () => ({ terminals: [term()] }),
      attachTerminal: sh.attachTerminal,
      writeTerminal: sh.writeTerminal,
    });
    expect(await screen.findByRole("heading", { level: 1, name: "Terminal" })).toBeInTheDocument();
    expect(await screen.findByText("root@hz-1 · through the agent")).toBeInTheDocument();
    expect(screen.queryByRole("tablist")).not.toBeInTheDocument();
    await waitFor(() => expect(sh.attaches).toHaveLength(1));
  });
});
