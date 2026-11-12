// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { createRef } from "react";
import { describe, expect, it, vi } from "vitest";

import { STATUS } from "../lib/status";
import { englishStrings, UIStringsProvider } from "../lib/strings";
import { expectAccessible } from "../test/a11y";
import { Button, ButtonLink } from "./Button/Button";
import { Glyph } from "./Glyph/Glyph";
import { Icon } from "./Icon/Icon";
import { iconNames, icons } from "./Icon/icons";
import { Kbd } from "./Kbd/Kbd";
import { Logo } from "./Logo/Logo";
import { Panel } from "./Panel/Panel";
import { StatusBadge } from "./StatusBadge/StatusBadge";
import { Switch } from "./Switch/Switch";
import { Tag } from "./Tag/Tag";

describe("Icon", () => {
  it("draws every icon on the 24-unit grid", () => {
    expect(iconNames).toHaveLength(67);
    for (const name of iconNames) {
      const { container, unmount } = render(<Icon name={name} />);
      const svg = container.querySelector("svg");
      expect(svg?.getAttribute("viewBox"), name).toBe("0 0 24 24");
      expect(svg?.childElementCount, name).toBeGreaterThan(0);
      unmount();
    }
  });

  // An arrow is one drawing, turned: the same shaft and head, inside the grid, so the
  // arrows read as one set wherever they stand.
  it("draws its arrows as one drawing turned", () => {
    const points = (name: "arrow-right" | "arrow-left" | "arrow-up") => {
      const [part] = icons[name];
      const d = (part?.[1] as { d: string }).d;
      const out: string[] = [];
      let x = 0;
      let y = 0;
      for (const [, cmd, args] of d.matchAll(/([MmLlHhVv])([^MmLlHhVv]*)/g)) {
        const nums = (args?.match(/-?\d+(?:\.\d+)?/g) ?? []).map(Number);
        const step = cmd === "H" || cmd === "h" || cmd === "V" || cmd === "v" ? 1 : 2;
        for (let i = 0; i < nums.length; i += step) {
          const a = nums[i] ?? 0;
          const b = nums[i + 1] ?? 0;
          if (cmd === "M" || cmd === "L") [x, y] = [a, b];
          else if (cmd === "m" || cmd === "l") [x, y] = [x + a, y + b];
          else if (cmd === "H") x = a;
          else if (cmd === "h") x += a;
          else if (cmd === "V") y = a;
          else y += a;
          expect(x >= 0 && x <= 24 && y >= 0 && y <= 24, `${name} stays on the grid`).toBe(true);
          out.push(`${x},${y}`);
        }
      }
      return out.sort();
    };
    const right = points("arrow-right").map((p) => p.split(",").map(Number) as [number, number]);
    // Up is right turned a quarter to the left; left is right mirrored.
    expect(points("arrow-up")).toEqual(right.map(([x, y]) => `${y},${24 - x}`).sort());
    expect(points("arrow-left")).toEqual(right.map(([x, y]) => `${24 - x},${y}`).sort());
  });

  it("is decorative unless labelled", () => {
    const { container } = render(<Icon name="server" />);
    expect(container.querySelector("svg")).toHaveAttribute("aria-hidden", "true");
    render(<Icon name="globe" label="Public" size={20} />);
    const img = screen.getByRole("img", { name: "Public" });
    expect(img).toHaveAttribute("width", "20");
  });
});

describe("Glyph", () => {
  it("draws each status shape, hidden from assistive technology", () => {
    for (const shape of [
      "circle",
      "triangle",
      "square",
      "spinner",
      "ring",
      "dotted-ring",
      "dashed-ring",
      "info",
    ] as const) {
      const { container, unmount } = render(<Glyph shape={shape} />);
      const svg = container.querySelector("svg");
      expect(svg).toHaveAttribute("aria-hidden", "true");
      expect(svg?.childElementCount, shape).toBeGreaterThan(0);
      unmount();
    }
  });

  it("tells the stopped ring from the unknown one by more than its dash", () => {
    const parts = (shape: "ring" | "dashed-ring" | "dotted-ring") => {
      const { container, unmount } = render(<Glyph shape={shape} />);
      const svg = container.querySelector("svg") as SVGElement;
      const out = {
        filled: svg.querySelectorAll("[fill='currentColor']").length,
        dashed: svg.querySelector("[stroke-dasharray]") !== null,
      };
      unmount();
      return out;
    };
    expect(parts("ring")).toEqual({ filled: 0, dashed: false });
    // A dash across it: something filled where the stopped ring has nothing.
    expect(parts("dashed-ring")).toEqual({ filled: 1, dashed: true });
    expect(parts("dotted-ring")).toEqual({ filled: 1, dashed: false });
  });

  it("animates only the deploying arc", () => {
    const { container } = render(<Glyph shape="spinner" />);
    expect(container.querySelector(".dw-spin")).not.toBeNull();
    const { container: still } = render(<Glyph shape="circle" />);
    expect(still.querySelector(".dw-spin")).toBeNull();
  });
});

