// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import {
  type CheckDatabaseSignInRequest,
  type ConnectDatabaseRequest,
  DatabaseEngine,
  DatabaseSchema,
  type EnvironmentVariable,
  EnvironmentVariableSchema,
  LinkPart,
  type ServiceInfo,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { anApp, aRevision, ENV, places, SERVER } from "../../test/apps";
import { webPlanned } from "../../test/changes";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { forgetDrafts } from "./AppEnvironment";
import { appNames, candidates, suggest, suggested } from "./connectDatabase";

const variable = (init: MessageInitShape<typeof EnvironmentVariableSchema>) => create(EnvironmentVariableSchema, init);
const uses = (name: string) => [{ serviceId: "svc_blog", service: "blog", name }];

/** A blog imported with the database and user it made by hand, as an import brings one. */
const variables = [
  variable({ id: "var_1", name: "BLOG_DB_HOST", value: "db", usedBy: uses("BLOG_DB_HOST") }),
  variable({ id: "sec_1", name: "BLOG_DB_PASSWORD", secret: true, usedBy: uses("BLOG_DB_PASSWORD") }),
  variable({ id: "var_2", name: "BLOG_DB_NAME", value: "blog", usedBy: uses("BLOG_DB_NAME") }),
  variable({ id: "var_3", name: "SMTP_HOST", value: "mail.example.com", usedBy: uses("SMTP_HOST") }),
  variable({
    id: "var_4",
    name: "REGION",
    value: "eu",
    usedBy: [{ serviceId: "svc_api", service: "api", name: "REGION" }],
  }),
];

function aDatabase(init: MessageInitShape<typeof DatabaseSchema> = {}) {
  return create(DatabaseSchema, {
    id: "svc_db",
    resource: { kind: "service", scope: ENV, name: "db" },
    engine: DatabaseEngine.POSTGRESQL,
    image: "postgres:17",
    version: "17",
    port: 5432,
    host: "db",
    readings: [
      {
        serverId: SERVER,
        serverName: "hz-1",
        version: "17.2",
        databases: [
          { name: "postgres", owner: "postgres", system: true },
          { name: "blog", owner: "blog" },
          // Kept for another app: Dockwell made shop, which owns it.
          { name: "shop", owner: "shop" },
        ],
        users: [
          { name: "postgres", superuser: true, canSignIn: true },
          { name: "blog", canSignIn: true },
          { name: "shop", canSignIn: true, secret: "SHOP_DATABASE_PASSWORD", resourceId: "dbu_shop" },
          { name: "wide", canSignIn: true },
        ],
      },
    ],
    ...init,
  } as MessageInitShape<typeof DatabaseSchema>);
}

function api(
  service: ServiceInfo,
  asked: ConnectDatabaseRequest[],
  refuse?: ConnectError,
  signsIn: (req: CheckDatabaseSignInRequest) => boolean = () => true,
  reaches: (req: CheckDatabaseSignInRequest) => boolean = (req) => req.user === "wide",
  read: readonly EnvironmentVariable[] = variables,
): Api {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: SERVER, name: "hz-1" })), snapshotComplete);
  return {
    resources: places(),
    services: {
      listServices: () => ({ services: [service] }),
      listServiceRevisions: () => ({ revisions: [aRevision(3n, "ghcr.io/acme/blog:1")] }),
    },
    servers: { watchServers: watch.stream },
    variables: { listVariables: () => ({ environmentId: ENV, variables: [...read] }) },
    databases: {
      listDatabases: () => ({
        databases: [
          aDatabase(),
          // A cache it can't connect to, and a PostgreSQL on another server.
          aDatabase({
            id: "svc_cache",
            resource: { kind: "service", scope: ENV, name: "cache" },
            engine: DatabaseEngine.REDIS,
          }),
          aDatabase({
            id: "svc_far",
            resource: { kind: "service", scope: ENV, name: "far" },
            readings: [{ serverId: "srv_2", serverName: "hz-2" }],
          }),
        ],
      }),
      checkDatabaseSignIn: (req: CheckDatabaseSignInRequest) => ({
        signsIn: signsIn(req),
        administrator: reaches(req),
        said: signsIn(req) ? "" : 'FATAL: password authentication failed for user "blog"',
      }),
      connectDatabase: (req: ConnectDatabaseRequest) => {
        asked.push(req);
        if (refuse) throw refuse;
        return { change: webPlanned() };
      },
    },
    changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: () => ({}) },
  };
}

afterEach(() => forgetDrafts());

