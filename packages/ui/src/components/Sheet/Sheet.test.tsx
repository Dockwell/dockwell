// SPDX-License-Identifier: AGPL-3.0-only

import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useRef, useState } from "react";
import { describe, expect, it } from "vitest";

import { englishStrings, UIStringsProvider } from "../../lib/strings";
import { expectAccessible } from "../../test/a11y";
import { Button } from "../Button/Button";
import { CommandBar } from "../CommandBar/CommandBar";
import { TextField } from "../TextField/TextField";
import { Sheet } from "./Sheet";

function PlanSheet({ withCommandBar = false }: { withCommandBar?: boolean }) {
  const [open, setOpen] = useState(false);
  const [commands, setCommands] = useState(false);
  return (
    <main>
      <p>Behind the sheet</p>
      <Button onClick={() => setOpen(true)}>Review plan</Button>
      <Sheet
        open={open}
        onClose={() => setOpen(false)}
        title="Review plan"
        subtitle="Apply 3 security updates to hz-1"
        footer={
          <>
            <span className="dw-sheet-note">About 40 s. Undo within 24 h.</span>
            <Button onClick={() => setOpen(false)}>Cancel</Button>
            <Button variant="primary">Apply plan</Button>
          </>
        }
      >
        <TextField label="Reason" />
        {withCommandBar ? (
          <>
            <Button onClick={() => setCommands(true)}>Find</Button>
            <CommandBar
              open={commands}
              onClose={() => setCommands(false)}
              items={[{ id: "a", label: "Open activity" }]}
            />
          </>
        ) : null}
      </Sheet>
    </main>
  );
}

/** Opens a sheet and a command bar inside it with one click, so both layers mount in the same render. */
function SheetWithCommandBar() {
  const [open, setOpen] = useState(false);
  const [commands, setCommands] = useState(false);
  return (
    <main>
      <Button
        onClick={() => {
          setOpen(true);
          setCommands(true);
        }}
      >
        Find in plan
      </Button>
      <Sheet open={open} onClose={() => setOpen(false)} title="Review plan">
        <TextField label="Reason" />
        <CommandBar open={commands} onClose={() => setCommands(false)} items={[{ id: "a", label: "Open activity" }]} />
      </Sheet>
    </main>
  );
}

