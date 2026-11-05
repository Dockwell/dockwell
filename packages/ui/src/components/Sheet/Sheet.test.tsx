// SPDX-License-Identifier: AGPL-3.0-only

import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { StrictMode, useRef, useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { useCoveredByLayer } from "../../lib/modal";
import { englishStrings, UIStringsProvider } from "../../lib/strings";
import { expectAccessible } from "../../test/a11y";
import { Button } from "../Button/Button";
import { CommandBar } from "../CommandBar/CommandBar";
import { TextField } from "../TextField/TextField";
import { Sheet, SheetSwap, SheetView, useSheetViewAway } from "./Sheet";

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

  describe("handing on to another view", () => {
    function Handover() {
      const [open, setOpen] = useState(true);
      const [over, setOver] = useState(false);
      const field = useRef<HTMLInputElement>(null);
      if (!open) return <p>Closed</p>;
      return (
        <Sheet
          open
          title="New app"
          initialFocusRef={field}
          onClose={() => setOpen(false)}
          footer={<Button onClick={() => setOver(true)}>Review plan</Button>}
          over={
            over ? (
              <SheetView
                title="Review plan"
                subtitle="Run nginx"
                onClose={() => setOver(false)}
                footer={<Button onClick={() => setOver(false)}>Cancel</Button>}
              >
                The plan
              </SheetView>
            ) : undefined
          }
        >
          <TextField ref={field} label="Image" />
        </Sheet>
      );
    }

    it("keeps one shell and one scrim, and the form as it was underneath", async () => {
      const user = userEvent.setup();
      render(<Handover />);
      const shell = screen.getByRole("dialog", { name: "New app" });
      const scrim = document.querySelector(".dw-scrim");
      const field = screen.getByLabelText("Image");
      await waitFor(() => expect(field).toHaveFocus());
      await user.type(field, "nginx:1.27");
      await user.click(screen.getByRole("button", { name: "Review plan" }));

      const plan = screen.getByRole("dialog", { name: "Review plan" });
      expect(plan).toBe(shell);
      expect(plan).toHaveAccessibleDescription("Run nginx");
      expect(document.querySelector(".dw-scrim")).toBe(scrim);
      expect(document.querySelectorAll(".dw-sheet")).toHaveLength(1);
      // What is underneath is out of reach, and out of the accessibility tree.
      expect(field.closest(".dw-sheet-view")).toHaveClass("is-away");
      expect(field.closest(".dw-sheet-view")).toHaveAttribute("inert");
      expect(screen.queryByRole("textbox", { name: "Image" })).toBeNull();
      // Focus goes with it, to what a screen reader then reads.
      expect(screen.getByRole("heading", { name: "Review plan" })).toHaveFocus();
      await expectAccessible(document.body);

      await user.click(screen.getByRole("button", { name: "Cancel" }));
      const back = screen.getByRole("dialog", { name: "New app" });
      expect(back).toBe(shell);
      expect(back).not.toHaveAccessibleDescription();
      expect(screen.getByRole("textbox", { name: "Image" })).toHaveValue("nginx:1.27");
      expect(document.querySelector(".dw-scrim")).toBe(scrim);
      expect(field.closest(".dw-sheet-view")).not.toHaveAttribute("inert");
    });

    it("gives focus back to where it was, and Escape closes what shows", async () => {
      const user = userEvent.setup();
      render(<Handover />);
      const review = screen.getByRole("button", { name: "Review plan" });
      review.focus();
      await user.keyboard("{Enter}");
      screen.getByRole("dialog", { name: "Review plan" });
      await user.keyboard("{Escape}");
      expect(screen.getByRole("dialog", { name: "New app" })).toBeVisible();
      expect(review).toHaveFocus();
      await user.keyboard("{Escape}");
      expect(screen.getByText("Closed")).toBeVisible();
    });

    it("fades the old view out over the new one, then lets it go", async () => {
      const user = userEvent.setup();
      render(<Handover />);
      await user.click(screen.getByRole("button", { name: "Review plan" }));
      await user.click(screen.getByRole("button", { name: "Cancel" }));
      const leaving = screen.getByText("The plan").closest(".dw-sheet-view");
      expect(leaving).toHaveClass("is-away");
      expect(leaving).toHaveAttribute("inert");
      await waitFor(() => expect(screen.queryByText("The plan")).toBeNull());
    });
  });

  it("swaps views in the sheet they share, with focus on the new one", async () => {
    function Swap() {
      const [step, setStep] = useState(0);
      return (
        <Sheet
          open
          over={
            <SheetSwap viewKey={step}>
              <SheetView title={`Step ${step}`} footer={<Button onClick={() => setStep(step + 1)}>Next</Button>} />
            </SheetSwap>
          }
        />
      );
    }
    const user = userEvent.setup();
    render(<Swap />);
    const shell = screen.getByRole("dialog", { name: "Step 0" });
    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByRole("dialog", { name: "Step 1" })).toBe(shell);
    expect(screen.getByRole("heading", { name: "Step 1" })).toHaveFocus();
  });

  it("goes round from a heading that holds focus with Shift+Tab, rather than leaving the sheet", async () => {
    const user = userEvent.setup();
    render(
      <Sheet open title="Plan" onClose={() => {}} footer={<Button>Apply plan</Button>}>
        Body
      </Sheet>,
    );
    screen.getByRole("heading", { name: "Plan" }).focus();
    await user.tab({ shift: true });
    expect(screen.getByRole("button", { name: "Apply plan" })).toHaveFocus();
  });
});

