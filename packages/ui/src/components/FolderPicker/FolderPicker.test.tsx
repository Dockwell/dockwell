// SPDX-License-Identifier: AGPL-3.0-only

import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { type FolderListing, FolderPicker } from "./FolderPicker";

/** A server with /var/www holding blog and docs, and a folder Dockwell doesn't open. */
const tree: Record<string, string[]> = {
  "/": ["etc", "var"],
  "/var": ["lib", "www"],
  "/var/www": ["blog", "docs"],
  "/var/www/blog": [],
};
const list = vi.fn(async (path: string): Promise<FolderListing> => {
  const folders = tree[path];
  if (!folders) throw new Error("not there");
  return { path, folders: folders.map((name) => ({ name, locked: name === "lib" })) };
});

function Picker({ start = "/var/www/new.example.com", note }: { start?: string; note?: string }) {
  const [value, setValue] = useState(start);
  return (
    <>
      <FolderPicker label="Folder" value={value} onChange={setValue} list={list} note={note} />
      <output>{value}</output>
    </>
  );
}

describe("FolderPicker", () => {
  it("is one line while closed: the folder, what's known of it, and Change", async () => {
    list.mockClear();
    const { container } = render(<Picker note="New, made with the site" />);
    expect(container.querySelector(".dw-folder-summary")).toHaveTextContent(
      "/var/www/new.example.comNew, made with the siteChange",
    );
    // Nothing is asked of the server until someone opens it.
    expect(list).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Change the folder, /var/www/new.example.com" })).toHaveAttribute(
      "aria-expanded",
      "false",
    );
    await expectAccessible(container);
  });

  it("opens where the folder would be, looking above a folder that isn't there yet", async () => {
    const user = userEvent.setup();
    const { container } = render(<Picker />);
    await user.click(screen.getByRole("button", { name: /^Change the folder/ }));
    const folders = await screen.findByRole("list", { name: "Folders in /var/www" });
    expect(
      within(folders)
        .getAllByRole("button")
        .map((b) => b.textContent),
    ).toEqual(["blog", "docs"]);
    expect(list).toHaveBeenCalledWith("/var/www/new.example.com");
    expect(list).toHaveBeenCalledWith("/var/www");
    await expectAccessible(container);
  });

  it("picks a folder that's there, goes into it, and goes back up by the crumbs", async () => {
    const user = userEvent.setup();
    render(<Picker />);
    await user.click(screen.getByRole("button", { name: /^Change the folder/ }));
    await user.click(await screen.findByRole("button", { name: "blog" }));
    expect(screen.getByRole("status")).toHaveTextContent("/var/www/blog");
    expect(await screen.findByText("No folders here")).toBeVisible();
    await user.click(
      within(screen.getByRole("navigation", { name: "Looking in" })).getByRole("button", { name: "var" }),
    );
    expect(screen.getByRole("status")).toHaveTextContent("/var");
    const folders = await screen.findByRole("list", { name: "Folders in /var" });
    // What Dockwell doesn't open can't be picked.
    expect(within(folders).getByRole("button", { name: "lib" })).toBeDisabled();
    expect(within(folders).getByRole("button", { name: "www" })).toBeEnabled();
  });

  it("names a new folder inside the one it's looking in, and makes nothing itself", async () => {
    const user = userEvent.setup();
    render(<Picker />);
    await user.click(screen.getByRole("button", { name: /^Change the folder/ }));
    await screen.findByRole("list", { name: "Folders in /var/www" });
    const name = screen.getByRole("textbox", { name: "New folder in /var/www" });
    const use = screen.getByRole("button", { name: "Use" });
    expect(use).toBeDisabled();
    await user.type(name, "shop");
    await user.click(use);
    expect(screen.getByRole("status")).toHaveTextContent("/var/www/shop");
    // A name, not a path: a slash is for the field above.
    await user.type(name, "a/b");
    expect(use).toBeDisabled();
  });

  it("takes a typed path, for someone who knows it, and looks there once it's left", async () => {
    const user = userEvent.setup();
    render(<Picker />);
    await user.click(screen.getByRole("button", { name: /^Change the folder/ }));
    const path = screen.getByRole("textbox", { name: "Path" });
    await user.clear(path);
    await user.type(path, "/var");
    expect(screen.getByRole("status")).toHaveTextContent("/var");
    await user.tab();
    expect(await screen.findByRole("list", { name: "Folders in /var" })).toBeVisible();
  });

  it("says so when the server's folders can't be read, and still takes a typed path", async () => {
    const user = userEvent.setup();
    const refused = vi.fn(async (): Promise<FolderListing> => {
      throw new Error("the agent is away");
    });
    render(<FolderPicker label="Folder" value="/srv/site" onChange={() => {}} list={refused} />);
    await user.click(screen.getByRole("button", { name: /^Change the folder/ }));
    expect(await screen.findByText("Can't read the server's folders now")).toBeVisible();
    expect(screen.getByRole("textbox", { name: "Path" })).toHaveValue("/srv/site");
    await waitFor(() => expect(refused).toHaveBeenCalledWith("/"));
  });

  it("says what's wrong in place of its note", () => {
    render(
      <FolderPicker
        label="Folder"
        value="var/www"
        onChange={() => {}}
        list={list}
        note="New"
        error="Start it with /."
      />,
    );
    expect(screen.getByText("Start it with /.")).toBeVisible();
    expect(screen.queryByText("New")).toBeNull();
  });
});