describe("ButtonLink", () => {
  it("is a link that looks like a button, and passes the ref", async () => {
    const ref = createRef<HTMLAnchorElement>();
    const { container } = render(
      <ButtonLink ref={ref} href="/profile" size="sm" icon="shield">
        Profile and security
      </ButtonLink>,
    );
    const link = screen.getByRole("link", { name: "Profile and security" });
    expect(link).toHaveAttribute("href", "/profile");
    expect(link).toHaveClass("dw-btn", "dw-btn-secondary", "dw-btn-sm");
    expect(link).not.toHaveAttribute("type");
    expect(ref.current).toBe(link);
    expect(container.querySelector(".dw-icon")).not.toBeNull();
    await expectAccessible(container);
  });

  it("downloads as any link does, and goes square without a label", () => {
    render(
      <>
        <ButtonLink href="data:text/plain,codes" download="codes.txt" variant="primary">
          Download
        </ButtonLink>
        <ButtonLink href="/help" icon="question" aria-label="Help" variant="ghost" />
      </>,
    );
    const download = screen.getByRole("link", { name: "Download" });
    expect(download).toHaveAttribute("download", "codes.txt");
    expect(download).toHaveClass("dw-btn-primary");
    expect(screen.getByRole("link", { name: "Help" })).toHaveClass("dw-btn-icon", "dw-btn-ghost");
  });
});

describe("Button", () => {
  it("defaults to a secondary, medium, non-submitting button", async () => {
    const onClick = vi.fn();
    render(<Button onClick={onClick}>Restart</Button>);
    const button = screen.getByRole("button", { name: "Restart" });
    expect(button).toHaveAttribute("type", "button");
    expect(button).toHaveClass("dw-btn", "dw-btn-secondary", "dw-btn-md");
    await userEvent.click(button);
    expect(onClick).toHaveBeenCalledOnce();
  });

  it("shows a spinner, stops working, keeps its focus and reports busy while loading", async () => {
    const onClick = vi.fn();
    const { container } = render(
      <Button variant="primary" icon="deploy" loading onClick={onClick}>
        Deploying
      </Button>,
    );
    const button = screen.getByRole("button", { name: "Deploying" });
    expect(button).toHaveAttribute("aria-disabled", "true");
    expect(button).toHaveAttribute("aria-busy", "true");
    button.focus();
    expect(button).toHaveFocus();
    expect(container.querySelector(".dw-btn-spinner")).not.toBeNull();
    expect(container.querySelector(".dw-icon")).toBeNull();
    await userEvent.click(button);
    expect(onClick).not.toHaveBeenCalled();
  });

  it("becomes square without a label, named by aria-label", async () => {
    const { container } = render(<Button icon="more" aria-label="More actions" variant="ghost" />);
    expect(screen.getByRole("button", { name: "More actions" })).toHaveClass("dw-btn-icon");
    await expectAccessible(container);
  });

  it("shows its shortcut, keeping it out of the name, and passes the ref", async () => {
    const ref = createRef<HTMLButtonElement>();
    const { container } = render(
      <Button ref={ref} kbd="⌘↵" type="submit">
        Apply plan
      </Button>,
    );
    expect(ref.current).toBe(screen.getByRole("button", { name: "Apply plan" }));
    expect(ref.current).toHaveAttribute("type", "submit");
    expect(ref.current).toHaveAttribute("aria-keyshortcuts", "Meta+Enter");
    expect(screen.getByText("⌘↵").tagName).toBe("KBD");
    expect(screen.getByText("⌘↵")).toHaveAttribute("aria-hidden", "true");
    await expectAccessible(container);
  });

  it("leaves out a shortcut aria-keyshortcuts can't say, and takes the caller's", () => {
    render(
      <>
        <Button kbd="G F">Fleet</Button>
        <Button kbd="⌘↵" aria-keyshortcuts="Control+Enter">
          Apply plan
        </Button>
      </>,
    );
    expect(screen.getByRole("button", { name: "Fleet" })).not.toHaveAttribute("aria-keyshortcuts");
    expect(screen.getByRole("button", { name: "Apply plan" })).toHaveAttribute("aria-keyshortcuts", "Control+Enter");
  });
});

