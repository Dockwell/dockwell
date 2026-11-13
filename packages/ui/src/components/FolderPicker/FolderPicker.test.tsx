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
  it("says where a folder holds more than a listing carries, and finds one past it by name on the server", async () => {
    const user = userEvent.setup();
    // A busy /tmp: its listing says it carries 5,000 of 5,942 entries, and the folder wanted sorts after
    // them. A few stand for them all, as the picker shows what it's given.
    const first = Array.from({ length: 40 }, (_, i) => `aa-${String(i).padStart(4, "0")}`);
    const busy = vi.fn(async (path: string, find?: string): Promise<FolderListing> => {
      if (path === "/tmp" && find) {
        const names = [...first, "shop-1", "shop-2"].filter((n) => n.includes(find));
        return { path, folders: names.map((name) => ({ name })) };
      }
      if (path === "/tmp") return { path, folders: first.map((name) => ({ name })), cut: { shown: 5000, total: 5942 } };
      if (path === "/tmp/shop-2") return { path, folders: [] };
      throw new Error("not there");
    });
    function Busy() {
      const [value, setValue] = useState("/tmp");
      return (
        <>
          <FolderPicker label="Folder" value={value} onChange={setValue} list={busy} defaultOpen />
          <output>{value}</output>
        </>
      );
    }
    const { container } = render(<Busy />);
    expect(await screen.findByText("Showing 5,000 of 5,942. Type a name to find one.")).toBeVisible();
    const find = screen.getByRole("searchbox", { name: "Find in /tmp" });
    expect(find).toHaveAccessibleDescription("Showing 5,000 of 5,942. Type a name to find one.");
    await user.type(find, "shop");
    const folders = screen.getByRole("list", { name: "Folders in /tmp" });
    await within(folders).findByRole("button", { name: "shop-2" }, { timeout: 3_000 });
    // Searched on the server, in the folder being looked in, not among what the listing carried.
    expect(busy).toHaveBeenLastCalledWith("/tmp", "shop");
    expect(within(folders).queryByRole("button", { name: "aa-0001" })).not.toBeInTheDocument();
    await user.click(within(folders).getByRole("button", { name: "shop-2" }));
    expect(container.querySelector("output")).toHaveTextContent("/tmp/shop-2");
    // Looking in the folder chosen starts afresh: nothing to find, and nothing cut.
    await waitFor(() => expect(screen.queryByRole("searchbox")).not.toBeInTheDocument());
    await expectAccessible(container);
  });

  it("says no folder holds a name, and that more do than a search shows", async () => {
    const user = userEvent.setup();
    const busy = vi.fn(async (path: string, find?: string): Promise<FolderListing> => {
      if (find === "zz") return { path, folders: [] };
      if (find === "a") return { path, folders: [{ name: "a1" }], cut: { shown: 5000, total: 6000 } };
      return { path, folders: [], cut: { shown: 5000, total: 0 } };
    });
    render(<FolderPicker label="Folder" value="/tmp" onChange={() => {}} list={busy} defaultOpen />);
    // A server that can't say how many there are still says the list is cut.
    expect(await screen.findByText("Showing the first 5,000. Type a name to find one.")).toBeVisible();
    // None of what it shows is a folder, which says nothing of what's past it.
    expect(screen.getByText("No folders among these")).toBeVisible();
    expect(screen.queryByText("No folders here")).not.toBeInTheDocument();
    const find = screen.getByRole("searchbox", { name: "Find in /tmp" });
    await user.type(find, "zz");
    expect(await screen.findByText("No folder here holds “zz”", {}, { timeout: 3_000 })).toBeVisible();
    await user.clear(find);
    await user.type(find, "a");
    expect(
      await screen.findByText("More match than this shows. Type more of the name.", {}, { timeout: 3_000 }),
    ).toBeVisible();
  });

  it("opens on what was refused, and stays open as it's fixed", async () => {
    const user = userEvent.setup();
    // A form that refuses an empty path, and forgets that as soon as anything is typed.
    function Refusing() {
      const [value, setValue] = useState("");
      const [error, setError] = useState<string>();
      return (
        <>
          <FolderPicker
            label="Folder"
            value={value}
            error={error}
            list={list}
            onChange={(next) => {
              setValue(next);
              setError(undefined);
            }}
          />
          <button type="button" onClick={() => setError("Give the folder that holds its files.")}>
            Check
          </button>
        </>
      );
    }
    render(<Refusing />);
    expect(screen.queryByRole("textbox", { name: "Path" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Check" }));
    const path = await screen.findByRole("textbox", { name: "Path" });
    await user.type(path, "/srv/x");
    expect(screen.getByRole("textbox", { name: "Path" })).toHaveValue("/srv/x");
    expect(screen.getByRole("textbox", { name: "Path" })).toHaveFocus();
  });

  it("describes Change by what's known of the folder, and says it as it changes", () => {
    render(<Picker note="Root owns it, so PHP can't save uploads there." />);
    const change = screen.getByRole("button", { name: /^Change the folder/ });
    expect(change).toHaveAccessibleDescription("Root owns it, so PHP can't save uploads there.");
    expect(screen.getByText("Root owns it, so PHP can't save uploads there.")).toHaveAttribute("aria-live", "polite");
  });

  it("is off, with why, for a reader who may not look in the server's folders", () => {
    render(<FolderPicker label="Folder" value="/var/www/x" list={list} onChange={() => {}} off="Only an admin can." />);
    expect(screen.getByRole("button", { name: /^Change the folder/ })).toHaveAttribute("aria-disabled", "true");
  });

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