describe("how a sheet leaves", () => {
  /** A browser's animations, which jsdom hasn't: each gives back what ends it. */
  function withAnimations() {
    const ends: (() => void)[] = [];
    const animations = () => {
      let end = () => {};
      const finished = new Promise<void>((resolve) => {
        end = resolve;
      });
      ends.push(end);
      return [{ finished }];
    };
    Object.defineProperty(Element.prototype, "getAnimations", { configurable: true, value: animations });
    return ends;
  }
  const leaving = () => document.querySelector(".dw-sheet-layer.is-leaving");

  afterEach(() => {
    Reflect.deleteProperty(Element.prototype, "getAnimations");
    vi.unstubAllGlobals();
    document.querySelector(".dw-sheet-layer.is-leaving")?.remove();
  });

  function Form({ onShow = true }: { onShow?: boolean }) {
    const [open, setOpen] = useState(onShow);
    return (
      <>
        <Button onClick={() => setOpen(true)}>Open</Button>
        {open ? (
          <Sheet
            open
            onClose={() => setOpen(false)}
            title="Deploy web"
            footer={<Button onClick={() => setOpen(false)}>Done</Button>}
          >
            <label>
              Image
              <input id="image" defaultValue="nginx:1.27" />
            </label>
          </Sheet>
        ) : null}
      </>
    );
  }

  it("leaves as it came, as a copy nothing can reach, however it was unmounted", async () => {
    const user = userEvent.setup();
    const ends = withAnimations();
    render(<Form />);
    await user.clear(screen.getByLabelText("Image"));
    await user.type(screen.getByLabelText("Image"), "nginx:1.28");
    await user.click(screen.getByRole("button", { name: "Done" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    await waitFor(() => expect(leaving()).not.toBeNull());
    const ghost = leaving() as HTMLElement;
    expect(ghost).toHaveAttribute("aria-hidden", "true");
    expect(ghost.inert).toBe(true);
    // No ID twice in the page, and what you typed stays until it has gone.
    expect(ghost.querySelector("[id]")).toBeNull();
    expect(ghost.querySelector("input")).toHaveValue("nginx:1.28");
    // The page is yours at once.
    expect(screen.getByRole("button", { name: "Open" })).toBeVisible();
    for (const end of ends) end();
    await waitFor(() => expect(leaving()).toBeNull());
  });

  it("goes anyway if its animations never end", async () => {
    const user = userEvent.setup();
    withAnimations();
    render(<Form />);
    await user.click(screen.getByRole("button", { name: "Done" }));
    await waitFor(() => expect(leaving()).not.toBeNull());
    await waitFor(() => expect(leaving()).toBeNull(), { timeout: 1_000 });
  });

  it("has nothing to slide away under reduced motion", async () => {
    const user = userEvent.setup();
    withAnimations();
    vi.stubGlobal(
      "matchMedia",
      vi.fn((query: string) => ({
        matches: query.includes("reduced-motion"),
        addEventListener() {},
        removeEventListener() {},
      })),
    );
    render(<Form />);
    await user.click(screen.getByRole("button", { name: "Done" }));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(leaving()).toBeNull();
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("leaves no copy of a sheet that stays, as StrictMode unmounts one to try it", async () => {
    withAnimations();
    render(
      <StrictMode>
        <Form />
      </StrictMode>,
    );
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(screen.getByRole("dialog")).toBeVisible();
    expect(leaving()).toBeNull();
  });

  it("doesn't leave as a form hands on to what takes it over: the shell stays", async () => {
    const ends = withAnimations();
    const { rerender } = render(<Sheet open title="Form" />);
    rerender(<Sheet open title="Form" over={<SheetView title="Plan" />} />);
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(leaving()).toBeNull();
    expect(ends).toEqual([]);
  });
});

describe("where focus goes when a sheet closes", () => {
  function Page({ keepOpener = true }: { keepOpener?: boolean }) {
    const [open, setOpen] = useState(false);
    const [opened, setOpened] = useState(false);
    return (
      <main>
        <h1>web</h1>
        {keepOpener || !opened ? (
          <Button
            onClick={() => {
              setOpen(true);
              setOpened(true);
            }}
          >
            Add a port
          </Button>
        ) : null}
        <Sheet open={open} onClose={() => setOpen(false)} title="Add a port" />
      </main>
    );
  }

  it("goes back to what opened it", async () => {
    const user = userEvent.setup();
    render(<Page />);
    await user.click(screen.getByRole("button", { name: "Add a port" }));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.getByRole("button", { name: "Add a port" })).toHaveFocus());
  });

  it("goes to the page's heading when what opened it has gone, and never falls to the body", async () => {
    const user = userEvent.setup();
    render(<Page keepOpener={false} />);
    await user.click(screen.getByRole("button", { name: "Add a port" }));
    await user.keyboard("{Escape}");
    const heading = screen.getByRole("heading", { level: 1, name: "web" });
    await waitFor(() => expect(heading).toHaveFocus());
    expect(heading).toHaveAttribute("tabindex", "-1");
  });

  it("finds the heading of a page still on its way", async () => {
    const user = userEvent.setup();
    function Moving() {
      const [open, setOpen] = useState(false);
      const [arrived, setArrived] = useState(false);
      return (
        <main>
          {arrived ? <h1>Apps</h1> : open ? null : <Button onClick={() => setOpen(true)}>New app</Button>}
          <Sheet
            open={open}
            title="New app"
            onClose={() => {
              setOpen(false);
              setTimeout(() => setArrived(true), 30);
            }}
          />
        </main>
      );
    }
    render(<Moving />);
    await user.click(screen.getByRole("button", { name: "New app" }));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.getByRole("heading", { level: 1, name: "Apps" })).toHaveFocus());
  });

  it("leaves focus alone once someone has moved on", async () => {
    const user = userEvent.setup();
    render(
      <main>
        <h1>web</h1>
        <PageWithAnother />
      </main>,
    );
    await user.click(screen.getByRole("button", { name: "Open" }));
    await user.keyboard("{Escape}");
    await user.click(screen.getByRole("button", { name: "Elsewhere" }));
    await new Promise((resolve) => setTimeout(resolve, 60));
    expect(screen.getByRole("button", { name: "Elsewhere" })).toHaveFocus();
  });

  function PageWithAnother() {
    const [open, setOpen] = useState(false);
    return (
      <>
        <Button onClick={() => setOpen(true)}>Open</Button>
        <Button>Elsewhere</Button>
        <Sheet open={open} onClose={() => setOpen(false)} title="A sheet" />
      </>
    );
  }
});

describe("a view on its way out", () => {
  it("can't be reached, even inside a swap of its own, as a plan with its undo is", () => {
    const plan = (
      <SheetSwap viewKey={1}>
        <SheetView title="Plan" footer={<Button>Cancel the plan</Button>} />
      </SheetSwap>
    );
    const { rerender } = render(<Sheet open title="Form" footer={<Button>Cancel the form</Button>} over={plan} />);
    expect(screen.getByRole("button", { name: "Cancel the plan" })).toBeVisible();
    rerender(<Sheet open title="Form" footer={<Button>Cancel the form</Button>} />);
    // For the moment it fades, the plan is still there, and nothing but the form answers.
    expect(screen.queryByRole("button", { name: "Cancel the plan" })).toBeNull();
    expect(screen.getByRole("button", { name: "Cancel the form" })).toBeVisible();
  });
});

describe("whether a view's keys are its own", () => {
  function Reads({ name }: { name: string }) {
    return <output aria-label={name}>{useSheetViewAway() ? "away" : "here"}</output>;
  }

  it("says no while any modal is open above its sheet, and yes once that has gone", async () => {
    const user = userEvent.setup();
    // Each holds its own state, as the shell's command bar does, so nothing re-renders what is under it.
    function Bar() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>
            Open the bar
          </button>
          <CommandBar open={open} items={[]} onClose={() => setOpen(false)} />
        </>
      );
    }
    function Another() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button type="button" onClick={() => setOpen(true)}>
            Open a sheet
          </button>
          <Sheet open={open} title="Another" onClose={() => setOpen(false)}>
            <Reads name="another" />
          </Sheet>
        </>
      );
    }
    render(
      <>
        <Sheet open title="Plan" onClose={() => {}}>
          <Reads name="plan" />
        </Sheet>
        <Bar />
        <Another />
      </>,
    );
    const plan = screen.getByRole("status", { name: "plan" });
    expect(plan).toHaveTextContent("here");

    await user.click(screen.getByRole("button", { name: "Open the bar" }));
    await waitFor(() => expect(plan).toHaveTextContent("away"));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(plan).toHaveTextContent("here"));

    await user.click(screen.getByRole("button", { name: "Open a sheet" }));
    await waitFor(() => expect(plan).toHaveTextContent("away"));
    // What is on top is on top: its own keys are its own.
    expect(screen.getByRole("status", { name: "another" })).toHaveTextContent("here");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(plan).toHaveTextContent("here"));
  });

  it("says no under a modal that opened in the same render as its sheet", () => {
    render(
      <Sheet open title="Plan" onClose={() => {}}>
        <Reads name="plan" />
        <Sheet open title="Inside" onClose={() => {}}>
          <Reads name="inside" />
        </Sheet>
      </Sheet>,
    );
    expect(screen.getByRole("status", { name: "plan", hidden: true })).toHaveTextContent("away");
    expect(screen.getByRole("status", { name: "inside", hidden: true })).toHaveTextContent("here");
  });

  it("says no under a modal opened from inside it, as well as beside it", async () => {
    const user = userEvent.setup();
    function Nested() {
      const [inner, setInner] = useState(false);
      return (
        <Sheet open title="Plan" onClose={() => {}}>
          <Reads name="plan" />
          <button type="button" onClick={() => setInner(true)}>
            Open inside
          </button>
          <Sheet open={inner} title="Inside" onClose={() => setInner(false)}>
            <Reads name="inside" />
          </Sheet>
        </Sheet>
      );
    }
    render(<Nested />);
    const plan = screen.getByRole("status", { name: "plan" });
    await user.click(screen.getByRole("button", { name: "Open inside" }));
    await waitFor(() => expect(plan).toHaveTextContent("away"));
    expect(screen.getByRole("status", { name: "inside" })).toHaveTextContent("here");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(plan).toHaveTextContent("here"));
  });

  it("says no for a view something covers, or that is on its way out, as it did", () => {
    render(
      <Sheet open title="Plan" onClose={() => {}} over={<Reads name="over" />}>
        <Reads name="under" />
      </Sheet>,
    );
    expect(screen.getByRole("status", { name: "over", hidden: true })).toHaveTextContent("here");
    expect(screen.getByRole("status", { name: "under", hidden: true })).toHaveTextContent("away");
  });

  it("reads as covered from the page, under any layer, and not with none open", async () => {
    const user = userEvent.setup();
    function OnThePage() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <output aria-label="page">{useCoveredByLayer() ? "covered" : "clear"}</output>
          <button type="button" onClick={() => setOpen(true)}>
            Open
          </button>
          <Sheet open={open} title="Above" onClose={() => setOpen(false)}>
            nothing
          </Sheet>
        </>
      );
    }
    render(<OnThePage />);
    const page = screen.getByRole("status", { name: "page" });
    expect(page).toHaveTextContent("clear");
    await user.click(screen.getByRole("button", { name: "Open" }));
    await waitFor(() => expect(page).toHaveTextContent("covered"));
    await user.keyboard("{Escape}");
    await waitFor(() => expect(page).toHaveTextContent("clear"));
  });
});
