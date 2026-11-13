// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import { timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  CatalogFormat,
  CatalogInstallSchema,
  CatalogSourceInfoSchema,
  type CatalogStore,
  CatalogStoreSchema,
  ChangeSchema,
  ChangeState,
  EntrySchema,
  HostAccessKind,
  PlanAction,
  PlanNoteSchema,
  Role,
  type SetCatalogSourceRequest,
  type WatchChangeResponse,
} from "@dockwell/api";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { authState, mira } from "../../test/auth";
import { type Api, feed, onPhone, renderAt } from "../../test/render";

afterEach(() => {
  vi.unstubAllGlobals();
});

const COMMIT = "0123456789abcdef0123456789abcdef01234567";
const REPO = "https://github.com/example/catalog";

const notes = create(EntrySchema, {
  id: "notes-src",
  name: "Notes",
  summary: "Keeps notes",
  category: "storage",
  version: "1.2",
});
const wiki = create(EntrySchema, {
  id: "wiki-src",
  name: "Wiki",
  summary: "Keeps a wiki",
  category: "storage",
  version: "2.0",
});
const postgres = create(EntrySchema, {
  id: "postgres",
  name: "PostgreSQL",
  summary: "Relational database",
  category: "databases",
  version: "17.11",
});

const community = create(CatalogSourceInfoSchema, {
  name: "community",
  source: { repository: REPO, path: "entries", commit: COMMIT },
  entries: ["notes-src", "wiki-src"],
  refused: [{ file: "broken.yaml", reason: "its Compose file can't be read" }],
});

/** The same source, following its main branch, looked at a moment ago. */
const following = create(CatalogSourceInfoSchema, {
  name: "community",
  entries: ["notes-src", "wiki-src"],
  refused: [{ file: "broken.yaml", reason: "its Compose file can't be read" }],
  source: { repository: REPO, path: "entries", commit: COMMIT, follow: "main" },
  checked: timestampFromDate(new Date(Date.now() - 5 * 60_000)),
  branchCommit: COMMIT,
});

const ONEPANEL = "https://github.com/1Panel-dev/appstore";

/** The stores the control plane reads, as this build measured them (ADR 0083). */
const stores = [
  create(CatalogStoreSchema, {
    id: "onepanel",
    name: "1Panel's app store",
    label: "1Panel",
    repository: ONEPANEL,
    format: CatalogFormat.ONEPANEL,
    branch: "dev",
    license: "GPL-3.0-only",
    apps: 269,
    installs: 93,
    commit: COMMIT,
  }),
  create(CatalogStoreSchema, {
    id: "dokploy",
    name: "Dokploy's templates",
    label: "Dokploy",
    repository: "https://github.com/Dokploy/templates",
    format: CatalogFormat.DOKPLOY,
    branch: "canary",
    license: "MIT",
    apps: 612,
    installs: 169,
    commit: COMMIT,
  }),
];

function api(more: Api["catalog"] = {}): Api {
  return {
    catalog: {
      listCatalogEntries: () => ({ entries: [notes, postgres, wiki] }),
      listCatalogSources: () => ({ sources: [community] }),
      listCatalogStores: () => ({ stores }),
      listCatalogInstalls: () => ({ installs: [] }),
      ...more,
    },
    changes: { watchChange: feed<WatchChangeResponse>().stream },
  };
}

/** An app's card in the catalog, which opens its page. */
async function card$(id: string): Promise<HTMLElement> {
  return waitFor(() => {
    const card = document.querySelector<HTMLElement>(`a.catalog-card[href="/catalog/${id}"]`);
    if (!card) throw new Error(`no card for ${id}`);
    return card;
  });
}

/** A plan for a source, with the notes the control plane writes. */
function planned(...reasons: [reason: string, names: string[]][]) {
  return create(ChangeSchema, {
    id: "chg_1",
    state: ChangeState.PLANNED,
    summary: "Add the catalog community",
    plan: {
      resources: [{ id: "cs_1", resource: { kind: "catalog.source", name: "community" }, action: PlanAction.CREATE }],
      notes: reasons.map(([reason, names]) => create(PlanNoteSchema, { reason, names, message: "The server's words" })),
    },
  });
}

