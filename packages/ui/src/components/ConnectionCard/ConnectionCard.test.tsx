// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { Button } from "../Button/Button";
import { ConnectionCard } from "./ConnectionCard";

const items = [
  { label: "Host", value: "db" },
  { label: "Port", value: "3306" },
  { label: "User", value: "root" },
];
const url = (password: string | undefined) =>
  password === undefined ? "mysql://root@db:3306/" : `mysql://root:${password}@db:3306/`;

/** A page that reveals as the panel does: asked, then given the value. */
function Revealing({ onReveal = () => {} }: { onReveal?: () => void }) {
  const [value, setValue] = useState<string>();
  return (
    <ConnectionCard
      items={items}
      secret={{
        label: "Password",
        value,
        onReveal: () => {
          onReveal();
          setValue("s3cret-example");
        },
        onHide: () => setValue(undefined),
        action: <Button size="sm">Change password</Button>,
      }}
      connectionString={{ label: "Connection string", build: url }}
    />
  );
}

describe("ConnectionCard", () => {
  // user-event puts a clipboard of its own in place as it's set up, so each test reads that one.
  let written: string[];
  const watch = () => {
    written = [];
    const write = navigator.clipboard.writeText.bind(navigator.clipboard);
    vi.spyOn(navigator.clipboard, "writeText").mockImplementation((text: string) => {
      written.push(text);
      return write(text);
    });
  };
  afterEach(() => vi.restoreAllMocks());

  it("lists each detail with a copy button of its own, under a default title", async () => {
    const user = userEvent.setup();
    watch();
    const { container } = render(<Revealing />);
    expect(screen.getByRole("heading", { name: "Connect" })).toBeInTheDocument();
    expect(screen.getByText("db")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Copy port" }));
    expect(written).toEqual(["3306"]);
    expect(await screen.findByRole("button", { name: "Port copied" })).toBeInTheDocument();
    await expectAccessible(container);
  });

  it("masks the secret, and the string, until it's revealed, and copies the string without it meanwhile", async () => {
    const user = userEvent.setup();
    watch();
    render(<Revealing />);
    expect(screen.queryByText(/s3cret/)).toBeNull();
    expect(screen.getByText("Hidden")).toBeInTheDocument();
    expect(screen.getByText("mysql://root:••••••••••••@db:3306/")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Copy password" })).toBeNull();
    await user.click(screen.getByRole("button", { name: "Copy connection string, without the password" }));
    expect(written).toEqual(["mysql://root@db:3306/"]);
  });

  it("reveals through the page, then copies the secret and the whole string, and hides again", async () => {
    const user = userEvent.setup();
    watch();
    const asked = vi.fn();
    render(<Revealing onReveal={asked} />);
    await user.click(screen.getByRole("button", { name: "Reveal password" }));
    expect(asked).toHaveBeenCalledOnce();
    expect(screen.getByText("s3cret-example")).toBeInTheDocument();
    expect(screen.getByText("mysql://root:s3cret-example@db:3306/")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Copy password" }));
    await user.click(screen.getByRole("button", { name: "Copy connection string" }));
    expect(written).toEqual(["s3cret-example", "mysql://root:s3cret-example@db:3306/"]);
    await user.click(screen.getByRole("button", { name: "Hide" }));
    expect(screen.queryByText(/s3cret/)).toBeNull();
  });

  it("keeps the action under the secret, and says why a reveal failed in place of the hint", () => {
    render(
      <ConnectionCard
        title="Root"
        items={items}
        secret={{
          label: "Password",
          hint: "In the secret MARIADB_ROOT_PASSWORD",
          problem: "Your passkey wasn't confirmed.",
          onReveal: () => {},
          onHide: () => {},
          action: <Button size="sm">Change password</Button>,
        }}
      />,
    );
    expect(screen.getByRole("heading", { name: "Root" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Change password" })).toBeInTheDocument();
    expect(screen.getByRole("alert")).toHaveTextContent("Your passkey wasn't confirmed.");
    expect(screen.queryByText(/MARIADB_ROOT_PASSWORD/)).toBeNull();
  });

  it("shows a busy reveal while the page confirms it", () => {
    render(
      <ConnectionCard items={items} secret={{ label: "Password", busy: true, onReveal: () => {}, onHide: () => {} }} />,
    );
    expect(screen.getByRole("button", { name: "Reveal password" })).toHaveAttribute("aria-busy", "true");
  });

  it("copies the string whole when there's no secret", async () => {
    const user = userEvent.setup();
    watch();
    render(
      <ConnectionCard
        items={[{ label: "Host", value: "redis" }]}
        connectionString={{ label: "Connection string", build: () => "redis://redis:6379" }}
      />,
    );
    await user.click(screen.getByRole("button", { name: "Copy connection string" }));
    expect(written).toEqual(["redis://redis:6379"]);
  });

  it("says in one line what the details don't show", () => {
    render(<ConnectionCard items={items} note="Its password is in its own environment" />);
    expect(screen.getByText("Its password is in its own environment")).toBeInTheDocument();
  });

  it("keeps an acronym's capitals in a button's name", () => {
    render(<ConnectionCard items={[{ label: "API key", value: "k" }]} />);
    expect(screen.getByRole("button", { name: "Copy API key" })).toBeInTheDocument();
  });
});