const open = async (user: ReturnType<typeof userEvent.setup>) => {
  await user.click(await screen.findByRole("button", { name: "Connect a database" }, { timeout: 5000 }));
  return screen.findByRole("dialog", { name: "Connect blog to a database" });
};

describe("connecting an app to a database", () => {
  it("takes on the database the app has, with the names it reads suggested, as one plan", async () => {
    const user = userEvent.setup();
    const asked: ConnectDatabaseRequest[] = [];
    const { container } = renderAt("/apps/svc_blog/environment", api(anApp("blog", "ghcr.io/acme/blog:1"), asked));
    const sheet = await open(user);
    const server = within(sheet).getByRole("combobox", { name: "Database server" });
    expect(
      within(server)
        .getAllByRole("option")
        .map((o) => o.textContent),
    ).toEqual(["db · PostgreSQL 17.2"]);
    const database = within(sheet).getByRole("combobox", { name: "Database" });
    expect(database).toHaveValue("");
    await waitFor(() =>
      expect(within(sheet).getByRole("combobox", { name: "BLOG_DB_HOST" })).toHaveValue(String(LinkPart.HOST)),
    );
    expect(within(sheet).getByRole("combobox", { name: "BLOG_DB_PASSWORD" })).toHaveValue(String(LinkPart.PASSWORD));
    expect(within(sheet).getByRole("combobox", { name: "BLOG_DB_NAME" })).toHaveValue(String(LinkPart.DATABASE));
    // A mail server's host is no database's, and a name another app reads isn't offered.
    expect(within(sheet).getByRole("combobox", { name: "SMTP_HOST" })).toHaveValue(String(LinkPart.UNSPECIFIED));
    expect(within(sheet).queryByRole("combobox", { name: "REGION" })).toBeNull();
    // A variable can't hold the password.
    const host = within(sheet).getByRole("combobox", { name: "BLOG_DB_HOST" });
    expect(within(host).queryByRole("option", { name: "Password" })).toBeNull();

    await user.selectOptions(database, "blog");
    expect(within(sheet).getByRole("combobox", { name: "Signs in as" })).toHaveValue("blog");
    expect(
      within(within(sheet).getByRole("combobox", { name: "Signs in as" })).queryByRole("option", { name: "postgres" }),
    ).toBeNull();
    expect(within(sheet).getByRole("combobox", { name: "Its password is in" })).toHaveValue("BLOG_DB_PASSWORD");
    // Signing in is checked as it's chosen.
    expect(await within(sheet).findByText("Signs in")).toBeVisible();
    await expectAccessible(container);
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toHaveLength(1));
    expect(asked[0]).toMatchObject({
      id: "svc_db",
      app: "blog",
      appProject: "prj_1",
      appEnvironment: ENV,
      database: "blog",
      user: "blog",
      passwordSecret: "BLOG_DB_PASSWORD",
      sets: { BLOG_DB_HOST: LinkPart.HOST, BLOG_DB_PASSWORD: LinkPart.PASSWORD, BLOG_DB_NAME: LinkPart.DATABASE },
    });
  });

  it("asks for a new database by default, and says a refusal beside what it's about", async () => {
    const user = userEvent.setup();
    const asked: ConnectDatabaseRequest[] = [];
    const refuse = new ConnectError(
      "BLOG_DB_HOST is a variable, and what it would hold is secret. Keep it as a secret first",
      Code.InvalidArgument,
    );
    refuse.metadata.set("Dockwell-Error", "invalid_sets");
    renderAt("/apps/svc_blog/environment", api(anApp("blog", "ghcr.io/acme/blog:1"), asked, refuse));
    const sheet = await open(user);
    await waitFor(() =>
      expect(within(sheet).getByRole("combobox", { name: "BLOG_DB_HOST" })).toHaveValue(String(LinkPart.HOST)),
    );
    expect(within(sheet).queryByRole("combobox", { name: "Signs in as" })).toBeNull();
    await user.click(within(sheet).getByRole("button", { name: /Review plan/ }));
    await waitFor(() => expect(asked).toHaveLength(1));
    expect(asked[0]).toMatchObject({ database: "", user: "", passwordSecret: "" });
    const said = await within(sheet).findByText(/BLOG_DB_HOST is a variable/);
    expect(said).toHaveAttribute("id", "app-connect-sets-error");
  });

  it("takes an app that reads nothing yet to the tab's own form for its first variable", async () => {
    const user = userEvent.setup();
    const asked: ConnectDatabaseRequest[] = [];
    // Only the api reads a variable here, so the blog reads none of its own.
    const others = variables.filter((v) => v.name === "REGION");
    renderAt(
      "/apps/svc_blog/environment",
      api(anApp("blog", "ghcr.io/acme/blog:1"), asked, undefined, undefined, undefined, others),
    );
    const sheet = await open(user);
    await within(sheet).findByText(/reads no variable or secret of its environment/);
    await user.click(within(sheet).getByRole("button", { name: "Add a variable" }));
    await waitFor(() => expect(screen.queryByRole("dialog", { name: "Connect blog to a database" })).toBeNull());
    expect(screen.getByRole("textbox", { name: "Key" })).toHaveFocus();
    expect(asked).toHaveLength(0);
  });
});