describe("Catalog sources", () => {
  it("says what each source reads, which apps it gives, and what it leaves out", async () => {
    const user = userEvent.setup();
    const { container } = renderAt("/catalog/sources", api());
    const panel = (await screen.findByRole("heading", { name: "example/catalog" })).closest("section") as HTMLElement;
    expect(within(panel).getByText("github.com/example/catalog/entries")).toHaveAttribute("title", REPO);
    expect(within(panel).getByText(COMMIT.slice(0, 12))).toBeVisible();
    // A count and the way into the catalog filtered to it, never a chip for each app.
    expect(within(panel).getByText("2 apps from here")).toBeVisible();
    expect(within(panel).queryByRole("link", { name: "Notes" })).toBeNull();
    expect(within(panel).getByRole("link", { name: "Browse the apps from example/catalog" })).toHaveAttribute(
      "href",
      "/catalog?catalog=community",
    );
    await user.click(within(panel).getByText("1 file left out, and why"));
    expect(within(panel).getByText("its Compose file can't be read")).toBeVisible();
    await expectAccessible(container);
  });

  it("says when a source's commit can't be read, with the way to move it", async () => {
    const user = userEvent.setup();
    const unread = create(CatalogSourceInfoSchema, {
      ...community,
      entries: [],
      refused: [],
      unread: "The host didn't answer.",
    });
    renderAt("/catalog/sources", api({ listCatalogSources: () => ({ sources: [unread] }) }));
    expect(await screen.findByText("Can't be read now, so its apps are out of the catalog")).toBeVisible();
    expect(screen.getByText("The host didn't answer.")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Move to another commit" }));
    expect(await screen.findByRole("dialog", { name: "Change example/catalog" })).toBeVisible();
  });

  it("offers Try again when the sources can't be listed", async () => {
    const user = userEvent.setup();
    let fail = true;
    renderAt(
      "/catalog/sources",
      api({
        listCatalogSources: () => {
          if (fail) throw new ConnectError("the store didn't answer", Code.Unavailable);
          return { sources: [community] };
        },
      }),
    );
    expect(await screen.findByText("Couldn't read the sources")).toBeVisible();
    fail = false;
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("heading", { name: "example/catalog" })).toBeVisible();
  });

  it("with no sources, offers Dockwell's own catalog back in one click, and any other repository", async () => {
    const user = userEvent.setup();
    let asked: SetCatalogSourceRequest | undefined;
    const { container } = renderAt(
      "/catalog/sources",
      api({
        listCatalogSources: () => ({ sources: [] }),
        setCatalogSource: (req) => {
          asked = req;
          return { change: planned(["catalog_source_follows", ["refs/heads/main"]]) };
        },
      }),
    );
    const empty = (await screen.findByRole("heading", { name: "No catalogs to install from" })).closest(
      ".empty-state",
    ) as HTMLElement;
    expect(empty.querySelectorAll("p")).toHaveLength(1);
    // Its own buttons, not a second set in the header.
    expect(screen.getAllByRole("button", { name: "Add another source" })).toHaveLength(1);
    await expectAccessible(container);
    await user.click(within(empty).getByRole("button", { name: "Add Dockwell's catalog" }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked).toMatchObject({
      name: "dockwell",
      repository: "https://github.com/Dockwell/catalog",
      path: "entries",
      follow: "main",
      commit: "",
    });
    expect(await screen.findByRole("dialog", { name: "Review plan" })).toBeVisible();
  });

  it("opens the form for another repository from the empty state", async () => {
    const user = userEvent.setup();
    renderAt("/catalog/sources", api({ listCatalogSources: () => ({ sources: [] }) }));
    await user.click(await screen.findByRole("button", { name: "Add another source" }));
    expect(await screen.findByRole("dialog", { name: "Add a catalog source" })).toBeVisible();
  });

  it("calls Dockwell's own catalog so, and no other", async () => {
    const own = create(CatalogSourceInfoSchema, {
      name: "dockwell",
      own: true,
      entries: ["postgres"],
      source: { repository: "https://github.com/Dockwell/catalog", path: "entries", commit: COMMIT, follow: "main" },
    });
    renderAt("/catalog/sources", api({ listCatalogSources: () => ({ sources: [own, community] }) }));
    const panel = (await screen.findByRole("heading", { name: "Dockwell's own" })).closest("section") as HTMLElement;
    expect(within(panel).getByTitle("From the catalog dockwell")).toBeVisible();
    expect(within(panel).getByRole("button", { name: "Change Dockwell's own" })).toBeVisible();
    const other = (await screen.findByRole("heading", { name: "example/catalog" })).closest("section") as HTMLElement;
    expect(within(other).queryByText("Dockwell's own")).not.toBeInTheDocument();
  });

  it("asks for the repository alone, reads it as it's typed and pins the branch it leads with", async () => {
    const user = userEvent.setup();
    let asked: SetCatalogSourceRequest | undefined;
    const looked: string[] = [];
    const { container } = renderAt(
      "/catalog/sources",
      api({
        listCatalogSources: () => ({ sources: [] }),
        listCatalogSourceRefs: (req) => {
          looked.push(req.repository);
          return {
            refs: [
              { name: "refs/heads/dev", commit: "b".repeat(40) },
              { name: "refs/heads/main", commit: COMMIT },
              { name: "refs/tags/v1", commit: "c".repeat(40) },
            ],
          };
        },
        setCatalogSource: (req) => {
          asked = req;
          return { change: planned(["catalog_source_reads", ["notes-src"]]) };
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add another source" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a catalog source" });
    // Nothing to fill but the repository: no name, folder or commit field until asked for.
    expect(within(sheet).getAllByRole("textbox")).toHaveLength(1);
    await user.type(within(sheet).getByLabelText("Repository"), REPO);
    expect(await within(sheet).findByText("Read. It has 3 branches and tags.")).toBeVisible();
    expect(looked).toEqual([REPO]);
    const pin = within(sheet).getByLabelText("Branch");
    expect(pin).toHaveValue("refs/heads/main");
    // It follows the branch it leads with, unless asked to stand.
    expect(within(sheet).getByRole("switch", { name: "Follow main" })).toBeChecked();
    expect(
      within(sheet).getByText("Its new commits come in on their own. What's installed stays as it is."),
    ).toBeVisible();
    expect(within(sheet).getByText("Named example · entries at its top")).toBeVisible();
    expect(within(sheet).queryByLabelText("Commit")).not.toBeInTheDocument();
    await expectAccessible(container);

    // A tag never moves, so it's pinned, and says so.
    await user.selectOptions(pin, "v1 (tag)");
    expect(within(sheet).queryByRole("switch")).not.toBeInTheDocument();
    expect(within(sheet).getByText(`At ${"c".repeat(12)} now. It stays there until you move it.`)).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked).toMatchObject({ name: "example", repository: REPO, path: "", commit: "c".repeat(40), follow: "" });
  });

  it("waits for the repository's branches when Review plan is pressed before they're read, then goes on", async () => {
    const user = userEvent.setup();
    let asked: SetCatalogSourceRequest | undefined;
    let answer!: () => void;
    const answered = new Promise<void>((done) => {
      answer = done;
    });
    renderAt(
      "/catalog/sources",
      api({
        listCatalogSourceRefs: async () => {
          await answered;
          return { refs: [{ name: "refs/heads/main", commit: COMMIT }], head: "refs/heads/main" };
        },
        setCatalogSource: (req) => {
          asked = req;
          return { change: planned(["catalog_source_reads", ["notes-src"]]) };
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a source" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a catalog source" });
    await user.type(within(sheet).getByLabelText("Repository"), "https://github.com/example/apps");
    // At once, before the address has stood still long enough to be read.
    await user.keyboard("{Control>}{Enter}{/Control}");
    const review = within(sheet).getByRole("button", { name: /Review plan/ });
    expect(review).toHaveAttribute("aria-busy", "true");
    expect(within(sheet).queryByText(/Use the commit in full/)).not.toBeInTheDocument();
    expect(await within(sheet).findByText("Reading the repository")).toBeVisible();
    expect(asked).toBeUndefined();
    await act(async () => answer());
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked).toMatchObject({ repository: "https://github.com/example/apps", commit: COMMIT, follow: "main" });
    expect(within(sheet).queryByText(/Use the commit in full/)).not.toBeInTheDocument();
  });

  it("leaves a waiting press for the commit field when the repository can't be read", async () => {
    const user = userEvent.setup();
    const set = vi.fn();
    renderAt(
      "/catalog/sources",
      api({
        listCatalogSourceRefs: () => {
          throw new ConnectError("example.com answered 404 Not Found", Code.FailedPrecondition);
        },
        setCatalogSource: set,
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a source" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a catalog source" });
    await user.type(within(sheet).getByLabelText("Repository"), REPO);
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(await within(sheet).findByLabelText("Commit")).toBeVisible();
    expect(within(sheet).getByRole("button", { name: /Review plan/ })).not.toHaveAttribute("aria-busy");
    expect(within(sheet).queryByText(/Use the commit in full/)).not.toBeInTheDocument();
    expect(set).not.toHaveBeenCalled();
  });

  it("leads with the repository's default branch, and says when it holds more than are listed", async () => {
    const user = userEvent.setup();
    const many = Array.from({ length: 499 }, (_, i) => ({
      name: `refs/heads/a-${String(i).padStart(3, "0")}`,
      commit: "b".repeat(40),
    }));
    renderAt(
      "/catalog/sources",
      api({
        listCatalogSourceRefs: () => ({
          refs: [{ name: "refs/heads/trunk", commit: COMMIT }, ...many],
          head: "refs/heads/trunk",
          more: true,
        }),
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a source" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a catalog source" });
    await user.type(within(sheet).getByLabelText("Repository"), REPO);
    expect(await within(sheet).findByText("Read. It has 500 or more branches and tags.")).toBeVisible();
    expect(within(sheet).getByLabelText("Branch")).toHaveValue("refs/heads/trunk");
  });

  it("says what's wrong with a repository beside it, before any plan", async () => {
    const user = userEvent.setup();
    const set = vi.fn();
    renderAt(
      "/catalog/sources",
      api({
        listCatalogSourceRefs: () => {
          const err = new ConnectError(
            "Dockwell can't read it: example.com answered 404 Not Found",
            Code.FailedPrecondition,
          );
          err.metadata.set("Dockwell-Error", "unreadable");
          throw err;
        },
        setCatalogSource: set,
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a source" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a catalog source" });
    await user.type(within(sheet).getByLabelText("Repository"), REPO);
    expect(await within(sheet).findByText(/example.com answered 404 Not Found/)).toBeVisible();
    // A power user can still pin a commit by hand.
    expect(within(sheet).getByLabelText("Commit")).toBeVisible();
    expect(set).not.toHaveBeenCalled();
  });

  it("says which branch a typed commit is, or that none is at it", async () => {
    const user = userEvent.setup();
    renderAt(
      "/catalog/sources",
      api({ listCatalogSourceRefs: () => ({ refs: [{ name: "refs/heads/main", commit: COMMIT }] }) }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a source" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a catalog source" });
    await user.type(within(sheet).getByLabelText("Repository"), REPO);
    await user.selectOptions(await within(sheet).findByLabelText("Branch"), "Another commit");
    await user.type(await within(sheet).findByLabelText("Commit"), COMMIT);
    expect(within(sheet).getByText("main is at this commit.")).toBeVisible();
    await user.clear(within(sheet).getByLabelText("Commit"));
    await user.type(await within(sheet).findByLabelText("Commit"), "d".repeat(40));
    expect(within(sheet).getByText("No branch or tag is at this commit.")).toBeVisible();
  });

  it("won't take a name a source already has, as that would move it", async () => {
    const user = userEvent.setup();
    const set = vi.fn();
    renderAt("/catalog/sources", api({ setCatalogSource: set }));
    await user.click(await screen.findByRole("button", { name: "Add a source" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a catalog source" });
    await user.type(within(sheet).getByLabelText("Repository"), "https://github.com/community/apps");
    // The default steps aside from the one that's taken.
    expect(await within(sheet).findByText("Named community-2 · entries at its top")).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Change" }));
    await user.clear(within(sheet).getByLabelText("Name"));
    await user.type(within(sheet).getByLabelText("Name"), "community");
    await user.type(await within(sheet).findByLabelText("Commit"), COMMIT);
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(await within(sheet).findByText("A source is already named community. Use another name.")).toBeVisible();
    expect(set).not.toHaveBeenCalled();
  });

  it("adds a source through a plan, checking what's typed first", async () => {
    const user = userEvent.setup();
    let asked: SetCatalogSourceRequest | undefined;
    const { container } = renderAt(
      "/catalog/sources",
      api({
        setCatalogSource: (req) => {
          asked = req;
          return {
            change: planned(["catalog_source_reads", ["notes-src"]], ["catalog_source_commit", ["refs/heads/main"]]),
          };
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a source" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a catalog source" });
    await user.click(within(sheet).getByRole("button", { name: "Change" }));
    await user.clear(within(sheet).getByLabelText("Name"));
    await user.type(within(sheet).getByLabelText("Name"), "Community");
    await user.type(within(sheet).getByLabelText("Repository"), "http://example.com/catalog");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(await within(sheet).findByText(/Use lowercase letters, digits and dashes/)).toBeVisible();
    expect(within(sheet).getByText(/Use the repository's https:\/\/ URL/)).toBeVisible();
    expect(asked).toBeUndefined();
    await expectAccessible(container);

    await user.clear(within(sheet).getByLabelText("Name"));
    await user.type(within(sheet).getByLabelText("Name"), "extras");
    await user.clear(within(sheet).getByLabelText("Repository"));
    await user.type(within(sheet).getByLabelText("Repository"), ` ${REPO} `);
    await user.type(within(sheet).getByLabelText("Folder"), "entries");
    // This repository offers no branch here, so the commit is typed, and a branch's name isn't one.
    await user.type(await within(sheet).findByLabelText("Commit"), "main");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(
      await within(sheet).findByText(/Use the commit in full, as 40 hex digits, or choose a branch/),
    ).toBeVisible();
    expect(asked).toBeUndefined();
    await user.clear(within(sheet).getByLabelText("Commit"));
    await user.type(within(sheet).getByLabelText("Commit"), COMMIT.toUpperCase());
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked).toMatchObject({ name: "extras", repository: REPO, path: "entries", commit: COMMIT });
    const plan = await screen.findByRole("dialog", { name: "Review plan" });
    expect(within(plan).getByText("The catalog shows 1 app from this commit: notes-src.")).toBeVisible();
    expect(within(plan).getByText("It's the commit the branch main is at.")).toBeVisible();
  });

  it("says why a commit can't be read, in the sheet, with what was typed", async () => {
    const user = userEvent.setup();
    renderAt(
      "/catalog/sources",
      api({
        setCatalogSource: () => {
          const err = new ConnectError("example.com answered 404 Not Found", Code.FailedPrecondition);
          err.metadata.set("Dockwell-Error", "unreadable");
          throw err;
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a source" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a catalog source" });
    await user.type(within(sheet).getByLabelText("Repository"), REPO);
    await user.type(await within(sheet).findByLabelText("Commit"), COMMIT);
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    expect(
      await within(sheet).findByText(/Dockwell can't read that commit: example.com answered 404 Not Found/),
    ).toBeVisible();
    expect(within(sheet).getByLabelText("Repository")).toHaveValue(REPO);
  });

  it("moves a source to another commit, and removes one, each through a plan", async () => {
    const user = userEvent.setup();
    let removed = "";
    renderAt(
      "/catalog/sources",
      api({
        removeCatalogSource: (req) => {
          removed = req.name;
          return { change: planned(["catalog_source_removed", []], ["catalog_source_strands", ["notes"]]) };
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Change example/catalog" }));
    const sheet = await screen.findByRole("dialog", { name: "Change example/catalog" });
    expect(within(sheet).getByLabelText("Repository")).toHaveValue(REPO);
    expect(within(sheet).getByText("Entries at entries")).toBeVisible();
    await user.click(within(sheet).getByRole("button", { name: "Change" }));
    expect(within(sheet).getByLabelText("Folder")).toHaveValue("entries");
    expect(within(sheet).queryByLabelText("Name")).not.toBeInTheDocument();
    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));

    await user.click(screen.getByRole("button", { name: "Remove example/catalog" }));
    await waitFor(() => expect(removed).toBe("community"));
    const plan = await screen.findByRole("dialog", { name: "Review plan" });
    // A warning reads as its fact, then what it means.
    expect(plan.textContent).toContain("Its apps leave the catalog");
    // The fact, and why behind "How this works".
    expect(within(plan).getAllByText(/notes will have no upgrades/)[0]).toBeVisible();
    expect(plan.textContent).toContain("Installs of them keep running, with no upgrades.");
    expect(plan.textContent).toContain("Each keeps running as it is.");
  });

  it.each(["close", "leave"])("discards a plan returned after you %s while its commit is read", async (how) => {
    const user = userEvent.setup();
    let resolve!: (value: { change: ReturnType<typeof planned> }) => void;
    const pending = new Promise<{ change: ReturnType<typeof planned> }>((done) => {
      resolve = done;
    });
    const requested = vi.fn(() => pending);
    const discard = vi.fn((req: { id: string }) => ({
      change: create(ChangeSchema, { id: req.id, state: ChangeState.DISCARDED }),
    }));
    const fixture = api({ setCatalogSource: requested });
    fixture.changes = { ...fixture.changes, discardChange: discard };
    const { router } = renderAt("/catalog/sources", fixture);
    await user.click(await screen.findByRole("button", { name: "Change example/catalog" }));
    const form = await screen.findByRole("dialog", { name: "Change example/catalog" });
    await user.type(await within(form).findByLabelText("Commit"), COMMIT);
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(requested).toHaveBeenCalledTimes(1));
    if (how === "close") {
      const reading = await screen.findByRole("dialog", { name: "Planning" });
      await user.click(within(reading).getByText("Close", { exact: true }));
      expect(await screen.findByLabelText("Commit")).toHaveValue(COMMIT);
    } else {
      await act(() => router.navigate({ to: "/catalog" }));
    }
    await act(async () => {
      resolve({ change: planned() });
      await pending;
    });
    await waitFor(() =>
      expect(discard).toHaveBeenCalledWith(expect.objectContaining({ id: "chg_1" }), expect.anything()),
    );
    expect(screen.queryByRole("dialog", { name: "Review plan" })).not.toBeInTheDocument();
  });

  it("keeps the warnings and version changes in a source plan made by an earlier server", async () => {
    const user = userEvent.setup();
    const old = planned();
    if (!old.plan) throw new Error("The fixture has no plan");
    old.plan.notes = [
      create(PlanNoteSchema, {
        reason: "catalog_source_commit",
        message: "No branch or tag is at this commit now. It may be a fork's commit; check it is the one you mean.",
      }),
      create(PlanNoteSchema, {
        reason: "catalog_source_changes",
        message: "notes-src moves from version 1.0 to 1.2, release 2",
      }),
    ];
    renderAt("/catalog/sources", api({ setCatalogSource: () => ({ change: old }) }));
    await user.click(await screen.findByRole("button", { name: "Change example/catalog" }));
    const form = await screen.findByRole("dialog", { name: "Change example/catalog" });
    await user.type(await within(form).findByLabelText("Commit"), COMMIT);
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    const plan = await screen.findByRole("dialog", { name: "Review plan" });
    expect(await within(plan).findByText("No branch or tag is at this commit now")).toBeVisible();
    expect(within(plan).getByText("It may be a fork's commit; check it is the one you mean.")).toBeVisible();
    expect(within(plan).getByText("notes-src moves from version 1.0 to 1.2, release 2")).toBeVisible();
    expect(plan.textContent).not.toContain("It's the commit  are at.");
  });

  it("keeps a refused file's reason whole, however the server words it", async () => {
    const user = userEvent.setup();
    renderAt(
      "/catalog/sources",
      api({ setCatalogSource: () => ({ change: planned(["catalog_source_refuses", ["broken.yaml"]]) }) }),
    );
    await user.click(await screen.findByRole("button", { name: "Change example/catalog" }));
    const sheet = await screen.findByRole("dialog", { name: "Change example/catalog" });
    await user.type(await within(sheet).findByLabelText("Commit"), COMMIT);
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    const plan = await screen.findByRole("dialog", { name: "Review plan" });
    expect(await within(plan).findByText(/Leaves out broken\.yaml/)).toBeVisible();
    // The fixture's message has no "file: why" shape, so all of it is the reason.
    expect(within(plan).getByText("The server's words")).toBeVisible();
  });

  it("words what a plan changes of where apps come from", async () => {
    const user = userEvent.setup();
    renderAt(
      "/catalog/sources",
      api({
        setCatalogSource: () => ({
          change: planned(
            ["catalog_source_unnamed", []],
            ["catalog_source_takes", ["foo"]],
            ["catalog_source_gives", ["bar"]],
            ["catalog_source_restores", ["baz"]],
          ),
        }),
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Change example/catalog" }));
    const sheet = await screen.findByRole("dialog", { name: "Change example/catalog" });
    await user.type(await within(sheet).findByLabelText("Commit"), COMMIT);
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    const plan = await screen.findByRole("dialog", { name: "Review plan" });
    expect(within(plan).getByText("No branch or tag is at this commit now")).toBeVisible();
    expect(within(plan).getAllByText("Check it's the commit you mean.")[0]).toBeVisible();
    expect(plan.textContent).toContain("It may be an older commit, or a fork's.");
    expect(within(plan).getAllByText(/Takes foo from another catalog/)[0]).toBeVisible();
    expect(plan.textContent).toContain("This catalog comes first, so the catalog shows its apps.");
    expect(within(plan).getAllByText(/bar will come from another catalog/)[0]).toBeVisible();
    expect(plan.textContent).toContain("Nothing installed from this catalog upgrades to them.");
    expect(within(plan).getByText("baz can upgrade again, from the catalog each came from.")).toBeVisible();
  });
});

describe("Catalog sources that follow a branch", () => {
  it("says which branch a source follows and when Dockwell looked, and looks now when asked", async () => {
    const user = userEvent.setup();
    let looked = "";
    const moved = create(ChangeSchema, {
      id: "chg_2",
      state: ChangeState.APPLIED,
      summary: "Move the catalog community, as main moved",
    });
    const fixture = api({
      listCatalogSources: () => ({ sources: [following] }),
      checkCatalogSource: (req) => {
        looked = req.name;
        return { source: following, change: moved };
      },
    });
    fixture.changes = { ...fixture.changes, getChange: () => ({ change: moved }) };
    const { container } = renderAt("/catalog/sources", fixture);
    const panel = (await screen.findByRole("heading", { name: "example/catalog" })).closest("section") as HTMLElement;
    expect(within(panel).getByText("Following main · checked 5 min ago")).toBeVisible();
    await expectAccessible(container);
    await user.click(within(panel).getByRole("button", { name: "Check now" }));
    await waitFor(() => expect(looked).toBe("community"));
    // What Dockwell moved it to opens, applied, with its undo there.
    expect(await screen.findByText("Move the catalog community, as main moved")).toBeVisible();
  });

  it("says why a source didn't follow its branch", async () => {
    const stuck = create(CatalogSourceInfoSchema, {
      ...following,
      problem:
        "None of the entries at 89abcdef0123, where main is now, pass Dockwell's checks. It stays at 0123456789ab.",
    });
    renderAt("/catalog/sources", api({ listCatalogSources: () => ({ sources: [stuck] }) }));
    expect(await screen.findByText("Didn't follow main")).toBeVisible();
    expect(screen.getByText(/None of the entries at 89abcdef0123/)).toBeVisible();
  });

  it("pins a following source where it is, through a plan", async () => {
    const user = userEvent.setup();
    let asked: SetCatalogSourceRequest | undefined;
    renderAt(
      "/catalog/sources",
      api({
        listCatalogSources: () => ({ sources: [following] }),
        setCatalogSource: (req) => {
          asked = req;
          return { change: planned(["catalog_source_pinned", []]) };
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Pin here" }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked).toMatchObject({ name: "community", repository: REPO, path: "entries", commit: COMMIT, follow: "" });
    const plan = await screen.findByRole("dialog", { name: "Review plan" });
    expect(
      await within(plan).findByText("Stops following its branch, and stays at this commit until you move it."),
    ).toBeVisible();
  });

  it("changes a following source with its branch chosen and followed, and can leave it to stand", async () => {
    const user = userEvent.setup();
    let asked: SetCatalogSourceRequest | undefined;
    renderAt(
      "/catalog/sources",
      api({
        listCatalogSources: () => ({ sources: [following] }),
        listCatalogSourceRefs: () => ({
          refs: [
            { name: "refs/heads/dev", commit: "b".repeat(40) },
            { name: "refs/heads/main", commit: COMMIT },
          ],
          head: "refs/heads/dev",
        }),
        setCatalogSource: (req) => {
          asked = req;
          return { change: planned(["catalog_source_follows", ["refs/heads/main"]]) };
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Change example/catalog" }));
    const sheet = await screen.findByRole("dialog", { name: "Change example/catalog" });
    expect(await within(sheet).findByLabelText("Branch")).toHaveValue("refs/heads/main");
    const follow = within(sheet).getByRole("switch", { name: "Follow main" });
    expect(follow).toBeChecked();
    await user.click(follow);
    expect(within(sheet).getByText(`Pinned at ${COMMIT.slice(0, 12)}.`)).toBeVisible();
    await user.click(follow);
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked).toMatchObject({ name: "community", commit: COMMIT, follow: "main" });
    const plan = await screen.findByRole("dialog", { name: "Review plan" });
    expect(
      await within(plan).findByText("Follows main: Dockwell moves the catalog along it, by changes you can undo."),
    ).toBeVisible();
  });
});

describe("Catalog sources, for whoever may not change them", () => {
  it("leaves Add, Change, Check now, Pin and Remove where they are for a developer, off, with why", async () => {
    const fixture = api({ listCatalogSources: () => ({ sources: [following] }) });
    fixture.auth = {
      getAuthState: () =>
        authState({
          user: { ...mira, role: Role.DEVELOPER },
          grants: [{ projectId: "", role: Role.DEVELOPER }],
        }),
    };
    renderAt("/catalog/sources", fixture);
    await screen.findByRole("heading", { name: "example/catalog" });
    const why = "Only an admin or the owner can do this. Ask one of them.";
    for (const control of [
      screen.getByRole("button", { name: "Add a source" }),
      screen.getByRole("button", { name: "Change example/catalog" }),
      screen.getByRole("button", { name: "Remove example/catalog" }),
      screen.getByRole("button", { name: "Check now" }),
      screen.getByRole("button", { name: "Pin here" }),
    ]) {
      expect(control).toHaveAttribute("aria-disabled", "true");
      expect(control).toHaveAccessibleDescription(why);
    }
    expect(screen.getByRole("button", { name: "Add 1Panel's app store" })).toHaveAttribute("aria-disabled", "true");
    // What a source gives is anyone's to read.
    expect(screen.getByRole("link", { name: "Browse the apps from example/catalog" })).toBeVisible();
  });
});

describe("Catalog sources on a phone", () => {
  it("leads the page with Add a source, as the header has no room for it", async () => {
    onPhone();
    renderAt("/catalog/sources", api());
    await screen.findByRole("heading", { name: "example/catalog" });
    expect(screen.getByRole("button", { name: "Add a source" })).toBeVisible();
  });

  it("is reached from the catalog's foot as well as its header", async () => {
    onPhone();
    renderAt("/catalog", api());
    expect(await screen.findByRole("link", { name: "Sources" })).toHaveAttribute("href", "/catalog/sources");
  });
});

describe("The catalog, with sources", () => {
  it("shows a source's trust notice beside the parts and host access it installs", async () => {
    const user = userEvent.setup();
    renderAt(
      "/catalog/notes-src/install",
      api({
        getCatalogEntry: () => ({
          entry: notes,
          project: "notes",
          parts: [
            {
              service: "web",
              image: "example/notes:1.2",
              opens: true,
              volumes: ["data"],
              hostAccess: [{ kind: HostAccessKind.DOCKER_SOCKET, path: "/var/run/docker.sock", sameAsRoot: true }],
            },
          ],
        }),
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Install Notes" });
    expect(await within(sheet).findByText(/Not tested by Dockwell/)).toBeVisible();
    expect(within(sheet).getByRole("group", { name: "Catalog" })).toHaveTextContent(
      "example/catalog · Not tested by Dockwell",
    );
    // What it installs is under Advanced, one step away.
    await user.click(within(sheet).getByRole("button", { name: "Advanced" }));
    const parts = within(sheet).getByRole("region", { name: "What it installs" });
    expect(within(parts).getByText("example/notes:1.2")).toBeVisible();
    expect(within(parts).getByText("data")).toBeVisible();
    expect(within(parts).getByText("/var/run/docker.sock").closest(".dw-diff-critical")).not.toBeNull();
    expect(within(sheet).getByRole("button", { name: /Review plan/ })).toBeEnabled();
  });

  it("still shows the catalog when the sources can't be read, and Try again reads both", async () => {
    const user = userEvent.setup();
    let fail = true;
    let entriesRead = 0;
    renderAt(
      "/catalog",
      api({
        listCatalogEntries: () => {
          entriesRead++;
          return { entries: [notes, postgres, wiki] };
        },
        listCatalogSources: () => {
          if (fail) throw new ConnectError("the store didn't answer", Code.Unavailable);
          return { sources: [community] };
        },
      }),
    );
    expect(await screen.findByText("Couldn't read the sources, so apps from them aren't marked")).toBeVisible();
    // Dockwell's own apps are still there to browse.
    expect(await card$("postgres")).toBeVisible();
    expect(screen.queryByTitle("From the catalog community")).not.toBeInTheDocument();
    const before = entriesRead;
    fail = false;
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findAllByTitle("From the catalog community")).not.toHaveLength(0);
    expect(entriesRead).toBeGreaterThan(before);
  });

  it("says which apps come from a source, and counts only installs from the catalog each came from", async () => {
    // Installed from Dockwell's own catalog, where an app of the same ID was then:
    // the source's notes-src is another app.
    const fromOwn = create(CatalogInstallSchema, {
      project: "notes",
      projectId: "prj_1",
      environmentId: "env_1",
      pin: { entry: "notes-src", version: "1.0", catalog: "" },
    });
    renderAt("/catalog", api({ listCatalogInstalls: () => ({ installs: [fromOwn] }) }));
    const card = await card$("notes-src");
    expect(within(card).getByTitle("From the catalog community")).toHaveTextContent("example/catalog");
    expect(card).not.toHaveTextContent("Installed");
    expect(await card$("postgres")).not.toHaveTextContent("example/catalog");
  });

  it.each([
    ["repository", "https://github.com/other/catalog", "entries"],
    ["folder", REPO, "other-entries"],
  ])("does not open the old source's install after its %s changes", async (_, repository, path) => {
    const old = create(CatalogInstallSchema, {
      project: "old-notes",
      projectId: "prj_old",
      environmentId: "env_old",
      webServiceId: "svc_old",
      pin: { entry: "notes-src", version: "1.0", catalog: "community", repository, path },
    });
    renderAt("/catalog", api({ listCatalogInstalls: () => ({ installs: [old] }) }));
    await screen.findAllByTitle("From the catalog community");
    expect(await card$("notes-src")).not.toHaveTextContent("Installed");
  });

  it.each([
    ["repository", "https://github.com/other/catalog", "entries"],
    ["folder", REPO, "other-entries"],
  ])("keeps the old source's install off the app's page after its %s changes", async (_, repository, path) => {
    const old = create(CatalogInstallSchema, {
      project: "old-notes",
      projectId: "prj_old",
      environmentId: "env_old",
      webServiceId: "svc_old",
      pin: { entry: "notes-src", version: "1.0", catalog: "community", repository, path },
    });
    renderAt(
      "/catalog/notes-src",
      api({ listCatalogInstalls: () => ({ installs: [old] }), getCatalogEntry: () => ({ entry: notes }) }),
    );
    await screen.findByRole("heading", { name: "Where it comes from" });
    expect(screen.queryByRole("heading", { name: "Your installations" })).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Install Notes" })).toBeVisible();
  });

  it("still opens an install from an older commit of the same source", async () => {
    const old = create(CatalogInstallSchema, {
      project: "notes",
      webServiceId: "svc_notes",
      pin: {
        entry: "notes-src",
        version: "1.0",
        catalog: "community",
        repository: REPO,
        path: "entries",
        commit: "a".repeat(40),
      },
    });
    const view = renderAt("/catalog", api({ listCatalogInstalls: () => ({ installs: [old] }) }));
    await waitFor(async () => expect(await card$("notes-src")).toHaveTextContent("Installed"));
    view.unmount();
    renderAt(
      "/catalog/notes-src",
      api({ listCatalogInstalls: () => ({ installs: [old] }), getCatalogEntry: () => ({ entry: notes }) }),
    );
    expect(await screen.findByRole("link", { name: "Open app: notes" })).toHaveAttribute("href", "/apps/svc_notes");
  });

  it("says where a source's app comes from on its page, and that Dockwell doesn't test it", async () => {
    const { container } = renderAt("/catalog/notes-src", api({ getCatalogEntry: () => ({ entry: notes }) }));
    const origin = await screen.findByRole("region", { name: "Where it comes from" });
    expect(within(origin).getByText("Not tested by Dockwell")).toBeVisible();
    expect(within(origin).getByRole("link", { name: "example/catalog" })).toHaveAttribute("href", "/catalog/sources");
    expect(within(origin).getByText(REPO)).toBeVisible();
    expect(within(origin).getByText(COMMIT.slice(0, 12))).toHaveAttribute("title", COMMIT);
    // A state and its facts: no paragraph of advice.
    expect(origin.textContent).not.toMatch(/Check the images/);
    await expectAccessible(container);
  });

  it("says an app of Dockwell's own catalog is its own, with no origin to weigh", async () => {
    renderAt("/catalog/postgres", api({ getCatalogEntry: () => ({ entry: postgres }) }));
    expect(await screen.findByText("Dockwell's own")).toBeVisible();
    expect(screen.queryByRole("region", { name: "Where it comes from" })).not.toBeInTheDocument();
  });

  it("doesn't call an app Dockwell's own while the sources can't be read", async () => {
    const user = userEvent.setup();
    let unavailable = true;
    renderAt(
      "/catalog/notes-src",
      api({
        listCatalogSources: () => {
          if (unavailable) throw new ConnectError("sources unavailable", Code.Unavailable);
          return { sources: [community] };
        },
        getCatalogEntry: () => ({ entry: notes }),
      }),
    );
    expect(await screen.findByText("Couldn't read the sources")).toBeVisible();
    expect(screen.queryByText("Dockwell's own")).not.toBeInTheDocument();
    unavailable = false;
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("region", { name: "Where it comes from" })).toBeVisible();
  });

  it("keeps an unknown origin apart from a built-in app until the sources can be read", async () => {
    const user = userEvent.setup();
    let unavailable = true;
    const install = vi.fn();
    renderAt(
      "/catalog/notes-src/install",
      api({
        listCatalogSources: () => {
          if (unavailable) throw new ConnectError("sources unavailable", Code.Unavailable);
          return { sources: [community] };
        },
        getCatalogEntry: () => ({ entry: notes, project: "notes" }),
        installCatalogEntry: install,
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Install Notes" });
    expect(await within(sheet).findByText("Couldn't read the sources")).toBeVisible();
    const review = within(sheet).getByRole("button", { name: /Review plan/ });
    expect(review).toBeDisabled();
    await user.click(within(sheet).getByRole("button", { name: "Change name" }));
    await user.type(within(sheet).getByLabelText("Name"), "-test");
    await user.keyboard("{Control>}{Enter}{/Control}");
    expect(install).not.toHaveBeenCalled();
    unavailable = false;
    await user.click(within(sheet).getByRole("button", { name: "Try again" }));
    expect(await within(sheet).findByText(/Not tested by Dockwell/)).toBeVisible();
    expect(within(sheet).getByLabelText("Name")).toHaveValue("notes-test");
    expect(review).toBeEnabled();
  });

  it("waits for a source lookup before making an install plan", async () => {
    let resolve!: (value: { sources: (typeof community)[] }) => void;
    const pending = new Promise<{ sources: (typeof community)[] }>((done) => {
      resolve = done;
    });
    renderAt(
      "/catalog/notes-src/install",
      api({
        listCatalogSources: () => pending,
        getCatalogEntry: () => ({ entry: notes, project: "notes" }),
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Install Notes" });
    await within(sheet).findByText("Reading the sources");
    expect(within(sheet).getByRole("button", { name: /Review plan/ })).toBeDisabled();
    await act(async () => {
      resolve({ sources: [community] });
      await pending;
    });
    await waitFor(() => expect(within(sheet).getByRole("button", { name: /Review plan/ })).toBeEnabled());
    expect(within(sheet).getByText(/Not tested by Dockwell/)).toBeVisible();
  });
});

describe("Other stores' catalogs", () => {
  const store = create(CatalogSourceInfoSchema, {
    name: "onepanel",
    source: { repository: ONEPANEL, commit: COMMIT, format: CatalogFormat.ONEPANEL, follow: "dev" },
    store: "onepanel",
    entries: ["notes-src"],
    also: ["postgres"],
    refused: [
      { file: "apps/2fauth/5.0.0", id: "2fauth", kind: "scripts", reason: "it runs scripts on the server" },
      { file: "apps/alist/3.0.0", id: "alist", kind: "scripts", reason: "it runs scripts on the server" },
      { file: "apps/artalk", id: "artalk", kind: "arch", reason: "1Panel says it runs on amd64 alone" },
    ],
  });

  it("lists the stores Dockwell reads with what each brings and its licence, and adds one in one click, straight to its plan", async () => {
    const user = userEvent.setup();
    let asked: SetCatalogSourceRequest | undefined;
    const { container } = renderAt(
      "/catalog/sources",
      api({
        listCatalogSources: () => ({ sources: [community] }),
        setCatalogSource: (req) => {
          asked = req;
          return { change: planned(["catalog_source_reads", ["notes-src"]]) };
        },
      }),
    );
    const gallery = (await screen.findByRole("heading", { name: "App stores" })).closest("section") as HTMLElement;
    const tiles = within(gallery).getAllByRole("listitem");
    expect(tiles.map((tile) => within(tile).getByText(/app store|templates/).textContent)).toEqual([
      "1Panel's app store",
      "Dokploy's templates",
    ]);
    expect(within(tiles[0] as HTMLElement).getByText("About 93 of 269 apps install")).toBeVisible();
    expect(within(tiles[0] as HTMLElement).getByText("GPL-3.0-only")).toBeVisible();
    await expectAccessible(container);
    await user.click(within(gallery).getByRole("button", { name: "Add 1Panel's app store" }));
    // Nothing to type: the plan, at once, following the store's default branch, read as that store.
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(asked).toMatchObject({
      name: "onepanel",
      repository: ONEPANEL,
      path: "",
      commit: "",
      follow: "dev",
      format: CatalogFormat.ONEPANEL,
    });
  });

  it("names a store's source by the next name free where its ID is taken", async () => {
    const user = userEvent.setup();
    let asked: SetCatalogSourceRequest | undefined;
    const taken = create(CatalogSourceInfoSchema, { ...community, name: "onepanel" });
    renderAt(
      "/catalog/sources",
      api({
        listCatalogSources: () => ({ sources: [taken] }),
        setCatalogSource: (req) => {
          asked = req;
          return { change: planned(["catalog_source_reads", ["notes-src"]]) };
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add 1Panel's app store" }));
    await waitFor(() => expect(asked?.name).toBe("onepanel-2"));
  });

  it("shows a store a source reads as added, with the way to that source, and never takes it off the list", async () => {
    const user = userEvent.setup();
    const scrolled = vi.fn();
    Element.prototype.scrollIntoView = scrolled;
    renderAt(
      "/catalog/sources",
      api({
        listCatalogSources: () => ({ sources: [community, store] }),
        listCatalogStores: () => ({
          stores: [
            create(CatalogStoreSchema, {
              ...(stores[0] as CatalogStore),
              source: "onepanel",
              live: true,
              installs: 72,
            }),
            stores[1] as CatalogStore,
          ],
        }),
      }),
    );
    const gallery = (await screen.findByRole("heading", { name: "App stores" })).closest("section") as HTMLElement;
    const added = within(gallery).getAllByRole("listitem")[0] as HTMLElement;
    expect(within(added).getByText("Added")).toBeVisible();
    expect(within(added).getByText("72 of 269 apps install")).toBeVisible();
    expect(within(added).queryByRole("button", { name: /^Add/ })).toBeNull();
    // One action to a tile: Add, or the way to the source that reads it.
    for (const tile of within(gallery).getAllByRole("listitem"))
      expect(within(tile).getAllByRole("button")).toHaveLength(1);
    await user.click(within(added).getByRole("button", { name: "Go to 1Panel" }));
    expect(scrolled).toHaveBeenCalled();
    expect(screen.getByRole("heading", { name: "1Panel" })).toHaveFocus();
  });

  it("offers the stores first in Add a source, one click each, with a link for anything else", async () => {
    const user = userEvent.setup();
    let asked: SetCatalogSourceRequest | undefined;
    renderAt(
      "/catalog/sources",
      api({
        setCatalogSource: (req) => {
          asked = req;
          return { change: planned(["catalog_source_reads", ["notes-src"]]) };
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a source" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a catalog source" });
    const pick = within(sheet).getByRole("group", { name: "App stores" });
    expect(within(pick).getByText("1Panel's app store")).toBeVisible();
    expect(within(sheet).getByText("Or any public Git repository")).toBeVisible();
    expect(within(sheet).getByLabelText("Repository")).toHaveValue("");
    await user.click(within(pick).getByRole("button", { name: "Add Dokploy's templates" }));
    await waitFor(() =>
      expect(asked).toMatchObject({ name: "dokploy", follow: "canary", format: CatalogFormat.DOKPLOY }),
    );
  });

  it("names a store's own link by the store, read as that store", async () => {
    const user = userEvent.setup();
    let asked: SetCatalogSourceRequest | undefined;
    const DEV = "d".repeat(40);
    renderAt(
      "/catalog/sources",
      api({
        listCatalogSources: () => ({ sources: [] }),
        listCatalogSourceRefs: () => ({
          refs: [
            { name: "refs/heads/main", commit: COMMIT },
            { name: "refs/heads/dev", commit: DEV },
          ],
          head: "refs/heads/dev",
          formats: [{ format: CatalogFormat.ONEPANEL, path: "", apps: 269 }],
        }),
        setCatalogSource: (req) => {
          asked = req;
          return { change: planned(["catalog_source_reads", ["notes-src"]]) };
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add another source" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a catalog source" });
    await user.type(within(sheet).getByLabelText("Repository"), ONEPANEL);
    expect(await within(sheet).findByText("269 apps")).toBeVisible();
    expect(within(sheet).getByText(/Named onepanel/)).toBeVisible();
    expect(within(sheet).getByLabelText("Branch")).toHaveValue("refs/heads/dev");
    expect(within(sheet).getByRole("switch", { name: "Follow dev" })).toBeChecked();
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked).toMatchObject({
      name: "onepanel",
      repository: ONEPANEL,
      commit: DEV,
      follow: "dev",
      format: CatalogFormat.ONEPANEL,
    });
  });

  it("asks which store a repository is only where it could be more than one", async () => {
    const user = userEvent.setup();
    let asked: SetCatalogSourceRequest | undefined;
    renderAt(
      "/catalog/sources",
      api({
        listCatalogSourceRefs: () => ({
          refs: [{ name: "refs/heads/main", commit: COMMIT }],
          head: "refs/heads/main",
          formats: [
            { format: CatalogFormat.DOKPLOY, path: "", apps: 2 },
            { format: CatalogFormat.ONEPANEL, path: "", apps: 1 },
          ],
        }),
        setCatalogSource: (req) => {
          asked = req;
          return { change: planned(["catalog_source_reads", ["notes-src"]]) };
        },
      }),
    );
    await user.click(await screen.findByRole("button", { name: "Add a source" }));
    const sheet = await screen.findByRole("dialog", { name: "Add a catalog source" });
    await user.type(within(sheet).getByLabelText("Repository"), "https://github.com/example/stores");
    const choice = await within(sheet).findByRole("radiogroup", { name: "Read it as" });
    expect(within(choice).getByRole("radio", { name: "Dokploy's templates" })).toBeChecked();
    await user.click(within(choice).getByRole("radio", { name: "1Panel's app store" }));
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toBeDefined());
    expect(asked?.format).toBe(CatalogFormat.ONEPANEL);
  });

  it("says a store's licence, None stated where its repository has none, and Not recognised where Dockwell can't name it", async () => {
    const unlicensed = create(CatalogSourceInfoSchema, {
      name: "apps",
      source: { repository: "https://github.com/example/apps", commit: COMMIT, format: CatalogFormat.UMBREL },
      entries: ["notes-src"],
      license: "NONE",
    });
    const unknown = create(CatalogSourceInfoSchema, {
      name: "more",
      source: { repository: "https://github.com/example/more", commit: COMMIT, format: CatalogFormat.CASAOS },
      entries: ["board-src"],
      license: "NOASSERTION",
    });
    const licensed = create(CatalogSourceInfoSchema, { ...store, license: "GPL-3.0-only" });
    renderAt("/catalog/sources", api({ listCatalogSources: () => ({ sources: [licensed, unlicensed, unknown] }) }));
    const none = (await screen.findByRole("heading", { name: "example/apps" })).closest("section") as HTMLElement;
    expect(within(none).getByText("None stated")).toBeVisible();
    expect(within(none).queryByText("NONE")).toBeNull();
    const other = screen.getByRole("heading", { name: "example/more" }).closest("section") as HTMLElement;
    expect(within(other).getByText("Not recognised")).toBeVisible();
    expect(within(other).queryByText("NOASSERTION")).toBeNull();
    const gpl = screen.getByRole("heading", { name: "1Panel" }).closest("section") as HTMLElement;
    expect(within(gpl).getByText("GPL-3.0-only")).toBeVisible();
  });

  it("says which store a source is, how many apps it brings with a few icons, and what it leaves out by why, the most first", async () => {
    const user = userEvent.setup();
    const iconed = create(EntrySchema, {
      ...notes,
      icon: `/catalog-media/0123456789abcdef/${COMMIT}/notes-src.svg`,
    });
    renderAt(
      "/catalog/sources",
      api({
        listCatalogSources: () => ({ sources: [store] }),
        listCatalogEntries: () => ({ entries: [iconed, postgres, wiki] }),
      }),
    );
    const panel = (await screen.findByRole("heading", { name: "1Panel" })).closest("section") as HTMLElement;
    expect(panel.querySelector(".store-mark img")).toHaveAttribute("src", "/stores/onepanel.png");
    expect(within(panel).getByText("2 apps from here")).toBeVisible();
    expect(panel.querySelectorAll(".source-card-icons img")).toHaveLength(1);
    await user.click(within(panel).getByText("3 apps left out, by why"));
    const kinds = [...panel.querySelectorAll(".sources-left-kind > summary")].map((k) => k.textContent);
    expect(kinds).toEqual([
      "Run scripts on the serverInstall once Dockwell runs their setup steps itself2",
      "Run on amd64 aloneInstall once Dockwell offers them for amd64 servers1",
    ]);
    await user.click(within(panel).getByText("Run scripts on the server"));
    expect(within(panel).getByText("2fauth")).toBeVisible();
  });
});
