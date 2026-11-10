// SPDX-License-Identifier: AGPL-3.0-only

import { create } from "@bufbuild/protobuf";
import {
  ChangeSchema,
  type ChangeServiceRuntimeRequest,
  ChangeState,
  FileType,
  HealthcheckSchema,
  HealthState,
  PlanAction,
  type ProposeChangeRequest,
  RestartPolicy,
  ServiceSchema,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { aHealth, anApp, aRevision, ENV, places, SERVER } from "../../test/apps";
import { type Api, feed, onPhone, onScreen, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { forgetDrafts } from "./AppEnvironment";
import { versionIn } from "./FolderFields";

const hz1 = aServer({ id: SERVER, name: "hz-1" });

/** What ListWebsiteRuntimes lists: PHP for websites, then each language an app from a folder runs on. */
const runtimes = [
  {
    id: "php",
    language: "PHP",
    version: "8.3",
    release: "8.3.36",
    image: "wordpress@sha256:83",
    extensions: ["mysqli"],
  },
  ...[
    [
      "node",
      "Node.js",
      "26",
      "26.12.0",
      "npm start",
      "if [ -f package-lock.json ]; then npm ci; else npm install; fi",
      3000,
    ],
    [
      "node",
      "Node.js",
      "24",
      "24.22.0",
      "npm start",
      "if [ -f package-lock.json ]; then npm ci; else npm install; fi",
      3000,
    ],
    ["python", "Python", "3.14", "3.14.9", "python app.py", "python -m venv .venv", 8000],
    ["python", "Python", "3.13", "3.13.17", "python app.py", "python -m venv .venv", 8000],
    ["go", "Go", "1.27", "1.27.2", "/tmp/app", "go build -o /tmp/app .", 8080],
    ["go", "Go", "1.26", "1.26.9", "/tmp/app", "go build -o /tmp/app .", 8080],
    ["java", "Java", "25", "25.0.4", "java -jar app.jar", "", 8080],
  ].map(([id, language, version, release, start, install, port]) => ({
    id: id as string,
    language: language as string,
    version: version as string,
    release: release as string,
    image: `${id}:${release}@sha256:${version}`,
    start: start as string,
    install: install as string,
    port: port as number,
    env: id === "node" ? ["HOME", "NPM_CONFIG_CACHE", "NPM_CONFIG_UPDATE_NOTIFIER", "PORT"] : ["HOME", "PORT"],
    extensions: [],
  })),
];

/** An app from /srv/shop on Node.js 24, on the build Dockwell offers unless image says. */
function shopApp(image = "node:24.22.0@sha256:24") {
  return anApp(
    "shop",
    image,
    {},
    {
      spec: create(ServiceSchema, {
        runtime: {
          language: "node",
          version: "24",
          folder: "/srv/shop",
          port: 3000,
          user: "1000:1000",
          wrote: { PORT: "3000", HOME: "/tmp", NPM_CONFIG_CACHE: "/tmp/.npm", NPM_CONFIG_UPDATE_NOTIFIER: "false" },
        },
        runs: {
          case: "app",
          value: {
            image,
            command: ["sh", "-c", "set -e\nnpm ci\nexec npm start"],
            workingDir: "/app",
            user: "1000:1000",
            // As ListServices has them: names, with their values hidden.
            env: { HOME: "", NODE_ENV: "", NPM_CONFIG_CACHE: "", NPM_CONFIG_UPDATE_NOTIFIER: "", PORT: "" },
            mounts: [{ source: { case: "hostPath", value: "/srv/shop" }, target: "/app", makeOwner: "1000:1000" }],
            healthcheck: { tcp: { port: 3000 }, startPeriod: "10m" },
          },
        },
      }),
    },
  );
}

const entry = (name: string) => ({ name, mode: "-rw-r--r--", type: FileType.FILE, linkType: FileType.UNSPECIFIED });

/** A disk with /srv, and the folders given in it, each owned by uid. */
function disk(folders: Record<string, { uid: number; entries: string[]; mode?: string }>) {
  return (req: { path: string }) => {
    const folder = folders[req.path];
    if (folder) {
      return {
        path: req.path,
        directory: {
          mode: folder.mode ?? "drwxr-xr-x",
          type: FileType.DIRECTORY,
          owner: "",
          uid: folder.uid,
          gid: folder.uid,
        },
        entries: folder.entries.map(entry),
      };
    }
    if (req.path === "/srv") {
      return {
        path: "/srv",
        directory: { mode: "drwxr-xr-x", type: FileType.DIRECTORY, owner: "mira", uid: 1000, gid: 1000 },
        entries: Object.keys(folders).map((path) => ({
          name: path.slice(path.lastIndexOf("/") + 1),
          mode: "drwxr-xr-x",
          type: FileType.DIRECTORY,
          linkType: FileType.UNSPECIFIED,
        })),
      };
    }
    throw new Error(`nothing at ${req.path}`);
  };
}

function api(more: Api = {}, services = [shopApp()], fleet = [hz1]): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(...fleet.map(changed), snapshotComplete);
  return {
    resources: places(),
    services: {
      listServices: () => ({ services }),
      listServiceRevisions: () => ({ revisions: [aRevision(3n, "node:24.22.0")] }),
      ...more.services,
    },
    servers: { watchServers: watch.stream },
    websites: { listWebsites: () => ({ websites: [] }), listWebsiteRuntimes: () => ({ runtimes }), ...more.websites },
    health: { listHealth: () => ({ services: services.map((s) => aHealth(s.id, { state: HealthState.HEALTHY })) }) },
    changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}), ...more.changes },
    ...(more.files ? { files: more.files } : {}),
  };
}

