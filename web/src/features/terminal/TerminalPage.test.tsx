// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type HandlerContext } from "@connectrpc/connect";
import {
  type AttachTerminalRequest,
  AttachTerminalResponseSchema,
  ContainerState,
  DockerObjectKind,
  DockerObjectSchema,
  SavedCommandSchema,
  StepUpAction,
  TerminalCursorStyle,
  TerminalEndReason,
  TerminalEndSchema,
  TerminalPreferencesSchema,
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
const screens = vi.hoisted(() => ({
  all: [] as {
    written: string[];
    type: (s: string) => void;
    paste: (s: string) => void;
    modes: { applicationCursorKeysMode: boolean; bracketedPasteMode: boolean };
    keyHandler: ((e: KeyboardEvent) => boolean) | undefined;
    selected: string;
    options: Record<string, unknown>;
  }[],
}));

vi.mock("@xterm/xterm", () => {
  class Terminal {
    cols = 100;
    rows = 30;
    modes = { applicationCursorKeysMode: false, bracketedPasteMode: false };
    options: Record<string, unknown>;
    private onDataFn: (d: string) => void = () => {};
    private customKeyHandler: ((e: KeyboardEvent) => boolean) | undefined;
    private selection = "";
    written: string[] = [];
    constructor(options: Record<string, unknown>) {
      this.options = { ...options };
      screens.all.push({
        written: this.written,
        type: (s: string) => this.onDataFn(s),
        paste: (s: string) => this.paste(s),
        modes: this.modes,
        get keyHandler() {
          return undefined;
        },
        get selected() {
          return "";
        },
        options: this.options,
      });
      const slot = screens.all.at(-1);
      if (slot) {
        Object.defineProperty(slot, "keyHandler", {
          get: () => this.customKeyHandler,
          enumerable: true,
        });
      }
    }
    loadAddon() {}
    open() {}
    onData(fn: (d: string) => void) {
      this.onDataFn = fn;
    }
    onBinary() {}
    onResize() {}
    onSelectionChange() {}
    attachCustomKeyEventHandler(fn: (e: KeyboardEvent) => boolean) {
      this.customKeyHandler = fn;
    }
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
    paste(data: string) {
      const normalized = data.replace(/\r?\n/g, "\r");
      const payload = this.modes.bracketedPasteMode ? `\x1b[200~${normalized}\x1b[201~` : normalized;
      this.onDataFn(payload);
    }
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
const term = (id = "trm_1", title = "root@hz-1", container = "") =>
  create(TerminalSchema, {
    id,
    serverId: "srv_1",
    title,
    container,
    columns: 100,
    rows: 30,
    openTime: timestampFromDate(new Date(2026, 10, 7)),
  });

const runningContainer = (id: string, name: string, image: string) =>
  create(DockerObjectSchema, {
    key: { serverId: "srv_1", kind: DockerObjectKind.CONTAINER, engineId: id },
    name,
    state: ContainerState.RUNNING,
    detail: { case: "container", value: { image } },
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

function renderTerminal(terminals: NonNullable<Api["terminals"]>, extra?: Omit<Api, "servers" | "terminals">) {
  return renderAt("/servers/srv_1/terminal", { servers: { watchServers }, terminals, ...extra });
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
      { auth: { getAuthState: () => authState(), beginStepUp, finishStepUp: () => ({ proof: "proof_1" }) } },
    );
    await userEvent.click(await screen.findByRole("button", { name: "Open a terminal" }));
    expect(await screen.findByRole("tab", { name: "Host · hz-1", selected: true })).toBeInTheDocument();
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

  it("offers to add a passkey here when there's none for this address", async () => {
    renderTerminal(
      {
        listTerminals: () => ({}),
        openTerminal: () => Promise.reject(authError(Code.PermissionDenied, "needs_step_up")),
      },
      {
        auth: {
          getAuthState: () => authState(),
          beginStepUp: () => Promise.reject(authError(Code.FailedPrecondition, "no_passkey_here")),
        },
      },
    );
    await userEvent.click(await screen.findByRole("button", { name: "Open a terminal" }));
    expect(await screen.findByText("A terminal takes a passkey, and there's none for this address")).toBeVisible();
    expect(screen.getByRole("button", { name: "Add a passkey for this address" })).toBeVisible();
    expect(screen.queryByRole("tab")).not.toBeInTheDocument();
  });

  it("says when the passkey prompt closed", async () => {
    stubPasskeys({ get: "NotAllowedError" });
    renderTerminal(
      {
        listTerminals: () => ({}),
        openTerminal: () => Promise.reject(authError(Code.PermissionDenied, "needs_step_up")),
      },
      {
        auth: {
          getAuthState: () => authState(),
          beginStepUp: () => ({ ceremonyId: "cer_1", optionsJson: requestOptions }),
        },
      },
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
    expect(tabs.getAllByRole("tab").map((t) => t.textContent)).toEqual(["Host · hz-1", "Host (2) · hz-1"]);
    expect(tabs.getByRole("tab", { name: "Host (2) · hz-1" })).toHaveAttribute("aria-selected", "true");
    await waitFor(() => expect(sh.attaches.map((a) => a.terminalId)).toEqual(["trm_2"]));
    await userEvent.click(tabs.getByRole("tab", { name: "Host · hz-1" }));
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

  describe("coming back to the tab (#652)", () => {
    /** Goes to the server's Overview with its own tab, then back to Terminal, as the owner did. */
    const awayAndBack = async () => {
      const tabs = () => within(screen.getByRole("navigation", { name: "Server hz-1" }));
      await userEvent.click(tabs().getByRole("link", { name: "Overview" }));
      expect(await screen.findByRole("heading", { name: "What runs here" })).toBeInTheDocument();
      expect(screen.queryByRole("tablist", { name: "Sessions" })).toBeNull();
      await userEvent.click(tabs().getByRole("link", { name: "Terminal" }));
    };
    const sessionTabs = async () =>
      within(await screen.findByRole("tablist", { name: "Sessions" }))
        .getAllByRole("tab")
        .map((tab) => tab.textContent);

    it("leaves a terminal closed with its × closed, and the one still running comes back attached", async () => {
      const sh = shell();
      // The control plane lists both, then can't answer once the page comes back, so only
      // what the page kept of the list can bring a tab back.
      let reads = 0;
      const closeTerminal = vi.fn(() => ({}));
      renderTerminal({
        listTerminals: () => {
          reads++;
          if (reads > 1) throw new ConnectError("the control plane is busy", Code.Unavailable);
          return { terminals: [term("trm_1"), term("trm_2", "deploy@hz-1")] };
        },
        attachTerminal: sh.attachTerminal,
        writeTerminal: sh.writeTerminal,
        closeTerminal,
      });
      await waitFor(async () => expect(await sessionTabs()).toEqual(["Host · hz-1", "Host (2) · hz-1"]));
      await userEvent.click(screen.getByTitle("Close Host (2) · hz-1"));
      await waitFor(() =>
        expect(closeTerminal).toHaveBeenCalledWith(expect.objectContaining({ terminalId: "trm_2" }), expect.anything()),
      );
      await waitFor(async () => expect(await sessionTabs()).toEqual(["Host · hz-1"]));
      const attached = sh.attaches.length;
      const before = reads;

      await awayAndBack();
      await waitFor(() => expect(reads).toBeGreaterThan(before));
      await waitFor(async () => expect(await sessionTabs()).toEqual(["Host · hz-1"]));
      await waitFor(() => expect(sh.attaches.length).toBeGreaterThan(attached));
      expect(sh.attaches.at(-1)?.terminalId).toBe("trm_1");
      expect(await screen.findByText("Connected through the agent")).toBeInTheDocument();
      expect(screen.queryByText("This terminal has ended")).toBeNull();
    });

    it("keeps a terminal that ended on its own as a tab until it's closed, and doesn't bring it back", async () => {
      const sh = shell("logout\r\n", { reason: TerminalEndReason.EXITED, exitCode: 0 });
      let reads = 0;
      renderTerminal({
        listTerminals: () => {
          reads++;
          if (reads > 1) throw new ConnectError("the control plane is busy", Code.Unavailable);
          return { terminals: [term()] };
        },
        attachTerminal: sh.attachTerminal,
      });
      expect(await screen.findByText("The shell exited")).toBeInTheDocument();
      // The ended tab stays while the page is up.
      expect(await sessionTabs()).toEqual(["Host · hz-1"]);
      const before = reads;

      await awayAndBack();
      await waitFor(() => expect(reads).toBeGreaterThan(before));
      expect(await screen.findByText("No terminal open", { selector: "h2" })).toBeInTheDocument();
      expect(screen.queryByRole("tablist", { name: "Sessions" })).toBeNull();
    });

    it("brings back only what the control plane still lists, not what the page kept", async () => {
      const sh = shell();
      // deploy@hz-1 isn't on screen, so the page never sees it end; the control plane does.
      let open = [term("trm_2", "deploy@hz-1"), term("trm_1")];
      renderTerminal({
        listTerminals: () => ({ terminals: open }),
        attachTerminal: sh.attachTerminal,
        writeTerminal: sh.writeTerminal,
      });
      await waitFor(async () => expect(await sessionTabs()).toEqual(["Host · hz-1", "Host (2) · hz-1"]));
      await waitFor(() => expect(sh.attaches.map((a) => a.terminalId)).toEqual(["trm_1"]));
      open = [term("trm_1")];

      await awayAndBack();
      await waitFor(() => expect(sh.attaches.map((a) => a.terminalId)).toEqual(["trm_1", "trm_1"]));
      expect(await sessionTabs()).toEqual(["Host · hz-1"]);
    });
  });

  it("offers a shell in the container the page was asked for, and opens one with no passkey where none is needed", async () => {
    const sh = shell("$ ");
    const openTerminal = vi.fn(() => ({ terminal: term("trm_9", "node@web-1", "web-1") }));
    renderAt("/servers/srv_1/terminal?container=web-1", {
      servers: { watchServers },
      terminals: { listTerminals: () => ({ terminals: [term()] }), openTerminal, attachTerminal: sh.attachTerminal },
    });
    expect(await screen.findByRole("heading", { name: "No shell open in web-1" })).toBeInTheDocument();
    expect(screen.getByText(/runs in the container, as its own user/)).toHaveTextContent(
      "A shell in web-1 runs in the container, as its own user, with the first of bash, ash and sh its image has. One in a container that reaches as far as root on hz-1 asks for your passkey.",
    );
    // The server's own terminal is there to go back to, and nothing is attached meanwhile.
    expect(screen.getByRole("tab", { name: "Host · hz-1" })).toHaveAttribute("aria-selected", "false");
    expect(sh.attaches).toHaveLength(0);
    await userEvent.click(screen.getByRole("button", { name: "Open a shell" }));
    expect(await screen.findByRole("tab", { name: "web-1 · hz-1", selected: true })).toBeInTheDocument();
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
      .mockResolvedValueOnce({ terminal: term("trm_9", "root@admin-1", "admin-1") });
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
    expect(await screen.findByRole("tab", { name: "admin-1 · hz-1", selected: true })).toBeInTheDocument();
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
    await screen.findByText("Host · hz-1 · through the agent");
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

  it("takes the phone's screen under a header of its own, with the server's tabs a tap away (#795)", async () => {
    onPhone();
    const sh = shell();
    renderTerminal({
      listTerminals: () => ({ terminals: [term()] }),
      attachTerminal: sh.attachTerminal,
      writeTerminal: sh.writeTerminal,
    });
    expect(await screen.findByRole("heading", { level: 1, name: "Terminal" })).toBeInTheDocument();
    expect(await screen.findByText("Host · hz-1 · through the agent")).toBeInTheDocument();
    expect(screen.queryByRole("tablist")).not.toBeInTheDocument();
    const tabs = screen.getByRole("navigation", { name: "Server hz-1" });
    expect(within(tabs).getByRole("link", { name: "Terminal" })).toHaveAttribute("aria-current", "page");
    expect(within(tabs).getByRole("link", { name: "Overview" })).toBeVisible();
    await waitFor(() => expect(sh.attaches).toHaveLength(1));
  });

  it("+ picks host first or a running container, with search when there are many", async () => {
    const sh = shell();
    const openTerminal = vi.fn(() => ({ terminal: term("trm_c1", "node@api-1", "api-1") }));
    const containers = [
      runningContainer("cid_1", "api-1", "acme/api:2"),
      runningContainer("cid_2", "cache-1", "redis:7"),
      runningContainer("cid_3", "db-1", "postgres:16"),
      runningContainer("cid_4", "proxy-1", "caddy:2"),
      runningContainer("cid_5", "worker-1", "acme/worker:2"),
    ];
    renderTerminal(
      { listTerminals: () => ({ terminals: [term()] }), openTerminal, attachTerminal: sh.attachTerminal },
      { docker: { listDockerObjects: () => ({ objects: containers }) } },
    );
    await screen.findByText("Connected through the agent");
    await userEvent.click(screen.getByRole("button", { name: "New session" }));
    const picker = await screen.findByRole("dialog", { name: "New session" });
    const items = within(picker).getAllByRole("button");
    expect(items[0]).toHaveTextContent("Host shell");
    expect(within(picker).getByText("acme/api:2")).toBeInTheDocument();
    const filter = within(picker).getByRole("textbox", { name: "Filter containers" });
    await expectAccessible(document.body);

    await userEvent.type(filter, "acme/api");
    expect(within(picker).getByText("api-1")).toBeInTheDocument();
    expect(within(picker).queryByText("redis:7")).toBeNull();

    await userEvent.click(within(picker).getByRole("button", { name: /api-1/ }));
    await waitFor(() =>
      expect(openTerminal).toHaveBeenCalledWith(
        expect.objectContaining({ serverId: "srv_1", containerId: "cid_1" }),
        expect.anything(),
      ),
    );
    expect(await screen.findByRole("tab", { name: "api-1 · hz-1", selected: true })).toBeInTheDocument();
  });

  it("saves, inserts without Enter, runs with Enter, warns on secrets, and deletes saved commands", async () => {
    const sh = shell();
    let stored = [
      create(SavedCommandSchema, {
        id: "tcm_1",
        name: "Check disk",
        command: "df -h",
        group: "Disk",
        shared: false,
      }),
      create(SavedCommandSchema, {
        id: "tcm_2",
        name: "Running containers",
        command: "docker ps",
        group: "Docker",
        shared: true,
      }),
    ];
    const saveCommand = vi.fn((req: { id: string; name: string; command: string; group: string; shared: boolean }) => {
      const cmd = create(SavedCommandSchema, {
        id: req.id || `tcm_${stored.length + 1}`,
        name: req.name,
        command: req.command,
        group: req.group,
        shared: req.shared,
      });
      stored = [...stored.filter((c) => c.id !== cmd.id), cmd];
      return { command: cmd };
    });
    const deleteSavedCommand = vi.fn((req: { id: string }) => {
      stored = stored.filter((c) => c.id !== req.id);
      return {};
    });

    renderTerminal({
      listTerminals: () => ({ terminals: [term()] }),
      attachTerminal: sh.attachTerminal,
      writeTerminal: sh.writeTerminal,
      listSavedCommands: () => ({ commands: stored }),
      saveCommand,
      deleteSavedCommand,
    });
    await screen.findByText("Connected through the agent");

    await userEvent.click(screen.getByRole("button", { name: "Commands" }));
    const aside = await screen.findByRole("complementary", { name: "Saved commands" });
    expect(within(aside).getByText("Disk")).toBeInTheDocument();
    expect(within(aside).getByText("Docker")).toBeInTheDocument();
    expect(within(aside).getByText("Shared", { selector: ".dw-tag" })).toBeInTheDocument();
    await expectAccessible(document.body);

    // Insert never presses Enter; Run pastes and sends \r.
    const written = () => sh.writes.map((w) => new TextDecoder().decode(w.data)).filter((s) => s.length > 0);
    await userEvent.click(within(aside).getByRole("button", { name: "Insert Check disk" }));
    await waitFor(() => expect(written()).toEqual(["df -h"]));
    await userEvent.click(within(aside).getByRole("button", { name: "Run Running containers" }));
    await waitFor(() => expect(written()).toEqual(["df -h", "docker ps", "\r"]));

    // H2: Editor refuses control characters (ESC, ^C, DEL) live and disables Save.
    await userEvent.click(within(aside).getByRole("button", { name: "Add" }));
    const ctrlDialog = await screen.findByRole("dialog", { name: "Add saved command" });
    await userEvent.type(within(ctrlDialog).getByLabelText("Name"), "Bad control");
    await userEvent.click(within(ctrlDialog).getByLabelText("Command"));
    await userEvent.paste("echo \x1b[31mred");
    expect(within(ctrlDialog).getByText("Control characters aren't allowed")).toBeInTheDocument();
    expect(within(ctrlDialog).getByRole("button", { name: "Save command" })).toBeDisabled();
    await userEvent.click(within(ctrlDialog).getByRole("button", { name: "Cancel" }));

    // Secret guard warns concisely (one sentence, no unactionable link) and blocks save until acknowledged.
    await userEvent.click(within(aside).getByRole("button", { name: "Add" }));
    const dialog = await screen.findByRole("dialog", { name: "Add saved command" });
    await userEvent.type(within(dialog).getByLabelText("Name"), "Prod token");
    await userEvent.type(within(dialog).getByLabelText("Command"), "export API_TOKEN=sk_live_1234567890abcdef");
    expect(within(dialog).getByText("This looks like a secret")).toBeInTheDocument();
    expect(within(dialog).getByText("Kept in plain text in Dockwell's database.")).toBeInTheDocument();
    expect(within(dialog).queryByRole("link", { name: /Variables/ })).toBeNull();
    const saveBtn = within(dialog).getByRole("button", { name: "Save command" });
    expect(saveBtn).toBeDisabled();
    await userEvent.click(within(dialog).getByRole("checkbox", { name: /store this command in plain text anyway/i }));
    expect(saveBtn).toBeEnabled();
    await userEvent.click(saveBtn);
    await waitFor(() =>
      expect(saveCommand).toHaveBeenCalledWith(
        expect.objectContaining({
          name: "Prod token",
          allowPlainTextSecret: true,
        }),
        expect.anything(),
      ),
    );
    expect(await within(aside).findByText("Prod token")).toBeInTheDocument();

    // Delete a personal saved command -> shows Undo toast and clicking Undo re-saves it.
    await userEvent.click(within(aside).getByRole("button", { name: "Options for Check disk" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Delete" }));
    await waitFor(() =>
      expect(deleteSavedCommand).toHaveBeenCalledWith(
        expect.objectContaining({ id: "tcm_1", shared: false }),
        expect.anything(),
      ),
    );
    const undoBtn = await screen.findByRole("button", { name: "Undo" });
    await userEvent.click(undoBtn);
    await waitFor(() =>
      expect(saveCommand).toHaveBeenLastCalledWith(
        expect.objectContaining({ name: "Check disk", command: "df -h", shared: false }),
        expect.anything(),
      ),
    );

    // Delete a shared saved command -> asks for confirmation first.
    await userEvent.click(within(aside).getByRole("button", { name: "Options for Running containers" }));
    await userEvent.click(await screen.findByRole("menuitem", { name: "Delete" }));
    const confirmDelete = await screen.findByRole("dialog", { name: "Delete shared command?" });
    expect(deleteSavedCommand).toHaveBeenCalledTimes(1);
    await userEvent.click(within(confirmDelete).getByRole("button", { name: "Delete command" }));
    await waitFor(() =>
      expect(deleteSavedCommand).toHaveBeenCalledWith(
        expect.objectContaining({ id: "tcm_2", shared: true }),
        expect.anything(),
      ),
    );
  });

  it("applies display settings live and saves them through the API", async () => {
    const sh = shell();
    let prefs = create(TerminalPreferencesSchema, {});
    const updateTerminalPreferences = vi.fn((req: { preferences?: typeof prefs }) => {
      prefs = req.preferences ?? create(TerminalPreferencesSchema, {});
      return { preferences: prefs };
    });
    renderTerminal({
      listTerminals: () => ({ terminals: [term()] }),
      attachTerminal: sh.attachTerminal,
      writeTerminal: sh.writeTerminal,
      getTerminalPreferences: () => ({ preferences: prefs }),
      updateTerminalPreferences,
    });
    await screen.findByText("Connected through the agent");

    await userEvent.click(screen.getByRole("button", { name: "Terminal settings" }));
    const panel = await screen.findByRole("region", { name: "Terminal settings" });
    await expectAccessible(document.body);

    await userEvent.selectOptions(within(panel).getByLabelText("Font size"), "16");
    await waitFor(() => expect(screens.all[0]?.options["fontSize"]).toBe(16));
    expect(updateTerminalPreferences).toHaveBeenCalledWith(
      expect.objectContaining({
        preferences: expect.objectContaining({ fontSize: 16 }),
      }),
      expect.anything(),
    );

    await userEvent.click(within(panel).getByRole("radio", { name: "Bar" }));
    await waitFor(() => expect(screens.all[0]?.options["cursorStyle"]).toBe("bar"));
    expect(updateTerminalPreferences).toHaveBeenLastCalledWith(
      expect.objectContaining({
        preferences: expect.objectContaining({ cursorStyle: TerminalCursorStyle.BAR }),
      }),
      expect.anything(),
    );
  });

  it("keeps bare Escape for the shell in fullscreen, leaves on Shift+Escape or outside Escape, and opens Files at cwd", async () => {
    const sh = shell();
    const listFiles = vi.fn(() => ({ path: "/var/log", entries: [] }));
    renderTerminal(
      {
        listTerminals: () => ({ terminals: [term()] }),
        attachTerminal: sh.attachTerminal,
        writeTerminal: sh.writeTerminal,
        getTerminalWorkingDirectory: () => ({ path: "/var/log", known: true, home: "/root" }),
      },
      { files: { listFiles } },
    );
    await screen.findByText("Connected through the agent");

    await userEvent.click(screen.getByRole("button", { name: "Fullscreen" }));
    const exitBtn = screen.getByRole("button", { name: "Exit fullscreen" });
    expect(exitBtn).toHaveAttribute("aria-pressed", "true");
    expect(exitBtn).toHaveAttribute("title", "Exit fullscreen (Shift+Esc)");
    expect(document.querySelector(".terminal-fullscreen")).not.toBeNull();

    // H1: Bare Escape inside xterm is NOT intercepted by fullscreen; it reaches the shell.
    const bareEsc = new KeyboardEvent("keydown", { key: "Escape", bubbles: true });
    expect(screens.all[0]?.keyHandler?.(bareEsc)).toBe(true);
    const termSection = document.querySelector(".terminal-screen");
    termSection?.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    expect(document.querySelector(".terminal-fullscreen")).not.toBeNull();

    // Shift+Escape leaves fullscreen even when xterm handles the key.
    const shiftEsc = new KeyboardEvent("keydown", { key: "Escape", shiftKey: true, bubbles: true });
    expect(screens.all[0]?.keyHandler?.(shiftEsc)).toBe(false);
    await waitFor(() => expect(document.querySelector(".terminal-fullscreen")).toBeNull());

    // Bare Escape when focus is outside the terminal still leaves fullscreen.
    await userEvent.click(screen.getByRole("button", { name: "Fullscreen" }));
    expect(document.querySelector(".terminal-fullscreen")).not.toBeNull();
    await userEvent.keyboard("{Escape}");
    expect(document.querySelector(".terminal-fullscreen")).toBeNull();

    await userEvent.click(screen.getByRole("button", { name: "Open Files here" }));
    await waitFor(() =>
      expect(listFiles).toHaveBeenCalledWith(
        expect.objectContaining({ serverId: "srv_1", path: "/var/log" }),
        expect.anything(),
      ),
    );
  });

  it("broadcasts typing only to opted-in tabs and turns off when tabs change", async () => {
    const sh = shell();
    const closeTerminal = vi.fn(() => ({}));
    renderTerminal({
      listTerminals: () => ({ terminals: [term("trm_1"), term("trm_2"), term("trm_3")] }),
      attachTerminal: sh.attachTerminal,
      writeTerminal: sh.writeTerminal,
      closeTerminal,
    });
    const tabs = within(await screen.findByRole("tablist", { name: "Sessions" }));
    await waitFor(() => expect(tabs.getAllByRole("tab")).toHaveLength(3));
    await screen.findByText("Connected through the agent");

    // Broadcast is off by default; turn it on and opt in only Host (3) (active) and Host (1).
    await userEvent.click(screen.getByRole("button", { name: "Type into several tabs" }));
    const bar = await screen.findByRole("region", { name: "Type into several tabs" });
    await expectAccessible(document.body);
    const checkboxes = within(bar).getAllByRole("checkbox");
    expect(checkboxes[0]).not.toBeChecked();
    expect(checkboxes[1]).not.toBeChecked();
    expect(checkboxes[2]).toBeChecked();

    const first = checkboxes[0];
    expect(first).toBeDefined();
    if (first) await userEvent.click(first);
    expect(within(bar).getByText("Typing into 2 tabs")).toBeInTheDocument();
    // Both opted-in tabs attach so writes reach both; the non-opted-in tab stays unattached.
    await waitFor(() => expect(sh.attaches.map((a) => a.terminalId)).toEqual(["trm_3", "trm_1"]));

    // Typing into the active tab mirrors to trm_1, never to trm_2.
    const activeScreen = screens.all[2];
    activeScreen?.type("whoami\r");
    await waitFor(() => {
      const byId = sh.writes
        .filter((w) => new TextDecoder().decode(w.data) === "whoami\r")
        .map((w) => w.terminalId)
        .sort();
      expect(byId).toEqual(["trm_1", "trm_3"]);
    });

    // Switching to the non-opted-in tab automatically turns broadcast off.
    await userEvent.click(tabs.getByRole("tab", { name: "Host (2) · hz-1" }));
    expect(screen.queryByRole("region", { name: "Type into several tabs" })).toBeNull();

    // Turning broadcast on again and closing a tab turns it off automatically.
    await userEvent.click(screen.getByRole("button", { name: "Type into several tabs" }));
    expect(await screen.findByRole("region", { name: "Type into several tabs" })).toBeInTheDocument();
    await userEvent.click(screen.getByTitle("Close Host (3) · hz-1"));
    await waitFor(() => expect(screen.queryByRole("region", { name: "Type into several tabs" })).toBeNull());
  });

  it("sends saved commands to exactly the ticked broadcast tabs named in the panel (M1) and uses bracketed paste / confirms multiline (H2)", async () => {
    const sh = shell();
    const saved = [
      create(SavedCommandSchema, {
        id: "tcm_1",
        name: "Check",
        command: "uptime",
        shared: true,
        createdBy: "Mira Chen",
        updatedBy: "Ada Lovelace",
      }),
      create(SavedCommandSchema, {
        id: "tcm_2",
        name: "Multi",
        command: "cd /srv\ndocker ps",
        shared: false,
      }),
    ];
    renderTerminal({
      listTerminals: () => ({ terminals: [term("trm_1"), term("trm_2")] }),
      attachTerminal: sh.attachTerminal,
      writeTerminal: sh.writeTerminal,
      listSavedCommands: () => ({ commands: saved }),
    });
    await screen.findByText("Connected through the agent");

    // Turn on broadcast (trm_2 active & ticked), tick trm_1, then untick trm_2 so only trm_1 is ticked.
    await userEvent.click(screen.getByRole("button", { name: "Type into several tabs" }));
    const bar = await screen.findByRole("region", { name: "Type into several tabs" });
    const [cb0, cb1] = within(bar).getAllByRole("checkbox");
    if (!cb0 || !cb1) throw new Error("expected 2 broadcast checkboxes");
    await userEvent.click(cb0);
    await userEvent.click(cb1);

    await userEvent.click(screen.getByRole("button", { name: "Commands" }));
    const aside = await screen.findByRole("complementary", { name: "Saved commands" });
    // M4: shared command shows author/editor.
    expect(within(aside).getByText("by Ada Lovelace")).toBeInTheDocument();
    // M1: panel target names Host · hz-1 (trm_1), NOT Host (2) · hz-1 (trm_2).
    expect(aside).toHaveTextContent("Target: Host · hz-1");

    // Run goes ONLY to trm_1.
    await userEvent.click(within(aside).getByRole("button", { name: "Run Check" }));
    await waitFor(() => {
      const ids = sh.writes.filter((w) => new TextDecoder().decode(w.data).includes("uptime")).map((w) => w.terminalId);
      expect(ids).toEqual(["trm_1"]);
    });

    // Tick trm_2 as well so both tabs are ticked -> label names both.
    await userEvent.click(cb1);
    expect(aside).toHaveTextContent("2 tabs: Host, Host (2)");

    // H2: Multi-line Insert is refused when bracketed paste isn't enabled on targets.
    await userEvent.click(within(aside).getByRole("button", { name: "Insert Multi" }));
    expect(await within(aside).findByText(/bracketed paste isn't enabled/i)).toBeInTheDocument();
    expect(sh.writes.some((w) => new TextDecoder().decode(w.data).includes("cd /srv"))).toBe(false);

    // Enable bracketed paste on both targets -> Insert sends bracketed paste without trailing Enter to both!
    const [s0, s1] = screens.all;
    if (!s0 || !s1) throw new Error("expected 2 terminal screens");
    s0.modes.bracketedPasteMode = true;
    s1.modes.bracketedPasteMode = true;
    await userEvent.click(within(aside).getByRole("button", { name: "Insert Multi" }));
    await waitFor(() => {
      const pasted = sh.writes.filter(
        (w) => new TextDecoder().decode(w.data) === "\x1b[200~cd /srv\rdocker ps\x1b[201~",
      );
      expect(pasted.map((w) => w.terminalId).sort()).toEqual(["trm_1", "trm_2"]);
    });

    // H2: Multi-line Run asks first naming the line count before sending.
    const beforeRun = sh.writes.length;
    await userEvent.click(within(aside).getByRole("button", { name: "Run Multi" }));
    const confirmDialog = await screen.findByRole("dialog", { name: "Run 2 lines?" });
    expect(sh.writes.length).toBe(beforeRun);
    await userEvent.click(within(confirmDialog).getByRole("button", { name: "Run 2 lines" }));
    await waitFor(() => {
      const after = sh.writes
        .slice(beforeRun)
        .map((w) => ({ id: w.terminalId, data: new TextDecoder().decode(w.data) }));
      expect(after.some((w) => w.id === "trm_1" && w.data.includes("\r"))).toBe(true);
      expect(after.some((w) => w.id === "trm_2" && w.data.includes("\r"))).toBe(true);
    });
  });

  it("reopens an ended container shell for that container rather than the URL's container (M2) and disables Files on container tabs (L1)", async () => {
    let openCount = 0;
    const openTerminal = vi.fn((req: { containerId?: string }) => {
      openCount++;
      if (req.containerId === "cid_y") {
        return { terminal: term(`trm_y${openCount}`, "node@ctr-y", "ctr-y") };
      }
      return { terminal: term(`trm_x${openCount}`, "node@ctr-x", "ctr-x") };
    });
    let attachCalls = 0;
    const attachTerminal = async function* (req: AttachTerminalRequest, ctx: HandlerContext) {
      attachCalls++;
      yield create(AttachTerminalResponseSchema, { attached: true, offset: 0n, inputOffset: 0n });
      // First attachment to ctr-y ends immediately.
      if (req.terminalId === "trm_y1" && attachCalls === 1) {
        yield create(AttachTerminalResponseSchema, {
          end: create(TerminalEndSchema, { reason: TerminalEndReason.EXITED, exitCode: 0 }),
        });
        return;
      }
      await untilAborted(ctx.signal);
    };

    renderAt("/servers/srv_1/terminal?containerId=cid_x&containerTitle=ctr-x", {
      servers: { watchServers },
      terminals: { listTerminals: () => ({}), openTerminal, attachTerminal },
      docker: { listDockerObjects: () => ({ objects: [runningContainer("cid_y", "ctr-y", "alpine:3")] }) },
    });
    expect(await screen.findByRole("heading", { name: "No shell open in ctr-x" })).toBeInTheDocument();

    // Open ctr-y via + picker.
    await userEvent.click(screen.getByRole("button", { name: "New session" }));
    const picker = await screen.findByRole("dialog", { name: "New session" });
    // N3: Host shell sublabel shows server name without assuming root.
    expect(within(picker).queryByText(/· root/)).toBeNull();
    await userEvent.click(within(picker).getByRole("button", { name: /ctr-y/ }));

    // L1: Open Files here is disabled on container tabs with an explanation.
    const filesBtn = await screen.findByRole("button", { name: "Open Files here" });
    expect(filesBtn).toBeDisabled();
    expect(filesBtn).toHaveAttribute("title", "Files shows hz-1, not files inside ctr-y");

    // ctr-y exits -> clicking "Open a new terminal" reopens cid_y, NOT cid_x!
    await screen.findByText("The shell exited");
    await userEvent.click(screen.getByRole("button", { name: "Open a new terminal" }));
    await waitFor(() => expect(openTerminal).toHaveBeenCalledTimes(2));
    expect(openTerminal).toHaveBeenLastCalledWith(
      expect.objectContaining({ serverId: "srv_1", containerId: "cid_y" }),
      expect.anything(),
    );
  });

  it("defaults import to personal, lists all commands without truncation, marks shared, and requires explicit confirmation for shared (M3)", async () => {
    const sh = shell();
    const importSavedCommands = vi.fn(() => ({ commands: [], imported: 25 }));
    renderTerminal({
      listTerminals: () => ({ terminals: [term()] }),
      attachTerminal: sh.attachTerminal,
      listSavedCommands: () => ({ commands: [] }),
      importSavedCommands,
    });
    await screen.findByText("Connected through the agent");

    await userEvent.click(screen.getByRole("button", { name: "Commands" }));
    const aside = await screen.findByRole("complementary", { name: "Saved commands" });

    const payload = {
      version: 1,
      commands: Array.from({ length: 25 }, (_, i) => ({
        name: `Cmd ${i + 1}`,
        command: `echo ${i + 1}`,
        group: "Batch",
        shared: i === 0,
      })),
    };
    const file = new File([JSON.stringify(payload)], "cmds.json", { type: "application/json" });
    const input = aside.querySelector<HTMLInputElement>('input[type="file"]');
    if (!input) throw new Error("expected file input");
    await userEvent.upload(input, file);

    const dialog = await screen.findByRole("dialog", { name: "Import saved commands" });
    // Default is "All personal".
    expect(within(dialog).getByRole("radio", { name: "All personal" })).toBeChecked();
    // All 25 commands are listed (no slice at 20).
    expect(within(dialog).getByText("Cmd 25")).toBeInTheDocument();
    expect(within(dialog).queryByText(/And \d+ more/)).toBeNull();
    // Shared item from file is tagged.
    expect(within(dialog).getByText("Shared in file")).toBeInTheDocument();

    // Switching to "Keep file scope" marks shared and requires explicit confirmation checkbox.
    await userEvent.click(within(dialog).getByRole("radio", { name: "Keep file scope" }));
    const importBtn = within(dialog).getByRole("button", { name: "Import 25 commands" });
    expect(importBtn).toBeDisabled();
    await userEvent.click(within(dialog).getByRole("checkbox", { name: /Share 1 command with all admins/i }));
    expect(importBtn).toBeEnabled();
    await userEvent.click(importBtn);
    await waitFor(() => expect(importSavedCommands).toHaveBeenCalledTimes(1));
  });

  it("re-encodes mirrored pastes and cursor keys per target terminal modes (L2)", async () => {
    const sh = shell();
    renderTerminal({
      listTerminals: () => ({ terminals: [term("trm_1"), term("trm_2")] }),
      attachTerminal: sh.attachTerminal,
      writeTerminal: sh.writeTerminal,
    });
    await screen.findByText("Connected through the agent");

    // Turn on broadcast for both tabs (trm_2 active).
    await userEvent.click(screen.getByRole("button", { name: "Type into several tabs" }));
    const bar = await screen.findByRole("region", { name: "Type into several tabs" });
    const firstBox = within(bar).getAllByRole("checkbox")[0];
    if (!firstBox) throw new Error("expected broadcast checkbox");
    await userEvent.click(firstBox);
    await waitFor(() => expect(sh.attaches).toHaveLength(2));

    // Source (trm_2) has bracketedPasteMode=true & applicationCursorKeysMode=true; target (trm_1) has both false.
    const [s0, s1] = screens.all;
    if (!s0 || !s1) throw new Error("expected 2 terminal screens");
    s0.modes.bracketedPasteMode = false;
    s0.modes.applicationCursorKeysMode = false;
    s1.modes.bracketedPasteMode = true;
    s1.modes.applicationCursorKeysMode = true;

    // Paste into trm_2 -> trm_2 gets bracketed paste, trm_1 gets plain paste without \x1b[200~.
    s1.paste("hello world");
    await waitFor(() => {
      const t1 = sh.writes.filter((w) => w.terminalId === "trm_1").map((w) => new TextDecoder().decode(w.data));
      const t2 = sh.writes.filter((w) => w.terminalId === "trm_2").map((w) => new TextDecoder().decode(w.data));
      expect(t2).toContain("\x1b[200~hello world\x1b[201~");
      expect(t1).toContain("hello world");
      expect(t1.some((d) => d.includes("\x1b[200~"))).toBe(false);
    });

    // Up arrow in trm_2 (\x1bOA) -> trm_1 receives normal cursor mode (\x1b[A).
    s1.type("\x1bOA");
    await waitFor(() => {
      const t1 = sh.writes.filter((w) => w.terminalId === "trm_1").map((w) => new TextDecoder().decode(w.data));
      expect(t1).toContain("\x1b[A");
    });
  });

  it("keeps phone Aa and Settings font size in sync (L3) and serializes rapid Settings saves (L4)", async () => {
    onPhone();
    const sh = shell();
    let resolveFirst!: (v: { preferences: ReturnType<typeof create<typeof TerminalPreferencesSchema>> }) => void;
    let callNum = 0;
    const updateTerminalPreferences = vi.fn(
      (req: { preferences?: ReturnType<typeof create<typeof TerminalPreferencesSchema>> }) => {
        callNum++;
        const p = req.preferences ?? create(TerminalPreferencesSchema, {});
        if (callNum === 1) {
          return new Promise<{ preferences: typeof p }>((resolve) => {
            resolveFirst = resolve;
          });
        }
        return { preferences: p };
      },
    );

    renderTerminal({
      listTerminals: () => ({ terminals: [term()] }),
      attachTerminal: sh.attachTerminal,
      writeTerminal: sh.writeTerminal,
      getTerminalPreferences: () => ({ preferences: create(TerminalPreferencesSchema, {}) }),
      updateTerminalPreferences,
    });
    await screen.findByText("Connected through the agent");

    // Clicking Aa cycles from 13 -> 15 and saves through preferences (L3).
    await userEvent.click(screen.getByRole("button", { name: "Text size" }));
    await waitFor(() => expect(screens.all[0]?.options["fontSize"]).toBe(15));
    expect(updateTerminalPreferences).toHaveBeenCalledTimes(1);

    // While first save (15) is still in flight, pick 18 in Settings (L4).
    await userEvent.click(screen.getByRole("button", { name: "Terminal settings" }));
    const sheet = await screen.findByRole("dialog", { name: "Terminal settings" });
    expect(within(sheet).getByLabelText("Font size")).toHaveValue("15");
    await userEvent.selectOptions(within(sheet).getByLabelText("Font size"), "18");
    expect(screens.all[0]?.options["fontSize"]).toBe(18);

    // Resolve the first (older) save with 15 -> UI must NOT revert from 18 back to 15!
    resolveFirst({ preferences: create(TerminalPreferencesSchema, { fontSize: 15 }) });
    await waitFor(() => expect(updateTerminalPreferences).toHaveBeenCalledTimes(2));
    expect(screens.all[0]?.options["fontSize"]).toBe(18);
    expect(within(sheet).getByLabelText("Font size")).toHaveValue("18");
  });

  it("pages through containers past 100 and forwards search to the server in the + picker (L6)", async () => {
    const sh = shell();
    const listDockerObjects = vi.fn((req: { pageToken?: string; filter?: { search?: string } }) => {
      if (req.filter?.search) {
        return { objects: [runningContainer("cid_105", "zebra-105", "nginx:1")], nextPageToken: "" };
      }
      if (!req.pageToken) {
        return {
          objects: [runningContainer("cid_1", "alpha-1", "alpine:3")],
          nextPageToken: "page-2",
        };
      }
      return {
        objects: [runningContainer("cid_2", "beta-2", "redis:7")],
        nextPageToken: "",
      };
    });

    renderTerminal(
      { listTerminals: () => ({ terminals: [term()] }), attachTerminal: sh.attachTerminal },
      { docker: { listDockerObjects } },
    );
    await screen.findByText("Connected through the agent");

    await userEvent.click(screen.getByRole("button", { name: "New session" }));
    const picker = await screen.findByRole("dialog", { name: "New session" });
    expect(await within(picker).findByText("alpha-1")).toBeInTheDocument();
    expect(await within(picker).findByText("beta-2")).toBeInTheDocument();
    expect(listDockerObjects).toHaveBeenCalledTimes(2);
  });

  it("auto-opens a database console in one click when console=true is in the URL", async () => {
    const sh = shell("shop=# ");
    const openTerminal = vi.fn(() => ({
      terminal: { ...term("trm_db", "psql/shop@pg-1"), container: "pg-1" },
    }));
    renderAt("/servers/srv_1/terminal?container=pg-1&console=true&database=shop", {
      auth: { getAuthState: () => authState() },
      servers: { watchServers },
      terminals: {
        listTerminals: () => ({ terminals: [] }),
        openTerminal,
        attachTerminal: sh.attachTerminal,
        writeTerminal: sh.writeTerminal,
      },
    });
    await waitFor(() =>
      expect(openTerminal).toHaveBeenCalledWith(
        expect.objectContaining({
          serverId: "srv_1",
          container: "pg-1",
          databaseConsole: true,
          database: "shop",
        }),
        expect.anything(),
      ),
    );
    expect(await screen.findByRole("tab", { name: "pg-1 · hz-1" })).toHaveAttribute("title", "psql/shop@pg-1 · hz-1");
    await waitFor(() => expect(screens.all.at(-1)?.written.join("")).toBe("shop=# "));
  });
});