describe("StatusBadge", () => {
  it("pairs the shape with the status word", () => {
    const { container } = render(<StatusBadge status="degraded" />);
    expect(screen.getByText("Degraded")).toBeVisible();
    expect(container.firstChild).toHaveClass("dw-status-dot", "dw-tone-warning");
  });

  it("gives what runs unchecked and what nobody can say shapes of their own, never green", () => {
    const shapes = new Set<string>();
    for (const status of ["healthy", "running", "degraded", "down", "deploying", "stopped", "unknown"] as const) {
      const { container, unmount } = render(<StatusBadge status={status} />);
      shapes.add(container.querySelector("svg")?.innerHTML ?? "");
      if (status === "running" || status === "unknown") {
        expect(container.firstChild).not.toHaveClass("dw-tone-healthy");
      }
      unmount();
    }
    expect(shapes.size).toBe(7);
    // Ink is for action, so no state spends it, and each has a token of its own.
    for (const [status, spec] of Object.entries(STATUS)) expect(spec.tone, status).not.toBe("ink");
    expect(STATUS.running.tone).toBe("running");
    expect(STATUS.unknown.tone).toBe("unknown");
    render(<StatusBadge status="running" />);
    expect(screen.getByText("Running")).toBeVisible();
    render(<StatusBadge status="unknown" />);
    expect(screen.getByText("Unknown")).toBeVisible();
  });

  it("still announces the state when the label names the subject", async () => {
    const { container } = render(<StatusBadge status="down" label="ams-1-03" meta="for 6 min" variant="pill" />);
    expect(container.textContent).toBe("Down: ams-1-03for 6 min");
    expect(container.querySelector(".dw-sr")).toHaveTextContent("Down:");
    expect(container.querySelector("svg")).toHaveAttribute("width", "8");
    await expectAccessible(container);
  });

  it("uses translated status words", () => {
    render(
      <UIStringsProvider strings={{ ...englishStrings, status: { ...englishStrings.status, healthy: "健康" } }}>
        <StatusBadge status="healthy" />
      </UIStringsProvider>,
    );
    expect(screen.getByText("健康")).toBeVisible();
  });
});

describe("Tag", () => {
  it("renders key:value in mono with a named remove button", async () => {
    const onRemove = vi.fn();
    const { container } = render(<Tag label="region" value="eu" onRemove={onRemove} />);
    expect(container.textContent).toBe("region:eu");
    await userEvent.click(screen.getByRole("button", { name: "Remove region:eu" }));
    expect(onRemove).toHaveBeenCalledOnce();
    await expectAccessible(container);
  });

  it("names the remove button after plain children, or an explicit label", () => {
    render(<Tag onRemove={() => {}}>prod</Tag>);
    expect(screen.getByRole("button", { name: "Remove prod" })).toBeVisible();
    render(
      <Tag onRemove={() => {}} removeLabel="pool web">
        <b>web</b>
      </Tag>,
    );
    expect(screen.getByRole("button", { name: "Remove pool web" })).toBeVisible();
  });

  it("renders plain children and the managed tone", () => {
    const { container } = render(
      <Tag tone="cobalt" icon="branch">
        main
      </Tag>,
    );
    expect(container.firstChild).toHaveClass("dw-tag-cobalt");
    expect(container.textContent).toBe("main");
  });
});

describe("Kbd", () => {
  it("is a key cap", () => {
    render(<Kbd>⌘</Kbd>);
    expect(screen.getByText("⌘").tagName).toBe("KBD");
  });
});