function planned(summary: string) {
  return create(ChangeSchema, {
    id: "chg_new",
    summary,
    state: ChangeState.PLANNED,
    plan: {
      fingerprint: "feedfacefeedfacefeedfacefeedface",
      summary: { create: 1 },
      resources: [{ id: "svc_api", resource: { kind: "service", name: "api" }, action: PlanAction.CREATE }],
    },
  });
}

afterEach(() => {
  forgetDrafts();
  vi.unstubAllGlobals();
});

describe("New app, from a folder", () => {
  it("asks for a name, and takes the folder, the language and its version from what's there", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    const { container } = renderAt(
      "/apps/new",
      api(
        {
          changes: {
            proposeChange: (req) => {
              proposals.push(req);
              return { change: planned(req.summary) };
            },
          },
          files: {
            listFiles: disk({ "/srv/api": { uid: 1001, entries: ["requirements.txt", "app.py", ".python-version"] } }),
            readFile: (req: { path: string }) => ({ path: req.path, content: "3.13.2\n" }),
          },
        },
        [],
      ),
    );
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await user.click(within(sheet).getByRole("radio", { name: "Folder" }));
    // The folder follows the name until it's typed.
    await user.type(within(sheet).getByLabelText("Name"), "api");
    expect(within(sheet).getByLabelText("Folder")).toHaveValue("/srv/api");
    // The server says whose the folder is, and it runs as them.
    await waitFor(() => expect(within(sheet).getByLabelText(/^Runs as/)).toHaveValue("1001:1001"), { timeout: 3_000 });
    expect(sheet).toHaveTextContent("Holds requirements.txt and 2 more.");
    // Its files say what it runs on, so nobody is asked: the pickers are one click away.
    await waitFor(() => expect(sheet).toHaveTextContent("RuntimePython 3.13 · from requirements.txt, .python-version"));
    expect(within(sheet).queryByRole("radio", { name: "Python" })).toBeNull();
    // What the language does unless you say is under Advanced, and shows on its line.
    expect(sheet).toHaveTextContent("Advancedpython app.py · port 8000 · as 1001:1001");
    expect(within(sheet).getByLabelText(/^Starts with/)).toHaveAttribute("placeholder", "python app.py");
    expect(within(sheet).getByLabelText(/^Port(?!s)/)).toHaveAttribute("placeholder", "8000");
    expect(sheet).toHaveTextContent("You approve what it reaches in the plan");
    await expectAccessible(container);

    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    await screen.findByRole("dialog", { name: "Review plan" });
    const [proposal] = proposals;
    expect(proposal?.summary).toBe("Run api from /srv/api");
    const edit = proposal?.edits[0];
    expect(edit?.resource).toMatchObject({ kind: "service", name: "api" });
    const spec = edit?.spec?.kind.case === "service" ? edit.spec.kind.value : undefined;
    expect(spec?.runtime).toMatchObject({
      language: "python",
      version: "3.13",
      folder: "/srv/api",
      install: "",
      start: "",
      port: 0,
      user: "1001:1001",
    });
    // The image is the server's to write, from the runtime.
    expect(spec?.runs.case === "app" ? spec.runs.value.image : "none").toBe("");
  });

  it("leaves the language to whoever picks one, whatever the folder holds", async () => {
    const user = userEvent.setup();
    renderAt(
      "/apps/new?source=folder",
      api({ files: { listFiles: disk({ "/srv/api": { uid: 1000, entries: ["package.json"] } }) } }, []),
    );
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    await user.type(within(sheet).getByLabelText("Name"), "api");
    await waitFor(() => expect(sheet).toHaveTextContent("RuntimeNode.js 26 · from package.json"), { timeout: 3_000 });
    await user.click(within(sheet).getByRole("button", { name: "Change" }));
    await user.click(within(sheet).getByRole("radio", { name: "Go" }));
    expect(within(sheet).getByRole("radio", { name: "Go" })).toBeChecked();
    // A folder typed after the name stays as typed, and the name stays too.
    const folder = within(sheet).getByLabelText("Folder");
    await user.clear(folder);
    await user.type(folder, "/srv/other");
    await user.type(within(sheet).getByLabelText("Name"), "2");
    expect(folder).toHaveValue("/srv/other");
    expect(within(sheet).getByRole("radio", { name: "Go" })).toBeChecked();
  });

  it("reads a version file as Dockwell names its versions", () => {
    expect(versionIn("node", ".nvmrc", "v24.3.0\n")).toBe("24");
    expect(versionIn("python", ".python-version", "3.13.2")).toBe("3.13");
    expect(versionIn("go", "go.mod", "module example.com/shop\n\ngo 1.27.1\n")).toBe("1.27");
    expect(versionIn("java", ".java-version", "21")).toBe("21");
    expect(versionIn("node", ".nvmrc", "lts/iron")).toBe("");
  });

  it("says what's wrong with a folder before the plan", async () => {
    const user = userEvent.setup();
    renderAt(
      "/apps/new?source=folder",
      api(
        {
          files: {
            listFiles: disk({
              "/srv/root": { uid: 0, entries: ["package.json"] },
              "/srv/empty": { uid: 1000, entries: [] },
              "/srv/locked": { uid: 1000, entries: ["package.json"], mode: "dr-xr-xr-x" },
              "/srv/theirs": { uid: 1001, entries: ["package.json"] },
            }),
          },
        },
        [],
      ),
    );
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    const folder = within(sheet).getByLabelText("Folder");
    await user.type(folder, "/srv/root");
    await waitFor(() => expect(sheet).toHaveTextContent("/srv/root is root's, and an app never runs as root"), {
      timeout: 3_000,
    });
    await user.clear(folder);
    await user.type(folder, "/srv/empty");
    await waitFor(() => expect(sheet).toHaveTextContent("No Node.js app in /srv/empty yetIt has no package.json."), {
      timeout: 3_000,
    });
    // Its own, closed to itself: the mode is what to change.
    await user.clear(folder);
    await user.type(folder, "/srv/locked");
    await waitFor(
      () =>
        expect(sheet).toHaveTextContent(
          "The app can't write to /srv/lockedIts mode is dr-xr-xr-x. Run chmod u+w /srv/locked on the server.",
        ),
      { timeout: 3_000 },
    );
    await user.clear(folder);
    await user.type(folder, "/srv/new");
    await waitFor(() => expect(sheet).toHaveTextContent("New Dockwell makes it, owned by 1000:1000."), {
      timeout: 3_000,
    });
    // The folder it found before was mira's, and the field took her uid:gid.
    const runsAs = within(sheet).getByLabelText(/^Runs as/);
    expect(runsAs).toHaveValue("1000:1000");
    // Someone else's folder: one press runs the app as its owner.
    await user.clear(folder);
    await user.type(folder, "/srv/theirs");
    await user.click(await within(sheet).findByRole("button", { name: "Run as 1001:1001" }, { timeout: 3_000 }));
    expect(runsAs).toHaveValue("1001:1001");
    await waitFor(() => expect(sheet).toHaveTextContent("Holds package.json."));
    await user.clear(runsAs);
    await user.type(runsAs, "0:0");
    await user.click(within(sheet).getByRole("button", { name: "Review plan" }));
    expect(within(sheet).getByLabelText(/^Runs as/)).toHaveAccessibleDescription(
      "An app never runs as root. Use the folder owner's uid:gid, such as 1000:1000.",
    );
  });

  it("reads the folder on each server it runs on, and says what each has", async () => {
    const user = userEvent.setup();
    const there = disk({ "/srv/api": { uid: 1001, entries: ["package.json"] } });
    renderAt(
      "/apps/new?source=folder",
      api(
        {
          files: {
            listFiles: (req: { serverId: string; path: string }) => {
              if (req.serverId === SERVER) return there(req);
              // hz-2 has /srv, empty.
              if (req.path === "/srv") return { ...there(req), entries: [] };
              throw new Error("nothing there");
            },
          },
        },
        [],
        [hz1, aServer({ id: "srv_2", name: "hz-2" })],
      ),
    );
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    const runsOn = await within(sheet).findByRole("group", { name: "Runs on" });
    await user.click(within(runsOn).getByRole("checkbox", { name: "hz-1" }));
    await user.click(within(runsOn).getByRole("checkbox", { name: "hz-2" }));
    await user.type(within(sheet).getByLabelText("Folder"), "/srv/api");
    const on1 = await within(sheet).findByText("hz-1", { selector: ".app-folder-on span" });
    const on2 = within(sheet).getByText("hz-2", { selector: ".app-folder-on span" });
    await waitFor(() => expect(on1.parentElement).toHaveTextContent("Holds package.json."), { timeout: 3_000 });
    await waitFor(() => expect(on2.parentElement).toHaveTextContent("New Dockwell makes it"), { timeout: 3_000 });
    // The first server's owner is who it runs as.
    expect(within(sheet).getByLabelText(/^Runs as/)).toHaveValue("1001:1001");
  });

  it("opens on the language and version a runtime's card asks for", async () => {
    renderAt("/apps/new?source=folder&language=go&version=%221.27%22", api({}, []));
    const sheet = await screen.findByRole("dialog", { name: "New app" });
    expect(within(sheet).getByRole("radio", { name: "Folder" })).toBeChecked();
    expect(within(sheet).getByRole("radio", { name: "Go" })).toBeChecked();
    expect(within(sheet).getByLabelText("Version")).toHaveValue("1.27");
    expect(within(sheet).getByLabelText("Name")).toHaveFocus();
  });
});