describe("Sheet", () => {
  it("renders nothing while closed", () => {
    render(<Sheet title="Review plan" />);
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("opens in a portal as a modal dialog named by its title and described by its subtitle", async () => {
    const user = userEvent.setup();
    const { container } = render(<PlanSheet />);
    await user.click(screen.getByRole("button", { name: "Review plan" }));
    const dialog = screen.getByRole("dialog", { name: "Review plan" });
    expect(dialog).toHaveAttribute("aria-modal", "true");
    expect(dialog).toHaveAccessibleDescription("Apply 3 security updates to hz-1");
    expect(container.contains(dialog)).toBe(false);
    expect(dialog).toHaveFocus();
    expect(document.body.style.overflow).toBe("hidden");
    expect(screen.getByText("About 40 s. Undo within 24 h.")).toHaveClass("dw-sheet-note");
    await expectAccessible(document.body);
  });

  it("keeps Tab and Shift+Tab inside, wrapping at both ends", async () => {
    const user = userEvent.setup();
    render(<PlanSheet />);
    await user.click(screen.getByRole("button", { name: "Review plan" }));
    await user.tab();
    expect(screen.getByRole("button", { name: "Close" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("textbox", { name: "Reason" })).toHaveFocus();
    await user.tab();
    await user.tab();
    expect(screen.getByRole("button", { name: "Apply plan" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("button", { name: "Close" })).toHaveFocus();
    await user.tab({ shift: true });
    expect(screen.getByRole("button", { name: "Apply plan" })).toHaveFocus();
  });

  it("brings focus back when it leaves by other means", async () => {
    const user = userEvent.setup();
    render(<PlanSheet />);
    const opener = screen.getByRole("button", { name: "Review plan" });
    await user.click(opener);
    opener.focus();
    expect(screen.getByRole("button", { name: "Close" })).toHaveFocus();
  });

  it("closes on Escape, the close button and the scrim, returning focus to the opener", async () => {
    const user = userEvent.setup();
    render(<PlanSheet />);
    const opener = screen.getByRole("button", { name: "Review plan" });

    await user.click(opener);
    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(opener).toHaveFocus();
    expect(document.body.style.overflow).toBe("");

    await user.click(opener);
    await user.click(screen.getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(opener).toHaveFocus();

    await user.click(opener);
    const scrim = document.querySelector(".dw-sheet-layer .dw-scrim");
    if (scrim) await user.click(scrim);
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(opener).toHaveFocus();
  });

  it("lets a command bar opened inside close on its own first", async () => {
    const user = userEvent.setup();
    render(<PlanSheet withCommandBar />);
    await user.click(screen.getByRole("button", { name: "Review plan" }));
    const find = screen.getByRole("button", { name: "Find" });
    await user.click(find);
    expect(screen.getByRole("combobox")).toHaveFocus();
    expect(document.body.style.overflow).toBe("hidden");
    // Only the innermost layer traps Tab.
    await user.tab();
    expect(screen.getByRole("combobox")).toHaveFocus();

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("combobox")).toBeNull();
    expect(screen.getByRole("dialog", { name: "Review plan" })).toBeVisible();
    expect(find).toHaveFocus();
    expect(document.body.style.overflow).toBe("hidden");

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(document.body.style.overflow).toBe("");
  });

  it.each([
    ["", false],
    [" under StrictMode", true],
  ])("puts a command bar that opens with the sheet on top of it%s", async (_, strict) => {
    const user = userEvent.setup();
    render(<SheetWithCommandBar />, { reactStrictMode: strict });
    const opener = screen.getByRole("button", { name: "Find in plan" });
    await user.click(opener);
    expect(screen.getByRole("combobox")).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("combobox")).toHaveFocus();

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("combobox")).toBeNull();
    const sheet = screen.getByRole("dialog", { name: "Review plan" });
    expect(sheet).toHaveFocus();
    expect(document.body.style.overflow).toBe("hidden");

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(opener).toHaveFocus();
    expect(document.body.style.overflow).toBe("");
  });

  it("leaves Escape to an input method while it composes", async () => {
    const user = userEvent.setup();
    render(<PlanSheet />);
    await user.click(screen.getByRole("button", { name: "Review plan" }));
    const reason = screen.getByRole("textbox", { name: "Reason" });
    // Chrome and Firefox mark the event; Safari only sends keyCode 229.
    fireEvent.keyDown(reason, { key: "Escape", isComposing: true });
    fireEvent.keyDown(reason, { key: "Escape", keyCode: 229 });
    expect(screen.getByRole("dialog", { name: "Review plan" })).toBeVisible();
    fireEvent.keyDown(reason, { key: "Escape" });
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("renders in place when inline, without trapping focus", async () => {
    const { container } = render(
      <Sheet inline title="New app" subtitle="In storefront / production" width={480} footer={<Button>Create</Button>}>
        <TextField label="Name" />
      </Sheet>,
    );
    const dialog = screen.getByRole("dialog", { name: "New app" });
    expect(container.contains(dialog)).toBe(true);
    expect(dialog).not.toHaveAttribute("aria-modal");
    expect(dialog).toHaveClass("is-inline");
    expect(dialog).toHaveStyle({ width: "480px" });
    expect(dialog).not.toHaveFocus();
    expect(screen.queryByRole("button", { name: "Close" })).toBeNull();
    expect(document.body.style.overflow).toBe("");
    await expectAccessible(container);
  });

  it("names the close button in the app's language", () => {
    render(
      <UIStringsProvider strings={{ ...englishStrings, close: "关闭" }}>
        <Sheet inline title="审查计划" onClose={() => {}} />
      </UIStringsProvider>,
    );
    expect(screen.getByRole("button", { name: "关闭" })).toBeVisible();
  });

  it("gives focus to the field it is told to, and keeps its head out of the page's landmarks", async () => {
    function WithField() {
      const field = useRef<HTMLInputElement>(null);
      return (
        <Sheet open title="New app" initialFocusRef={field} footer={<button type="button">Go</button>}>
          <input ref={field} aria-label="Image" />
        </Sheet>
      );
    }
    render(<WithField />);
    await waitFor(() => expect(screen.getByLabelText("Image")).toHaveFocus());
    expect(screen.queryByRole("banner")).not.toBeInTheDocument();
    expect(screen.queryByRole("contentinfo")).not.toBeInTheDocument();
  });
});