describe("Panel", () => {
  it("is a region named by its title, with actions in the heading row", async () => {
    const { container } = render(
      <Panel title="Recent changes" actions={<StatusBadge status="healthy" />}>
        <p>Revision 128 replaced 127.</p>
      </Panel>,
    );
    const region = screen.getByRole("region", { name: "Recent changes" });
    expect(region).toHaveTextContent("Revision 128 replaced 127.");
    expect(container.querySelector(".dw-panel-actions")).toHaveTextContent("Healthy");
    await expectAccessible(container);
  });

  it("renders without a heading row, and edge to edge when flush", () => {
    const { container } = render(<Panel flush>rows</Panel>);
    expect(container.querySelector(".dw-panel-head")).toBeNull();
    expect(container.querySelector(".dw-panel-body")).toHaveClass("is-flush");
  });
});

describe("Logo", () => {
  it("is an image named Dockwell, sized by cap height", () => {
    render(<Logo height={14} />);
    const logo = screen.getByRole("img", { name: "Dockwell" });
    expect(logo).toHaveAttribute("height", "14");
    expect(Number(logo.getAttribute("width"))).toBeCloseTo((14 * 3850.4) / 700, 3);
  });

  it("keeps the water cobalt unless one colour is asked for", () => {
    const { container } = render(<Logo variant="mark" />);
    expect(container.querySelectorAll("path")[1]).toHaveAttribute("fill", "var(--cobalt)");
    const { container: mono } = render(<Logo variant="mark" mono />);
    expect(mono.querySelectorAll("path")[1]).toHaveAttribute("fill", "currentColor");
  });

  it("draws the app icon in fixed colours", () => {
    const { container } = render(<Logo variant="icon" height={32} title="Dockwell app" />);
    expect(screen.getByRole("img", { name: "Dockwell app" })).toHaveAttribute("width", "32");
    expect(container.querySelector("rect")).toHaveAttribute("fill", "#191c1a");
  });
});

describe("Switch", () => {
  it("turns on and off by its words as well as its track, and has its label for a name", async () => {
    const user = userEvent.setup();
    const changes: boolean[] = [];
    const { container } = render(
      <Switch label="A tunnel in front" description="Says what it does" onChange={(on) => changes.push(on)} />,
    );
    const toggle = screen.getByRole("switch", { name: "A tunnel in front" });
    await user.click(screen.getByText("Says what it does"));
    expect(toggle).toHaveAttribute("aria-checked", "true");
    await user.click(screen.getByText("A tunnel in front"));
    expect(toggle).toHaveAttribute("aria-checked", "false");
    await user.click(toggle);
    expect(changes).toEqual([true, false, true]);
    await expectAccessible(container);
  });

  it("does nothing, by its words either, while it's disabled", async () => {
    const user = userEvent.setup();
    const changes: boolean[] = [];
    render(<Switch label="Off for now" description="Because of this" disabled onChange={(on) => changes.push(on)} />);
    await user.click(screen.getByText("Because of this"));
    expect(changes).toEqual([]);
  });
});

describe("a Button that's off with a reason", () => {
  it("keeps its place in the tab order, says why as its description, and does nothing when pressed", async () => {
    const onClick = vi.fn();
    const submit = vi.fn((event: { preventDefault: () => void }) => event.preventDefault());
    render(
      <form onSubmit={submit}>
        <Button type="submit" off="Only an admin or the owner can do this." disabled onClick={onClick}>
          Deploy
        </Button>
      </form>,
    );
    const button = screen.getByRole("button", { name: "Deploy" });
    expect(button).not.toBeDisabled();
    expect(button).toHaveAttribute("aria-disabled", "true");
    const why = document.getElementById(button.getAttribute("aria-describedby") ?? "");
    expect(why).toHaveTextContent("Only an admin or the owner can do this.");
    expect(button).toHaveAccessibleDescription("Only an admin or the owner can do this.");
    await userEvent.tab();
    expect(button).toHaveFocus();
    await userEvent.click(button);
    await userEvent.keyboard("{Enter}");
    expect(onClick).not.toHaveBeenCalled();
    expect(submit).not.toHaveBeenCalled();
  });

  it("is an ordinary button with no reason", async () => {
    const onClick = vi.fn();
    render(<Button onClick={onClick}>Deploy</Button>);
    const button = screen.getByRole("button", { name: "Deploy" });
    expect(button).not.toHaveAttribute("aria-disabled");
    expect(button).not.toHaveAttribute("aria-describedby");
    await userEvent.click(button);
    expect(onClick).toHaveBeenCalledTimes(1);
  });
});