describe("what connecting suggests", () => {
  it("reads a name's part from what it says of a database, and nothing from what doesn't", () => {
    expect(suggest("DATABASE_URL")).toBe(LinkPart.URL);
    expect(suggest("POSTGRES_PASSWORD")).toBe(LinkPart.PASSWORD);
    expect(suggest("MYSQL_DATABASE")).toBe(LinkPart.DATABASE);
    expect(suggest("DB_PORT")).toBe(LinkPart.PORT);
    expect(suggest("DB_USERNAME")).toBe(LinkPart.USER);
    expect(suggest("SMTP_HOST")).toBe(LinkPart.UNSPECIFIED);
    expect(suggest("REDIS_URL")).toBe(LinkPart.UNSPECIFIED);
  });

  it("keeps one password, and none in a variable", () => {
    const names = [
      { name: "DB_PASSWORD", secret: false },
      { name: "PG_PASSWORD", secret: true },
      { name: "MYSQL_PASSWORD", secret: true },
    ];
    expect([...suggested(names)]).toEqual([["PG_PASSWORD", LinkPart.PASSWORD]]);
  });

  it("offers only the servers on the app's own server, of an engine it connects to", () => {
    const app = anApp("blog", "ghcr.io/acme/blog:1");
    const list = candidates(
      [
        aDatabase(),
        aDatabase({ id: "svc_r", engine: DatabaseEngine.REDIS }),
        aDatabase({ id: "svc_x", readings: [{ serverId: "srv_2" }] }),
      ],
      app,
    );
    expect(list.map((d) => d.id)).toEqual(["svc_db"]);
    expect(appNames(app, variables).map((n) => n.name)).toEqual([
      "BLOG_DB_HOST",
      "BLOG_DB_NAME",
      "BLOG_DB_PASSWORD",
      "SMTP_HOST",
    ]);
  });

  it("says a password that doesn't sign in as it's chosen, and holds the plan back", async () => {
    const user = userEvent.setup();
    const asked: ConnectDatabaseRequest[] = [];
    renderAt(
      "/apps/svc_blog/environment",
      api(anApp("blog", "ghcr.io/acme/blog:1"), asked, undefined, () => false),
    );
    const sheet = await open(user);
    await user.selectOptions(within(sheet).getByRole("combobox", { name: "Database" }), "blog");
    expect(
      await within(sheet).findByText(
        "blog doesn't sign in with BLOG_DB_PASSWORD, so blog couldn't reach its data. Pick the secret that holds its password.",
      ),
    ).toBeVisible();
    expect(within(sheet).getByRole("button", { name: /Review plan/ })).toBeDisabled();
    expect(asked).toHaveLength(0);
  });

  it("leaves out what's kept for another app, and offers a new database for a user that reaches past its own", async () => {
    const user = userEvent.setup();
    const asked: ConnectDatabaseRequest[] = [];
    renderAt("/apps/svc_blog/environment", api(anApp("blog", "ghcr.io/acme/blog:1"), asked));
    const sheet = await open(user);
    const database = within(sheet).getByRole("combobox", { name: "Database" });
    expect(within(database).queryByRole("option", { name: "shop" })).toBeNull();
    await user.selectOptions(database, "blog");
    const signsInAs = within(sheet).getByRole("combobox", { name: "Signs in as" });
    expect(within(signsInAs).queryByRole("option", { name: "shop" })).toBeNull();
    await user.selectOptions(signsInAs, "wide");
    expect(await within(sheet).findByText(/wide reaches past blog/)).toBeVisible();
    expect(within(sheet).getByRole("button", { name: /Review plan/ })).toBeDisabled();
    await user.click(within(sheet).getByRole("button", { name: "Use a new database" }));
    expect(database).toHaveValue("");
    expect(within(sheet).queryByRole("combobox", { name: "Signs in as" })).toBeNull();
  });
});