describe("an app from a folder", () => {
  it("says what it runs on, and changes version through a plan", async () => {
    const user = userEvent.setup();
    const asked: ChangeServiceRuntimeRequest[] = [];
    const { container } = renderAt(
      "/apps/svc_shop",
      api({
        services: {
          changeServiceRuntime: (req) => {
            asked.push(req);
            return { change: planned(req.summary) };
          },
        },
      }),
    );
    const head = await screen.findByRole("heading", { name: "shop" });
    const title = head.closest(".page") as HTMLElement;
    await waitFor(() => expect(title).toHaveTextContent("Node.js 24 from /srv/shop"));
    const config = await screen.findByRole("region", { name: "Configuration" });
    expect(config).toHaveTextContent("RuntimeNode.js 24 · 24.22.0");
    expect(config).toHaveTextContent("Folder/srv/shop");
    expect(config).toHaveTextContent("Starts withnpm start");
    expect(config).toHaveTextContent("Port3000");
    expect(config).toHaveTextContent("Each start installs again");
    // What the runtime writes isn't said as if someone had set it.
    expect(config).not.toHaveTextContent("set -e");
    await expectAccessible(container);

    await user.click(screen.getAllByRole("button", { name: "Change version" })[0] as HTMLElement);
    const sheet = await screen.findByRole("dialog", { name: "Change shop's version" });
    // The version it has, on its newest build, has nothing to plan.
    expect(within(sheet).getByRole("button", { name: /Review plan/ })).toBeDisabled();
    await user.selectOptions(within(sheet).getByLabelText("Node.js version"), "26");
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(asked[0]).toMatchObject({ id: "svc_shop", version: "26", summary: "Run shop on Node.js 26, from 24" });
  });

  it("offers the newest build of its version when it runs an older one", async () => {
    const user = userEvent.setup();
    const asked: ChangeServiceRuntimeRequest[] = [];
    renderAt(
      "/apps/svc_shop",
      api(
        {
          services: {
            changeServiceRuntime: (req) => {
              asked.push(req);
              return { change: planned(req.summary) };
            },
          },
        },
        [shopApp("node:24.21.0@sha256:old")],
      ),
    );
    const config = await screen.findByRole("region", { name: "Configuration" });
    await waitFor(() => expect(config).toHaveTextContent("An older build of 24"));
    await user.click(within(config).getByRole("button", { name: "Update to 24.22.0" }));
    const sheet = await screen.findByRole("dialog", { name: "Change shop's version" });
    expect(sheet).toHaveTextContent("It runs an older build of 24. The plan updates it to 24.22.0.");
    // The release listed isn't the one it runs, so its option says neither.
    expect(within(sheet).getByRole("option", { name: "24, the one it runs" })).toBeInTheDocument();
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await screen.findByRole("dialog", { name: "Review plan" });
    expect(asked[0]).toMatchObject({ id: "svc_shop", version: "24", summary: "Update shop to Node.js 24.22.0" });
  });

  it("says what its runtime gives it apart from what's its own", async () => {
    const page = renderAt("/apps/svc_shop", api());
    const config = await screen.findByRole("region", { name: "Configuration" });
    await waitFor(() => expect(config).toHaveTextContent("Environment1 variable"));
    page.unmount();
    renderAt("/apps/svc_shop/environment", api());
    const port = await screen.findByRole("row", { name: /^PORT/ });
    await waitFor(() => expect(port).toHaveTextContent("PORT3000Set by its runtime"));
    expect(screen.getByRole("row", { name: /^NODE_ENV/ })).toHaveTextContent("Set on this app");
    // What the runtime wrote isn't secret, so its value shows.
    expect(screen.getByRole("row", { name: /^NPM_CONFIG_CACHE/ })).toHaveTextContent(
      "NPM_CONFIG_CACHE/tmp/.npmSet by its runtime",
    );
  });

  it("overrides a runtime default for this app, keeping its hidden values and runtime", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    const service = shopApp();
    service.revision = 7n;
    renderAt("/apps/svc_shop/environment", {
      ...api(
        {
          changes: {
            proposeChange: (req) => {
              proposals.push(req);
              return { change: planned(req.summary) };
            },
          },
        },
        [service],
      ),
      // A shared variable with the same name belongs to other apps, not this one.
      variables: {
        listVariables: () => ({
          variables: [
            {
              id: "var_cache",
              name: "NPM_CONFIG_CACHE",
              value: "/shared/cache",
              usedBy: [{ serviceId: "svc_other", service: "other", name: "NPM_CONFIG_CACHE" }],
            },
          ],
        }),
      },
    });
    await user.click(await screen.findByRole("button", { name: "Change NPM_CONFIG_CACHE" }));
    const form = screen.getByRole("form", { name: "Change NPM_CONFIG_CACHE" });
    expect(form).toHaveTextContent("This app uses the new value instead of its runtime's default.");
    await user.type(within(form).getByRole("textbox", { name: "New value of NPM_CONFIG_CACHE" }), "/app/.cache");
    await user.click(within(form).getByRole("button", { name: "Change" }));
    const row = screen.getByRole("row", { name: /^NPM_CONFIG_CACHE/ });
    expect(row).toHaveTextContent("Set on this app");
    expect(within(row).getByRole("img", { name: "Hidden" })).toBeVisible();
    const bar = screen.getByRole("region", { name: "Unapplied changes" });
    expect(bar).toHaveTextContent("Applying it restarts shop.");
    await user.click(within(bar).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    const edits = proposals[0]?.edits ?? [];
    expect(edits.map((edit) => edit.resource?.kind)).toEqual(["service"]);
    const edit = edits[0];
    const spec = edit?.spec?.kind.case === "service" ? edit.spec.kind.value : undefined;
    expect(spec?.runs.case === "app" ? spec.runs.value.env : undefined).toEqual({ NPM_CONFIG_CACHE: "/app/.cache" });
    expect(spec?.runtime).toEqual(service.spec?.runtime);
    expect(spec?.runs.case === "app" ? spec.runs.value.image : undefined).toBe("node:24.22.0@sha256:24");
    expect(edit?.keepEnv).toEqual(["HOME", "NODE_ENV", "NPM_CONFIG_UPDATE_NOTIFIER", "PORT"]);
    expect(edit?.baseRevision).toBe(7n);
  });

  it("keeps PORT with its runtime, and refuses to import it as an app override", async () => {
    const user = userEvent.setup();
    renderAt("/apps/svc_shop/environment", {
      ...api(),
      variables: { listVariables: () => ({ variables: [] }) },
    });
    const port = await screen.findByRole("row", { name: /^PORT/ });
    expect(within(port).queryByRole("button", { name: "Change PORT" })).toBeNull();
    expect(within(port).queryByRole("button", { name: "Remove PORT" })).toBeNull();
    const cache = screen.getByRole("row", { name: /^NPM_CONFIG_CACHE/ });
    expect(within(cache).queryByRole("button", { name: "Remove NPM_CONFIG_CACHE" })).toBeNull();
    await waitFor(() => expect(screen.getByRole("button", { name: "Import .env" })).toBeEnabled());
    await user.click(screen.getByRole("button", { name: "Import .env" }));
    const sheet = await screen.findByRole("dialog", { name: "Import .env" });
    await user.type(within(sheet).getByRole("textbox", { name: "Paste the file's text" }), "PORT=4000");
    await user.click(within(sheet).getByRole("button", { name: "Import 1 value" }));
    expect(within(sheet).getByRole("alert")).toHaveTextContent(
      "PORT is set by its runtime, so it can't be imported. Remove PORT from this file.",
    );
    expect(screen.queryByRole("region", { name: "Unapplied changes" })).toBeNull();
  });

  it("keeps runtime-owned settings read-only while planning an app's restart policy", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    const service = shopApp();
    const page = renderAt(
      "/apps/svc_shop/settings",
      api(
        {
          changes: {
            proposeChange: (req) => {
              proposals.push(req);
              return { change: planned(req.summary) };
            },
          },
        },
        [service],
      ),
    );
    await user.click(await screen.findByRole("button", { name: "Edit General" }));
    const form = screen.getByRole("form", { name: "Edit General" });
    for (const field of ["Command", "Entrypoint", "User", "Directory"]) {
      expect(within(form).getByRole("textbox", { name: field })).toBeDisabled();
    }
    expect(within(form).getByRole("switch", { name: "Run no entrypoint, not even the image's" })).toBeDisabled();
    expect(form).toHaveTextContent("Its runtime sets the command, entrypoint, user and directory.");
    await user.selectOptions(
      within(form).getByRole("combobox", { name: "Restarts" }),
      String(RestartPolicy.ON_FAILURE),
    );
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    const edit = proposals[0]?.edits[0];
    const spec = edit?.spec?.kind.case === "service" ? edit.spec.kind.value : undefined;
    expect(spec?.runtime).toEqual(service.spec?.runtime);
    expect(spec?.runs.case === "app" ? spec.runs.value.restart : undefined).toBe(RestartPolicy.ON_FAILURE);
    expect(edit?.keepEnv).toEqual(["HOME", "NODE_ENV", "NPM_CONFIG_CACHE", "NPM_CONFIG_UPDATE_NOTIFIER", "PORT"]);
    page.unmount();

    renderAt("/apps/svc_shop/settings", api());
    await user.click(await screen.findByRole("button", { name: "Edit Volumes and mounts" }));
    const mounts = screen.getByRole("form", { name: "Edit Volumes and mounts" });
    expect(within(mounts).getByRole("textbox", { name: "Mount 1: volume or server path" })).toBeDisabled();
    expect(within(mounts).getByRole("textbox", { name: "Mount 1: path in the app" })).toBeDisabled();
    expect(within(mounts).getByRole("checkbox", { name: "Read-only" })).toBeDisabled();
    expect(within(mounts).getByRole("button", { name: "Remove mount 1" })).toBeDisabled();
    expect(mounts).toHaveTextContent("Its runtime mounts this folder at /app.");
    await user.click(within(mounts).getByRole("button", { name: "Add a volume or mount" }));
    expect(within(mounts).getByRole("textbox", { name: "Mount 2: volume or server path" })).toBeEnabled();
  });

  it("uses its runtime's TCP check or plans an app's own HTTP check", async () => {
    const user = userEvent.setup();
    const proposals: ProposeChangeRequest[] = [];
    const withPlans = (service = shopApp()) =>
      api(
        {
          changes: {
            proposeChange: (req) => {
              proposals.push(req);
              return { change: planned(req.summary) };
            },
          },
        },
        [service],
      );
    const page = renderAt("/apps/svc_shop/settings", withPlans());
    await user.click(await screen.findByRole("button", { name: "Edit Health check" }));
    let form = screen.getByRole("form", { name: "Edit Health check" });
    expect(form).toHaveTextContent("Its runtime checks its port by TCP, with 10 minutes to start.");
    expect(within(form).queryByRole("textbox", { name: "Port in the app" })).toBeNull();
    expect(within(form).queryByRole("radio", { name: "The image's" })).toBeNull();
    await user.click(within(form).getByRole("radio", { name: "HTTP" }));
    await user.type(within(form).getByRole("textbox", { name: "Path" }), "/health");
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(1));
    const http = proposals[0]?.edits[0]?.spec?.kind;
    expect(
      http?.case === "service" && http.value.runs.case === "app" ? http.value.runs.value.healthcheck?.http : undefined,
    ).toMatchObject({ port: 3000, path: "/health" });
    page.unmount();

    const service = shopApp();
    if (service.spec?.runs.case === "app") {
      service.spec.runs.value.healthcheck = create(HealthcheckSchema, { http: { port: 8080, path: "/health" } });
    }
    renderAt("/apps/svc_shop/settings", withPlans(service));
    await user.click(await screen.findByRole("button", { name: "Edit Health check" }));
    form = screen.getByRole("form", { name: "Edit Health check" });
    await user.click(within(form).getByRole("radio", { name: "TCP" }));
    await user.click(within(form).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(proposals).toHaveLength(2));
    const restored = proposals[1]?.edits[0]?.spec?.kind;
    expect(restored?.case === "service" ? restored.value.runtime : undefined).toEqual(service.spec?.runtime);
    expect(
      restored?.case === "service" && restored.value.runs.case === "app"
        ? restored.value.runs.value.healthcheck
        : "missing",
    ).toBeUndefined();
  });

  it("says when Dockwell no longer offers its version, with a way off it, and New app doesn't offer it", async () => {
    const user = userEvent.setup();
    const retired = runtimes.map((r) => (r.id === "node" && r.version === "24" ? { ...r, retired: true } : r));
    const base = api();
    const withRetired: Api = {
      ...base,
      websites: { ...base.websites, listWebsiteRuntimes: () => ({ runtimes: retired }) },
    };
    const page = renderAt("/apps/svc_shop", withRetired);
    const config = await screen.findByRole("region", { name: "Configuration" });
    await waitFor(() =>
      expect(config).toHaveTextContent("Dockwell no longer offers this version, so no new app runs on it"),
    );
    await user.click(within(config).getByRole("button", { name: "Change version" }));
    const sheet = await screen.findByRole("dialog", { name: "Change shop's version" });
    expect(within(sheet).queryByRole("option", { name: /^24/ })).toBeNull();
    page.unmount();
    renderAt("/apps/new?source=folder", withRetired);
    const form = await screen.findByRole("dialog", { name: "New app" });
    // 26 is all Node.js offers now, so there's no version to choose.
    expect(within(form).queryByRole("option", { name: /^24/ })).toBeNull();
    expect(within(form).queryByLabelText("Version")).toBeNull();
  });

  it("keeps the runtime under the app name beside its domain on a wide screen", async () => {
    onScreen("(min-width: 1100px)");
    renderAt("/apps", {
      ...api(),
      domains: {
        listDomains: () => ({
          domains: [{ name: "shop.example.com", environmentId: ENV, routes: [{ service: "shop", port: 3000 }] }],
        }),
      },
    });
    const row = await screen.findByRole("row", { name: /shop/ });
    expect(screen.getByRole("columnheader", { name: "Domain" })).toBeVisible();
    await waitFor(() => expect(within(row).getAllByRole("cell")[0]).toHaveTextContent("Node.js 24 · /srv/shop"));
    await waitFor(() => expect(row).toHaveTextContent("shop.example.com"));
  });

  it("lists as what it runs on and its folder, on a wide screen and a phone", async () => {
    renderAt("/apps", api());
    const row = await screen.findByRole("row", { name: /shop/ });
    await waitFor(() => expect(row).toHaveTextContent("Node.js 24 · /srv/shop"));
    onPhone();
    renderAt("/apps", api());
    await waitFor(() =>
      expect(document.querySelector(".apps-phone-image")).toHaveTextContent("Node.js 24 · /srv/shop"),
    );
  });
});
