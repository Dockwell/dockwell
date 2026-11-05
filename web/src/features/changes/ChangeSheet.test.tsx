// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { DurationSchema, type Timestamp, timestampFromDate } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type HandlerContext } from "@connectrpc/connect";
import {
  type ApplyChangeRequest,
  CertificateState,
  type Change,
  ChangeState,
  DomainCheckSchema,
  DomainCheckState,
  DroppedMountSchema,
  FieldChangeSchema,
  FieldOp,
  HostAccessKind,
  HostAccessSchema,
  IngressChange,
  IngressEffectSchema,
  IntentKind,
  IntentSchema,
  PlanAction,
  PlanNoteSchema,
  PlannedResourceSchema,
  PlanStepSchema,
  PlanSummarySchema,
  type PortMappingSchema,
  ResourceRefSchema,
  RolloutPlanSchema,
  RouteAnswersSchema,
  RouteGoesSchema,
  RouteMoveSchema,
  RouteReach,
  ServerPlanSchema,
  ServerPortCheckSchema,
  type ServiceInfoSchema,
  StepEffect,
  StepProgressSchema,
  StepState,
  VisitorsFind,
  type WatchChangeResponse,
  WatchChangeResponseSchema,
} from "@dockwell/api";
import { act, fireEvent, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { StrictMode, useEffect } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { activateLocale } from "../../app/i18n";
import { expectAccessible } from "../../test/a11y";
import { applied, moved, shopPlanned, watched, webPlanned } from "../../test/changes";
import { type Api, type CacheOptions, feed, onPhone, quietWatch, renderWithApi } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { ChangeSheetProvider, type Opening, useChangeSheet } from "./ChangeSheet";

function Opener({ opening }: { opening: Opening }) {
  const { open } = useChangeSheet();
  useEffect(() => open(opening), [open, opening]);
  return null;
}

function renderSheet(opening: Opening, api: Api, cache?: CacheOptions) {
  return renderWithApi(
    <ChangeSheetProvider>
      <Opener opening={opening} />
    </ChangeSheetProvider>,
    api,
    cache,
  );
}

const proposeWeb: Opening = {
  kind: "propose",
  summary: "Update web",
  edits: [{ resource: { name: "web" }, spec: { kind: { case: "container", value: { image: "nginx:1.28" } } } }],
};

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("the plan sheet", () => {
  it("reviews a plan, applies it, and follows its steps inline", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchChangeResponse>();
    const applies: ApplyChangeRequest[] = [];
    const discard = vi.fn(() => ({}));
    const planned = webPlanned();
    renderSheet(proposeWeb, {
      changes: {
        proposeChange: () => ({ change: planned }),
        watchChange: watch.stream,
        applyChange: (req) => {
          applies.push(req);
          return { change: moved(planned, { state: ChangeState.APPLYING }) };
        },
        discardChange: discard,
      },
    });

    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(sheet).toHaveAccessibleDescription("Update web");
    await within(sheet).findByText("to replace");
    expect(sheet).toHaveTextContent("1 to replace");
    // A replace counts on its own, and the secret says only that it changed.
    expect(sheet).not.toHaveTextContent(/\b0 to/);
    expect(within(sheet).getAllByText("hidden")).toHaveLength(1);
    expect(sheet).toHaveTextContent("hz-1 · about 2 s down");
    // With one server, there are no steps to tell apart, and nothing about how they run.
    expect(within(sheet).queryByRole("region", { name: /Steps/ })).toBeNull();
    expect(sheet).not.toHaveTextContent("one at a time");
    expect(sheet).toHaveTextContent("Undo for 7 days");
    await expectAccessible(document.body);

    act(() => watch.push(watched(planned)));
    await user.click(within(sheet).getByRole("button", { name: /Apply plan/ }));
    await waitFor(() => expect(applies).toHaveLength(1));
    expect(applies[0]?.fingerprint).toBe("a1b2c3d4e5f60718293a4b5c6d7e8f90");

    const running = moved(planned, {
      state: ChangeState.APPLYING,
      steps: [
        create(StepProgressSchema, {
          serverId: "srv_1",
          resourceId: "res_web",
          state: StepState.QUEUED,
          message: "Waiting to replace web on hz-1",
        }),
      ],
    });
    act(() => watch.push(watched(running)));
    const applying = await screen.findByRole("dialog", { name: "Applying" });
    expect(await within(applying).findByRole("region", { name: "Steps" })).toHaveTextContent("Replace web");
    act(() =>
      watch.push(
        create(WatchChangeResponseSchema, {
          event: {
            case: "step",
            value: {
              index: 0,
              step: {
                serverId: "srv_1",
                resourceId: "res_web",
                state: StepState.RUNNING,
                message: "Replacing web on hz-1",
              },
            },
          },
        }),
      ),
    );
    // In the tense of where it is: never "Replaced" while it runs.
    expect(await within(applying).findByText("Running")).toBeVisible();
    expect(within(applying).getByRole("region", { name: "Steps" })).toHaveTextContent("Replacing web");
    expect(applying).toHaveTextContent("It keeps applying if you close this.");

    act(() => watch.push(watched(applied(running))));
    const done = await screen.findByRole("dialog", { name: "web is deployed" });
    expect(done).toHaveTextContent("1 replaced");
    expect(done).not.toHaveTextContent("to replace");
    expect(done).not.toHaveTextContent("about 2 s down");
    expect(within(done).getByRole("button", { name: /Undo until/ })).toBeVisible();
    await expectAccessible(document.body);
    // An applied plan stays, sheet or no sheet.
    await user.click(within(done).getByRole("button", { name: /Done/ }));
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(discard).not.toHaveBeenCalled();
  });

  it("asks for the start of the fingerprint before applying a destructive plan", async () => {
    const user = userEvent.setup();
    const applies: ApplyChangeRequest[] = [];
    const destructive = webPlanned();
    if (destructive.plan) destructive.plan.destructive = true;
    renderSheet(proposeWeb, {
      changes: {
        proposeChange: () => ({ change: destructive }),
        watchChange: feed<WatchChangeResponse>().stream,
        applyChange: (req) => {
          applies.push(req);
          return { change: destructive };
        },
      },
    });

    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const apply = await within(sheet).findByRole("button", { name: /Apply plan/ });
    expect(within(sheet).getByRole("alert")).toHaveTextContent(
      "This plan destroys data, may cut off access, or lets two containers write to one place",
    );
    expect(apply).toBeDisabled();
    const field = within(sheet).getByLabelText("Type a1b2c3d4 to apply it");
    await user.type(field, "a1b2c3d5");
    expect(apply).toBeDisabled();
    await user.clear(field);
    await user.type(field, "A1B2C3D4");
    expect(apply).toBeEnabled();
    await expectAccessible(document.body);
    await user.click(apply);
    await waitFor(() => expect(applies[0]?.fingerprint).toBe("a1b2c3d4e5f60718293a4b5c6d7e8f90"));
  });

  it("quotes what a spec holds, so it can't write the plan's words", async () => {
    const spoof = "nothing, and it is safe";
    const reaching = webPlanned();
    const plan = reaching.plan;
    const resource = plan?.resources[0];
    if (!plan || !resource) throw new Error("webPlanned has no resource");
    plan.hostAccess = true;
    resource.hostAccess = [
      create(HostAccessSchema, { kind: HostAccessKind.SERVER_PATH, path: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.DOCKER_SOCKET, path: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.VOLUME_MOUNT, driver: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.VOLUME_MOUNT, type: "nfs", address: spoof, path: ":/x" }),
      create(HostAccessSchema, { kind: HostAccessKind.VOLUME_MOUNT, type: "nfs", address: "10.0.0.2", path: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.VOLUME_MOUNT, type: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.NETWORK_DRIVER, driver: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.NETWORK_DRIVER, driver: "macvlan", path: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.NETWORK_DRIVER, driver: "macvlan", detail: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.DOMAIN, path: spoof, detail: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.PANEL_DOMAIN, path: spoof, detail: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.EXTERNAL_NETWORK, path: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.EXTERNAL_NETWORK, path: "edge", detail: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.CAPABILITY, path: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.GROUP, path: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.SERVER_PORT, path: spoof, detail: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.TRUSTED_PROXY, path: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.PLAIN_FOR_ANYONE, path: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.HTTPS_OFF, path: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.SERVER_PATH, path: "/srv/a", leadsTo: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.SERVER_PATH, path: "/srv/b", leadsTo: spoof, sameAsRoot: true }),
      create(HostAccessSchema, { kind: HostAccessKind.DOCKER_SOCKET, path: "/srv/c", leadsTo: spoof }),
      create(HostAccessSchema, { kind: HostAccessKind.CAPABILITY, path: spoof, sameAsRoot: true }),
      create(HostAccessSchema, { kind: HostAccessKind.GROUP, path: spoof, sameAsRoot: true }),
    ];
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: reaching }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByRole("alert");
    const text = sheet.textContent ?? "";
    expect(text).toContain(`reaches "${spoof}" on hz-1`);
    expect(text).toContain(`joins the group "${spoof}" on hz-1`);
    expect(text).toContain(`has the Linux capability "${spoof}" on hz-1`);
    expect(text.replace(/"(?:[^"\\]|\\.)*"/g, "")).not.toContain(spoof);
  });

  it("asks for the fingerprint before giving a container more of its server, and says what it reaches", async () => {
    const user = userEvent.setup();
    const applies: ApplyChangeRequest[] = [];
    const reaching = webPlanned();
    const plan = reaching.plan;
    const step = plan?.servers[0]?.steps[0];
    if (!plan || !step) throw new Error("webPlanned has no step");
    plan.hostAccess = true;
    const resource = plan.resources[0];
    if (resource) {
      resource.hostAccess = [
        create(HostAccessSchema, {
          kind: HostAccessKind.VOLUME_MOUNT,
          type: "nfs",
          address: "10.0.0.2",
          path: ":/media",
        }),
      ];
    }
    step.healthUnknown = true;
    step.hidesLogs = true;
    step.hostAccess = [
      create(HostAccessSchema, { kind: HostAccessKind.SERVER_PATH, path: "/srv/app/config", readOnly: true }),
      create(HostAccessSchema, { kind: HostAccessKind.DOCKER_SOCKET, path: "/var/run/docker.sock" }),
      create(HostAccessSchema, { kind: HostAccessKind.EXTERNAL_NETWORK, path: "edge", detail: "db" }),
      create(HostAccessSchema, { kind: HostAccessKind.CAPABILITY, path: "SYS_ADMIN", sameAsRoot: true }),
      create(HostAccessSchema, { kind: HostAccessKind.CAPABILITY, path: "NET_RAW" }),
      create(HostAccessSchema, { kind: HostAccessKind.GROUP, path: "999" }),
      create(HostAccessSchema, { kind: HostAccessKind.PRIVILEGED, sameAsRoot: true }),
    ];
    plan.notes = [
      create(PlanNoteSchema, {
        reason: "no_build",
        message: "web has build beside image, and Dockwell builds nothing, so it runs nginx:1.27 as named",
      }),
      create(PlanNoteSchema, {
        reason: "exposed_variable",
        message: "REDIS_PASSWORD from your .env is in redis's command, so anyone who can read this change can read it",
        names: ["REDIS_PASSWORD", "command"],
      }),
    ];
    step.untilHealthy = { $typeName: "google.protobuf.Duration", seconds: 110n, nanos: 0 };
    if (plan.servers[0]) plan.servers[0].untilHealthy = step.untilHealthy;
    step.keptPaths = ["/var/lib/postgresql/data"];
    step.destroysData = true;
    renderSheet(proposeWeb, {
      changes: {
        proposeChange: () => ({ change: reaching }),
        watchChange: feed<WatchChangeResponse>().stream,
        applyChange: (req) => {
          applies.push(req);
          return { change: reaching };
        },
      },
    });

    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const risk = await within(sheet).findByRole("alert");
    // The worst of it leads, in the form's words, above everything else in the plan.
    expect(risk.querySelector(".dw-callout-title")).toHaveTextContent("web gets root on hz-1");
    expect(risk).toHaveTextContent("/var/run/docker.sock gives it control of Docker on the server, the same as root.");
    expect(risk).toHaveTextContent("Apply this only if you trust what it runs.");
    // What stops short of root has a row of its own, as a warning, and no alarm.
    expect(risk).not.toHaveTextContent("/srv/app/config");
    const effects = within(sheet).getByRole("region", { name: "What applying means" });
    expect(effects).toHaveTextContent("web reaches the nfs mount 10.0.0.2:/media (read-write) on hz-1");
    expect(effects).toHaveTextContent("web reaches /srv/app/config on hz-1");
    expect(effects).toHaveTextContent("It can read that path on the server.");
    expect(risk).toHaveTextContent(
      "web loses what its image keeps at /var/lib/postgresql/data. The server keeps that data for 7 days",
    );
    expect(
      risk.compareDocumentPosition(sheet.querySelector(".dw-plan") as Element) & Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(sheet).not.toHaveTextContent("about 2 s down, no downtime");
    expect(sheet).toHaveTextContent("web's kept logs can't be read from then on, and go in 7 days.");
    expect(sheet).toHaveTextContent("web may take longer to be healthy, as its image isn't on the server yet.");
    expect(sheet).toHaveTextContent("about 2 s down, and up to 1 min 50 s until healthy");
    // The confirmation sits in the footer beside Apply, with the reason Apply waits.
    const footer = sheet.querySelector(".dw-sheet-foot") as HTMLElement;
    const apply = within(footer).getByRole("button", { name: /Apply plan/ });
    const field = within(footer).getByLabelText("Type a1b2c3d4 to apply it");
    expect(apply).toBeDisabled();
    expect(apply).toHaveAccessibleDescription("Type a1b2c3d4 to apply it");
    expect(apply).toHaveClass("dw-btn-danger");
    await waitFor(() => expect(field).toHaveFocus());
    await user.type(field, "a1b2c3d4");
    expect(apply).toBeEnabled();
    await expectAccessible(document.body);
    // Enter in the field applies it, as the button beside it says.
    await user.keyboard("{Enter}");
    await waitFor(() => expect(applies[0]?.fingerprint).toBe("a1b2c3d4e5f60718293a4b5c6d7e8f90"));
  });

  it("says what a route does to its server's ingress, and warns of DNS without refusing", async () => {
    const routed = webPlanned();
    const step = routed.plan?.servers[0]?.steps[0];
    if (!step) throw new Error("webPlanned has no step");
    step.ingress = create(IngressEffectSchema, {
      change: IngressChange.STARTS,
      proxy: "Caddy",
      ports: ["80/tcp", "443/tcp", "443/udp"],
      panelDomain: "panel.example.com",
    });
    step.domainCheck = create(DomainCheckSchema, {
      domain: "app.example.com",
      state: DomainCheckState.ELSEWHERE,
      found: ["198.51.100.1"],
      here: ["203.0.113.9"],
    });
    step.returningContainers = ["1Panel-openresty-abc12"];
    renderSheet(proposeWeb, {
      changes: {
        proposeChange: () => ({ change: routed }),
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });

    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(sheet).toHaveTextContent(
      "Caddy isn't running on hz-1, so this route starts it, and it takes ports 80/tcp, 443/tcp, 443/udp",
    );
    expect(sheet).toHaveTextContent("The panel's domain, panel.example.com, moves behind Caddy");
    expect(sheet).toHaveTextContent("it keeps its present certificate until Caddy has its own");
    expect(sheet).toHaveTextContent("the SSH tunnel keeps working");
    expect(sheet).toHaveTextContent("DNS for app.example.com points at 198.51.100.1, not this server (203.0.113.9)");
    expect(sheet).toHaveTextContent("Docker starts 1Panel-openresty-abc12 again when hz-1 next restarts");
    expect(sheet).toHaveTextContent("docker update --restart=no 1Panel-openresty-abc12");
    // It warns, and nothing asks for more than any other plan does.
    const apply = await within(sheet).findByRole("button", { name: /Apply plan/ });
    expect(apply).toBeEnabled();
    await expectAccessible(document.body);
  });

  it("says what a route without a certificate means, and doesn't call it an alarm", async () => {
    const routed = webPlanned();
    const step = routed.plan?.servers[0]?.steps[0];
    if (!step) throw new Error("webPlanned has no step");
    step.unencrypted = true;
    step.answers = create(RouteAnswersSchema, { reach: RouteReach.THIS_SERVER, plain: true });
    step.ingress = create(IngressEffectSchema, {
      change: IngressChange.STARTS,
      proxy: "Caddy",
      ports: ["80/tcp"],
    });
    renderSheet(proposeWeb, {
      changes: {
        proposeChange: () => ({ change: routed }),
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });

    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(sheet).toHaveTextContent("so this route starts it, and it takes ports 80/tcp");
    const notice = within(sheet).getByText(/Traffic for this route reaches hz-1 unencrypted/);
    expect(notice).toHaveTextContent("That's fine on a tunnel or a private network, and not on the open internet");
    expect(notice).toHaveClass("plan-step-notice");
    expect(notice).not.toHaveClass("plan-step-warn");
    const answers = within(sheet).getByText(/It answers only what runs on hz-1 itself/);
    expect(answers).toHaveClass("plan-step-notice");
    // It's something to know, and nothing asks for more than any other plan does.
    const apply = await within(sheet).findByRole("button", { name: /Apply plan/ });
    expect(apply).toBeEnabled();
    await expectAccessible(document.body);
  });

  it("names a port on the server and a proxy to believe, as what a person approves", async () => {
    const routed = webPlanned();
    const step = routed.plan?.servers[0]?.steps[0];
    if (!step) throw new Error("webPlanned has no step");
    step.hostAccess = [
      create(HostAccessSchema, { kind: HostAccessKind.SERVER_PORT, path: "127.0.0.1:8080", detail: "app.example.com" }),
      create(HostAccessSchema, { kind: HostAccessKind.TRUSTED_PROXY, path: "cloudflare" }),
      create(HostAccessSchema, { kind: HostAccessKind.PLAIN_FOR_ANYONE, path: "lan.example.com" }),
      create(HostAccessSchema, { kind: HostAccessKind.HTTPS_OFF, path: "live.example.com" }),
    ];
    step.answers = create(RouteAnswersSchema, { reach: RouteReach.ANYONE, plain: true });
    step.serverPort = create(ServerPortCheckSchema, { port: 8080, listening: "nginx, in the container web-1" });
    step.ingress = create(IngressEffectSchema, {
      change: IngressChange.TAKES_PORTS,
      proxy: "Caddy",
      ports: ["443/tcp", "443/udp"],
    });
    renderSheet(proposeWeb, {
      changes: {
        proposeChange: () => ({ change: routed }),
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });

    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await waitFor(() => expect(sheet.querySelector(".plan-risk")).toBeTruthy());
    expect(sheet).toHaveTextContent("web reaches the port 127.0.0.1:8080 on hz-1");
    expect(sheet).toHaveTextContent("That is the server itself, past its containers, at app.example.com.");
    expect(sheet).toHaveTextContent("web believes the proxy cloudflare on hz-1");
    expect(sheet).toHaveTextContent("It takes that proxy's word for where a request came from.");
    expect(sheet).toHaveTextContent("web answers plain HTTP on lan.example.com on hz-1");
    expect(sheet).toHaveTextContent("Anyone who can reach this server gets it, around whatever is in front of it.");
    expect(sheet).toHaveTextContent("web stops HTTPS on live.example.com on hz-1");
    expect(sheet).toHaveTextContent(
      "https:// addresses get an error, and browsers that saw HSTS from it refuse plain HTTP until that runs out.",
    );
    expect(sheet).toHaveTextContent("It answers anyone who can reach hz-1 on port 80, over plain HTTP");
    expect(sheet).toHaveTextContent("That lets anyone who can reach hz-1 reach port 8080");
    expect(sheet).toHaveTextContent("which an app that trusts localhost will believe");
    expect(sheet).toHaveTextContent("Listening there now: nginx, in the container web-1");
    expect(sheet).toHaveTextContent(
      "This is the first route on hz-1 that takes a certificate, so Caddy takes ports 443/tcp, 443/udp too",
    );
  });

  it("says how a step rolls, what a drain ends, and that rolling back leaves the data", async () => {
    const rolling = webPlanned();
    const step = rolling.plan?.servers[0]?.steps[0];
    if (!step || !rolling.plan) throw new Error("webPlanned has no step");
    step.effect = StepEffect.ROLL;
    step.downtime = undefined;
    const server = rolling.plan.servers[0];
    if (server) server.downtime = undefined;
    step.changes = [];
    step.rollout = create(RolloutPlanSchema, {
      rolling: true,
      drain: create(DurationSchema, { seconds: 30n }),
      healthyWithin: create(DurationSchema, { seconds: 90n }),
      soak: create(DurationSchema, { seconds: 5n }),
      others: ["env_01abc-web-1"],
      earlyNetworks: ["back"],
      earlyNames: ["db"],
      dropped: [create(DroppedMountSchema, { source: "cache-data", target: "/data", emptyVolume: true })],
    });
    rolling.plan.notes = [
      create(PlanNoteSchema, {
        reason: "volumes_stay",
        names: ["uploads"],
        resource: create(ResourceRefSchema, { kind: "service", name: "web" }),
      }),
    ];
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: rolling }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: /plan/i });
    expect(sheet).toHaveTextContent("rolling, with nothing stopped");
    expect(sheet).toHaveTextContent("A new container takes over once it has been healthy for");
    expect(sheet).toHaveTextContent("Dockwell rolls this back");
    expect(sheet).toHaveTextContent("What is in flight gets up to");
    expect(sheet).toHaveTextContent("ends with the old container");
    expect(sheet).toHaveTextContent(
      "On back, other services can reach the new container by its service's name and by db",
    );
    // A mount that goes is worth weighing, so it's a warning, and the only one here.
    const warned = within(sheet)
      .getByText(/cache-data stops being/)
      .closest(".dw-callout") as HTMLElement;
    expect(warned).toHaveClass("dw-tone-warning");
    expect(warned).toHaveTextContent("Its image puts an empty volume there");
    // Data that stays is said once, and quietly on a plan that isn't going back.
    expect(sheet).toHaveTextContent("If web is rolled back, uploads stays as it is");
    expect(warned).not.toHaveTextContent("uploads");
    // One old container is what any replace removes, and its name has an ID in it.
    expect(sheet).not.toHaveTextContent("env_01abc");
    await expectAccessible(document.body);
  });

  it("says only what's true of what's stopped, as nothing is up to go on serving", async () => {
    const stopped = webPlanned();
    const step = stopped.plan?.servers[0]?.steps[0];
    if (!step) throw new Error("webPlanned has no step");
    step.downtime = undefined;
    const server = stopped.plan?.servers[0];
    if (server) server.downtime = undefined;
    step.changes = [create(FieldChangeSchema, { path: "state", from: "exited", to: "running" })];
    step.rollout = create(RolloutPlanSchema, {
      rolling: true,
      soak: create(DurationSchema, { seconds: 5n }),
      healthyWithin: create(DurationSchema, { seconds: 90n }),
      drain: create(DurationSchema, { seconds: 30n }),
      others: ["web-1"],
      earlyNetworks: ["back"],
    });
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: stopped }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: /plan/i });
    expect(sheet).toHaveTextContent("It has to stay healthy for 5 s");
    expect(sheet).not.toHaveTextContent("rolling, with nothing stopped");
    expect(sheet).not.toHaveTextContent("goes on serving");
    expect(sheet).not.toHaveTextContent("in flight");
    expect(sheet).not.toHaveTextContent("beside the old one");
  });

  it("says nothing of stopping first for what's already stopped", async () => {
    const stopped = webPlanned();
    const step = stopped.plan?.servers[0]?.steps[0];
    if (!step) throw new Error("webPlanned has no step");
    step.restart = "chg_1";
    step.changes = [create(FieldChangeSchema, { path: "state", from: "exited", to: "running" })];
    step.rollout = create(RolloutPlanSchema, {
      ports: ["8080/tcp"],
      volumes: ["data"],
      hostNetwork: true,
      chosen: true,
    });
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: stopped }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: /plan/i });
    expect(sheet).not.toHaveTextContent("stops first");
    expect(sheet).not.toHaveTextContent("stop first");
    expect(sheet).not.toHaveTextContent("Two containers");
  });

  it("says why a step has to stop first, with its downtime", async () => {
    const stopping = webPlanned();
    const step = stopping.plan?.servers[0]?.steps[0];
    if (!step || !stopping.plan) throw new Error("webPlanned has no step");
    step.effect = StepEffect.RECREATE;
    step.downtime = create(DurationSchema, { seconds: 40n });
    const server = stopping.plan.servers[0];
    if (server) server.downtime = create(DurationSchema, { seconds: 40n });
    step.rollout = create(RolloutPlanSchema, {
      ports: ["8080/tcp"],
      volumes: ["data"],
      paths: ["/srv/a"],
      imageData: ["/var/lib/x"],
      cantTell: ["nfs-share"],
      editedRoutes: ["www"],
      hostNetwork: true,
    });
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: stopping }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: /plan/i });
    expect(sheet).toHaveTextContent("about 40 s down");
    expect(sheet).toHaveTextContent("Two containers can't publish 8080/tcp at once");
    expect(sheet).toHaveTextContent("A container on the server's own network listens on the server's ports itself");
    expect(sheet).toHaveTextContent("Two containers mustn't write to the volume data at once");
    expect(sheet).toHaveTextContent("Two containers mustn't write to the server path /srv/a at once");
    expect(sheet).toHaveTextContent("Its image keeps data at /var/lib/x");
    expect(sheet).toHaveTextContent("Dockwell can't read where nfs-share really lands");
    expect(sheet).toHaveTextContent("The route www was edited by hand");
    expect(sheet).not.toHaveTextContent("takes over once it has been healthy");
  });

  it("makes two writers on shared data the plan's first risk", async () => {
    const chosen = webPlanned();
    const step = chosen.plan?.servers[0]?.steps[0];
    if (!step || !chosen.plan) throw new Error("webPlanned has no step");
    chosen.plan.destructive = true;
    step.rollout = create(RolloutPlanSchema, { rolling: true, chosen: true, volumes: ["data"] });
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: chosen }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: /plan/i });
    const risk = within(sheet).getByRole("alert");
    expect(risk).toHaveTextContent("Two containers may write to data");
    expect(risk).toHaveTextContent("which can corrupt a database");
  });

  it("doesn't tell of a rollback a restart doesn't have", async () => {
    const restart = webPlanned();
    const step = restart.plan?.servers[0]?.steps[0];
    if (!step) throw new Error("webPlanned has no step");
    step.restart = "chg_1";
    step.downtime = undefined;
    step.rollout = create(RolloutPlanSchema, {
      rolling: true,
      soak: create(DurationSchema, { seconds: 5n }),
      healthyWithin: create(DurationSchema, { seconds: 90n }),
    });
    // What it would roll back to, were this a deploy.
    const service = restart.plan?.resources[0];
    if (!service) throw new Error("webPlanned has no resource");
    service.resource = create(ResourceRefSchema, { kind: "service", name: "web" });
    service.baseRevision = 3n;
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: restart }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: /plan/i });
    expect(sheet).toHaveTextContent("the new one goes, and what runs stays");
    expect(sheet).not.toHaveTextContent("Dockwell rolls this back");
    expect(sheet).not.toHaveTextContent("Rolls back to");
    expect(sheet).not.toHaveTextContent("rolls back to what it runs now");
  });

  it("warns that a restart which stops first has nothing to go back to", async () => {
    const restart = webPlanned();
    const step = restart.plan?.servers[0]?.steps[0];
    if (!step) throw new Error("webPlanned has no step");
    step.restart = "chg_1";
    step.rollout = create(RolloutPlanSchema, { imageData: ["/var/lib/x"] });
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: restart }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: /plan/i });
    const warned = within(sheet)
      .getByText(/The old container stops first/)
      .closest(".dw-callout") as HTMLElement;
    expect(warned).toHaveClass("dw-tone-warning");
    expect(warned).toHaveTextContent("a restart has nothing to go back to");
  });

  it("says a stopped app starts, and what changing a route costs the other sites", async () => {
    const stopped = webPlanned();
    if (!stopped.plan) throw new Error("webPlanned has no plan");
    stopped.plan.notes = [
      create(PlanNoteSchema, {
        reason: "starts_stopped",
        message: "web is stopped, so this starts it",
        resource: create(ResourceRefSchema, { kind: "service", name: "web" }),
      }),
    ];
    const step = stopped.plan.servers[0]?.steps[0];
    if (!step) throw new Error("webPlanned has no step");
    step.resource = create(ResourceRefSchema, { kind: "route", name: "www" });
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: stopped }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: /plan/i });
    expect(sheet).toHaveTextContent("web is stopped, so this starts it.");
    expect(sheet).toHaveTextContent("This reloads the proxy on hz-1");
  });

  it("says what stopping the ingress leaves behind, and names the domain a plan takes", async () => {
    const routed = webPlanned();
    const plan = routed.plan;
    const step = plan?.servers[0]?.steps[0];
    if (!plan || !step) throw new Error("webPlanned has no step");
    step.ingress = create(IngressEffectSchema, {
      change: IngressChange.STOPS,
      proxy: "Caddy",
      ports: ["80/tcp", "443/tcp", "443/udp"],
    });
    plan.hostAccess = true;
    step.hostAccess = [
      create(HostAccessSchema, { kind: HostAccessKind.DOMAIN, path: "app.example.com", detail: "production" }),
      create(HostAccessSchema, { kind: HostAccessKind.PANEL_DOMAIN, path: "panel.example.com", detail: "Caddy" }),
    ];
    renderSheet(proposeWeb, {
      changes: {
        proposeChange: () => ({ change: routed }),
        watchChange: feed<WatchChangeResponse>().stream,
      },
    });

    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(sheet).toHaveTextContent(
      "This is the last route on hz-1, so Caddy stops, and ports 80/tcp, 443/tcp, 443/udp are free",
    );
    expect(sheet).toHaveTextContent("Its certificates stay on the server");
    expect(sheet).toHaveTextContent(
      "the domain app.example.com, which the environment production held within the last 7 days",
    );
    expect(sheet).toHaveTextContent("the panel's domain, panel.example.com, which moves behind Caddy");
  });

  it("marks what reaches the server as a risk in the diff, never as a green addition", async () => {
    const reaching = webPlanned();
    const plan = reaching.plan;
    const resource = plan?.resources[0];
    if (!plan || !resource) throw new Error("webPlanned has no resource");
    plan.hostAccess = true;
    resource.action = PlanAction.CREATE;
    resource.hostAccess = [
      create(HostAccessSchema, { kind: HostAccessKind.DOCKER_SOCKET, path: "/var/run/docker.sock" }),
    ];
    resource.changes = [
      create(FieldChangeSchema, { path: "image", op: FieldOp.ADDED, to: "nginx:1.28" }),
      create(FieldChangeSchema, { path: "mounts[0].host_path", op: FieldOp.ADDED, to: "/var/run/docker.sock" }),
      create(FieldChangeSchema, { path: "mounts[0].target", op: FieldOp.ADDED, to: "/var/run/docker.sock" }),
    ];
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: reaching }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const socket = await within(sheet).findByText("Docker's socket");
    expect(socket).toHaveClass("is-critical");
    expect(socket.nextElementSibling?.querySelector(".dw-diff-critical")).toHaveTextContent("/var/run/docker.sock");
    expect(socket.nextElementSibling?.querySelector(".dw-diff-to")).toBeNull();
    // What puts the server at risk comes first in its resource.
    expect(sheet.querySelector(".dw-plan-changes dt")).toBe(socket);
  });

  it("leads with what a plan says about itself, and says it in its own words", async () => {
    const noted = webPlanned();
    if (noted.plan) {
      noted.plan.intent = create(IntentSchema, { kind: IntentKind.ROLL_BACK, resourceId: "svc_web" });
      noted.plan.notes = [
        create(PlanNoteSchema, {
          reason: "volumes_stay",
          message: "web keeps what is in data as it is. Check before you apply",
          resource: { kind: "service", name: "web" },
          names: ["data", "/srv/data"],
        }),
        create(PlanNoteSchema, {
          reason: "exposed_variable",
          message: "web's command holds the value of TOKEN.",
          resource: { kind: "service", name: "web" },
          names: ["TOKEN", "command"],
        }),
        create(PlanNoteSchema, { reason: "newer", message: "This reason is new. The panel doesn't know it yet." }),
      ];
    }
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: noted }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const callout = (await within(sheet).findByText(/keeps what is in/)).closest(".dw-callout") as HTMLElement;
    expect(callout).toHaveClass("dw-tone-warning");
    expect(callout.querySelector(".dw-callout-title")).toHaveTextContent(
      "web keeps what is in data, /srv/data as it is",
    );
    expect(callout).toHaveTextContent("A rollback puts back its spec, not its data");
    expect(callout).toHaveTextContent(
      "web's command holds the value of TOKEN. Anyone who can read this change, or export this environment, can read it.",
    );
    expect(callout).toHaveTextContent("write the command as a list, with each word its own variable.");
    expect(callout).toHaveTextContent("This reason is new. The panel doesn't know it yet.");
    expect(sheet.querySelector(".plan-body")?.firstElementChild).toBe(callout);
    // It stops nobody.
    expect(within(sheet).getByRole("button", { name: /Apply plan/ })).toBeEnabled();
    await expectAccessible(document.body);
  });

  it("says what an offline server keeps of a deleted service, and until when", async () => {
    const noted = webPlanned();
    if (noted.plan) {
      noted.plan.notes = [
        create(PlanNoteSchema, {
          reason: "kept_logs_offline",
          message: "b is offline, so what it kept of web stays on its disk until it's back.",
          resource: { kind: "service", name: "web" },
          names: ["b"],
        }),
      ];
    }
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: noted }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const callout = (await within(sheet).findByText(/stays on its disk/)).closest(".dw-callout") as HTMLElement;
    expect(callout.querySelector(".dw-callout-title")).toHaveTextContent(
      "b is offline, so what it kept of web stays on its disk until it's back",
    );
    expect(callout).toHaveTextContent(
      "It can't be read meanwhile, and it goes once the server is back and the 7 days are up.",
    );
    await expectAccessible(document.body);
  });

  it("leaves a plan's own word to a row once something worse has the Callout", async () => {
    const noted = webPlanned();
    if (noted.plan) {
      noted.plan.destructive = true;
      noted.plan.notes = [create(PlanNoteSchema, { message: "This is worth knowing. Look at it." })];
    }
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: noted }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(await within(sheet).findByRole("alert")).toHaveTextContent("This plan destroys data");
    expect(
      within(sheet)
        .getAllByText(/./)
        .filter((el) => el.classList.contains("dw-callout-title")),
    ).toHaveLength(1);
    expect(within(sheet).getByRole("region", { name: "What applying means" })).toHaveTextContent(
      "This is worth knowing",
    );
  });

  it("gives data loss the same treatment: it leads, it is marked in the diff, and it asks first", async () => {
    const losing = webPlanned();
    const plan = losing.plan;
    const step = plan?.servers[0]?.steps[0];
    const resource = plan?.resources[0];
    if (!plan || !step || !resource) throw new Error("webPlanned has no step");
    plan.destructive = true;
    resource.action = PlanAction.DELETE;
    step.action = PlanAction.DELETE;
    step.keptPaths = ["/var/lib/postgresql/data"];
    step.destroysData = true;
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: losing }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    expect(await within(sheet).findByRole("alert")).toHaveTextContent(
      "web loses what its image keeps at /var/lib/postgresql/data",
    );
    // Open though it deletes, as what is at risk is never folded away.
    const row = within(sheet).getByText("data taken out of use");
    expect(row).toBeVisible();
    expect(row).toHaveClass("is-critical");
    expect(row.nextElementSibling?.querySelector(".dw-diff-critical")).toHaveTextContent("/var/lib/postgresql/data");
    expect(within(sheet).getByRole("button", { name: /Delete web/ })).toBeDisabled();
    expect(within(sheet).getByLabelText("Type a1b2c3d4 to apply it")).toBeVisible();
  });

  it("discards the plan it made when you cancel", async () => {
    const user = userEvent.setup();
    const discarded: string[] = [];
    renderSheet(
      { kind: "propose", summary: "Create the project shop", edits: [] },
      {
        changes: {
          proposeChange: () => ({ change: shopPlanned() }),
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: (req) => {
            discarded.push(req.id);
            return {};
          },
        },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to create");
    expect(sheet).toHaveTextContent("2 to create");
    // Projects run on no server: there are no steps to show, and nothing goes down.
    expect(within(sheet).queryByRole("region", { name: /Steps/ })).toBeNull();
    expect(sheet).toHaveTextContent("No server · nothing goes down");
    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(discarded).toEqual(["chg_shop"]));
    expect(screen.queryByRole("dialog")).toBeNull();
  });

  it("discards a plan that arrives after you close the sheet", async () => {
    const user = userEvent.setup();
    const discarded: string[] = [];
    let made: (value: { change: ReturnType<typeof shopPlanned> }) => void = () => {};
    renderSheet(
      { kind: "propose", summary: "Create the project shop", edits: [] },
      {
        changes: {
          proposeChange: () => new Promise((resolve) => (made = resolve)),
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: (req) => {
            discarded.push(req.id);
            return {};
          },
        },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Planning" });
    await user.click(within(sheet).getAllByRole("button", { name: "Close" }).at(-1) as HTMLElement);
    expect(screen.queryByRole("dialog")).toBeNull();
    // The control plane finishes planning after all.
    act(() => made({ change: shopPlanned() }));
    await waitFor(() => expect(discarded).toEqual(["chg_shop"]));
  });

  it("plans once in StrictMode, which mounts the sheet twice, and discards that one plan", async () => {
    const user = userEvent.setup();
    const proposals = vi.fn(() => ({ change: shopPlanned() }));
    const discarded: string[] = [];
    renderWithApi(
      <StrictMode>
        <ChangeSheetProvider>
          <Opener opening={{ kind: "propose", summary: "Create the project shop", edits: [] }} />
        </ChangeSheetProvider>
      </StrictMode>,
      {
        changes: {
          proposeChange: proposals,
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: (req) => {
            discarded.push(req.id);
            return {};
          },
        },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to create");
    expect(proposals).toHaveBeenCalledTimes(1);
    expect(discarded).toEqual([]);
    await user.click(within(sheet).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(discarded).toEqual(["chg_shop"]));
  });

  it("discards the plan it made when another sheet opens in its place", async () => {
    const discarded: string[] = [];
    let api: ReturnType<typeof useChangeSheet> | undefined;
    function Handle() {
      api = useChangeSheet();
      return null;
    }
    renderWithApi(
      <ChangeSheetProvider>
        <Handle />
      </ChangeSheetProvider>,
      {
        changes: {
          proposeChange: () => ({ change: shopPlanned() }),
          getChange: () => ({ change: applied(webPlanned()) }),
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: (req) => {
            discarded.push(req.id);
            return {};
          },
        },
      },
    );
    act(() => api?.open({ kind: "propose", summary: "Create the project shop", edits: [] }));
    await within(await screen.findByRole("dialog", { name: "Review plan" })).findByText("to create");
    // The command bar opens a recent change's plan over it.
    act(() => api?.open({ kind: "change", id: "chg_web" }));
    expect(await screen.findByRole("dialog", { name: "Applied" })).toBeVisible();
    await waitFor(() => expect(discarded).toEqual(["chg_shop"]));
  });

  it("counts only the resources that change, as the plan's summary does", async () => {
    const planned = shopPlanned();
    const [project] = planned.plan?.resources ?? [];
    if (project) project.action = PlanAction.NONE;
    if (planned.plan?.summary) planned.plan.summary.create = 1;
    renderSheet(
      { kind: "change", id: "chg_shop" },
      { changes: { getChange: () => ({ change: planned }), watchChange: feed<WatchChangeResponse>().stream } },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to create");
    expect(sheet).toHaveTextContent("1 to create");
    expect(sheet).not.toHaveTextContent(/\b0 to/);
    expect(within(sheet).queryByText("shop")).toBeNull();
    expect(within(sheet).getByText("production")).toBeVisible();
  });

  it("leaves a plan someone else made when you close it", async () => {
    const user = userEvent.setup();
    const discard = vi.fn();
    renderSheet(
      { kind: "change", id: "chg_shop" },
      {
        changes: {
          getChange: () => ({ change: shopPlanned() }),
          watchChange: feed<WatchChangeResponse>().stream,
          discardChange: discard,
        },
      },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("2");
    await user.click(within(sheet).getByRole("button", { name: "Close" }));
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(discard).not.toHaveBeenCalled();
  });

  it("says why a plan won't apply in plain words, and the plan stays to try again", async () => {
    const user = userEvent.setup();
    renderSheet(proposeWeb, {
      changes: {
        proposeChange: () => ({ change: webPlanned() }),
        watchChange: feed<WatchChangeResponse>().stream,
        applyChange: () => {
          throw new ConnectError(
            "another change is applying web now, so this one can't start yet. Apply it again once that one ends (chg_01M6MGMR9)",
            Code.FailedPrecondition,
          );
        },
      },
    });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const apply = await within(sheet).findByRole("button", { name: /Apply plan/ });
    await user.click(apply);
    const alert = await within(sheet).findByRole("alert");
    expect(alert).toHaveTextContent("That didn't apply");
    expect(alert).toHaveTextContent(
      "Another change is applying web now, so this one can't start yet. Apply it again once that one ends.",
    );
    expect(alert).not.toHaveTextContent("chg_");
    expect(apply).toBeEnabled();
  });

  it("keeps a server's name as it is written when it starts a sentence", async () => {
    const user = userEvent.setup();
    renderSheet(proposeWeb, {
      changes: {
        proposeChange: () => ({ change: webPlanned() }),
        watchChange: feed<WatchChangeResponse>().stream,
        applyChange: () => {
          throw new ConnectError(
            "hz-1 isn't connected, so this change can't apply. Apply it again once its agent reconnects",
            Code.Unavailable,
          );
        },
      },
    });
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await user.click(await within(sheet).findByRole("button", { name: /Apply plan/ }));
    expect(await within(sheet).findByRole("alert")).toHaveTextContent(
      "hz-1 isn't connected, so this change can't apply. Apply it again once its agent reconnects.",
    );
  });

  it("gives a failure a capital where it starts with a word, and leaves a name as it is", async () => {
    const failed = (error: string) => moved(webPlanned(), { state: ChangeState.FAILED, error, steps: [] });
    const first = renderSheet(
      { kind: "plan", make: () => Promise.resolve({ change: failed("the server's agent isn't connected") }) },
      { changes: { watchChange: feed<WatchChangeResponse>().stream } },
    );
    expect((await screen.findByRole("alert")).querySelector(".dw-callout-title")).toHaveTextContent(
      "The server's agent isn't connected",
    );
    first.unmount();
    renderSheet(
      { kind: "plan", make: () => Promise.resolve({ change: failed("hz-1: it exits. Check it") }) },
      { changes: { watchChange: feed<WatchChangeResponse>().stream } },
    );
    expect((await screen.findByRole("alert")).querySelector(".dw-callout-title")).toHaveTextContent("hz-1: it exits");
  });

  it("says why a plan couldn't be made", async () => {
    renderSheet(proposeWeb, {
      changes: {
        proposeChange: () => {
          throw new ConnectError(
            "there are two projects, default and shop. Name the one the environment is for",
            Code.InvalidArgument,
          );
        },
      },
    });
    const sheet = await screen.findByRole("dialog", { name: "The plan didn't work" });
    expect(await within(sheet).findByRole("alert")).toHaveTextContent(
      "There are two projects, default and shop. Name the one the environment is for.",
    );
  });

  it("fills a phone's screen, and reads in Chinese", async () => {
    onPhone();
    await activateLocale("zh-CN");
    renderSheet(proposeWeb, {
      changes: { proposeChange: () => ({ change: webPlanned() }), watchChange: feed<WatchChangeResponse>().stream },
    });
    const sheet = await screen.findByRole("dialog", { name: "审查计划" });
    expect(sheet).toHaveClass("is-phone");
    expect(within(sheet).getByRole("button", { name: /应用计划/ })).toBeVisible();
    await expectAccessible(document.body);
  });
});

type Timed = { startTime?: Timestamp; finishTime?: Timestamp };

describe("the plan sheet, once it runs", () => {
  const aStep = (fields: { state: StepState; message?: string; activity?: string; detail?: string } & Timed) =>
    create(StepProgressSchema, { serverId: "srv_1", resourceId: "res_web", ...fields });

  /** The fleet: hz-1, which people reach on a public address. */
  async function* serversWatch(req: unknown, ctx: HandlerContext) {
    yield changed(aServer({ id: "srv_1", name: "hz-1", inventory: { addresses: ["203.0.113.9"] } }));
    yield snapshotComplete;
    yield* quietWatch(req, ctx);
  }

  function aService(ports: MessageInitShape<typeof PortMappingSchema>[]): MessageInitShape<typeof ServiceInfoSchema> {
    return {
      id: "res_web",
      resource: { kind: "service", scope: "env_1", name: "web" },
      spec: { runs: { case: "app", value: { image: "nginx:1.28", ports } } },
      deployments: [{ serverId: "srv_1", serverName: "hz-1" }],
    };
  }

  function aServicePlan(): Change {
    const change = webPlanned();
    const resource = change.plan?.resources[0];
    const step = change.plan?.servers[0]?.steps[0];
    if (resource?.resource) resource.resource.kind = "service";
    if (step?.resource) step.resource.kind = "service";
    return change;
  }

  it("says what it is doing, for how long, in figures that hold still", async () => {
    const watch = feed<WatchChangeResponse>();
    const planned = aServicePlan();
    renderSheet(
      { kind: "plan", make: () => Promise.resolve({ change: planned }) },
      { changes: { watchChange: watch.stream, applyChange: () => ({ change: planned }) } },
    );
    await screen.findByRole("dialog", { name: "Review plan" });
    act(() =>
      watch.push(
        watched(
          moved(planned, {
            state: ChangeState.APPLYING,
            steps: [
              aStep({
                state: StepState.RUNNING,
                message: "Pulling nginx:1.28: 12 of 48 MB",
                activity: "Pulling nginx:1.28: 12 of 48 MB",
                startTime: timestampFromDate(new Date(Date.now() - 6_000)),
              }),
            ],
          }),
        ),
      ),
    );
    const applying = await screen.findByRole("dialog", { name: "Applying" });
    const step = await within(applying).findByText("Pulling nginx:1.28: 12 of 48 MB");
    const elapsed = step.closest(".plan-step")?.querySelector(".plan-step-time");
    expect(elapsed?.textContent).toMatch(/^[67] s$/);
    // The downtime is still to come, and in the future tense.
    expect(applying).toHaveTextContent("hz-1 · about 2 s down");
  });

  it("arrives: the title says it is deployed, and its address is the first line", async () => {
    const change = applied(aServicePlan());
    change.steps = [
      aStep({
        state: StepState.DONE,
        startTime: timestampFromDate(new Date(Date.now() - 6_000)),
        finishTime: timestampFromDate(new Date()),
      }),
    ];
    renderSheet(
      { kind: "plan", make: () => Promise.resolve({ change }), doneLabel: "Open web" },
      {
        services: { listServices: () => ({ services: [aService([{ hostPort: 8080, containerPort: 3000 }])] }) },
        servers: { watchServers: serversWatch },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      },
    );
    const done = await screen.findByRole("dialog", { name: "web is deployed" });
    const link = await within(done).findByRole("link", { name: "203.0.113.9:8080" });
    expect(link).toHaveAttribute("href", "http://203.0.113.9:8080");
    expect(link.closest(".plan-body")?.firstElementChild).toContainElement(link);
    expect(done).toHaveTextContent("1 replaced");
    expect(done).not.toHaveTextContent("no downtime");
    expect(within(done).queryByRole("alert")).toBeNull();
    expect(done.querySelector(".dw-callout")).toBeNull();
    // With one server the steps say nothing the plan doesn't, once it has landed.
    expect(within(done).queryByRole("region", { name: "Steps" })).toBeNull();
    expect(within(done).getByRole("button", { name: /Open web/ })).toBeVisible();
    await expectAccessible(document.body);
  });

  it("says so when nothing can reach it yet, and what to do about that", async () => {
    const change = applied(aServicePlan());
    renderSheet(
      { kind: "plan", make: () => Promise.resolve({ change }) },
      {
        services: { listServices: () => ({ services: [aService([])] }) },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      },
    );
    const done = await screen.findByRole("dialog", { name: "web is deployed" });
    expect(await within(done).findByRole("button", { name: "Add a domain" })).toBeVisible();
    expect(within(done).getByRole("button", { name: "Add a port" })).toBeVisible();
    expect(done).toHaveTextContent("Nothing outside hz-1 reaches web yet.");
    await expectAccessible(document.body);
  });

  it("offers a port or a domain for each app, when several have nothing reaching them", async () => {
    const change = applied(aServicePlan());
    const second = create(PlannedResourceSchema, {
      id: "res_api",
      resource: { kind: "service", scope: "env_1", name: "api" },
      action: PlanAction.REPLACE,
    });
    change.plan?.resources.push(second);
    change.plan?.servers[0]?.steps.push(
      create(PlanStepSchema, { resourceId: "res_api", resource: second.resource, action: PlanAction.REPLACE }),
    );
    renderSheet(
      { kind: "plan", make: () => Promise.resolve({ change }) },
      {
        services: {
          listServices: () => ({
            services: [aService([]), { ...aService([]), id: "res_api", resource: second.resource }],
          }),
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      },
    );
    const done = await screen.findByRole("dialog");
    expect(await within(done).findByRole("button", { name: "Add a domain to web" })).toBeVisible();
    expect(within(done).getByRole("button", { name: "Add a port to web" })).toBeVisible();
    expect(within(done).getByRole("button", { name: "Add a domain to api" })).toBeVisible();
    expect(within(done).getByRole("button", { name: "Add a port to api" })).toBeVisible();
    expect(done).toHaveTextContent("Nothing outside the server reaches these yet.");
    await expectAccessible(document.body);
  });

  describe("with a domain", () => {
    const aDomain = (
      name: string,
      state: CertificateState,
      message = "",
      service = "web",
      url = `https://${name}`,
    ) => ({
      name,
      url,
      routes: [{ id: `res_${name}`, name: name.replaceAll(".", "-"), service, port: 80 }],
      servers: [{ serverName: "hz-1", status: { domain: name, certificate: { state, message } } }],
    });
    const arrivedWith = (
      domains: ReturnType<typeof aDomain>[],
      ports: MessageInitShape<typeof PortMappingSchema>[] = [],
      change = applied(aServicePlan()),
    ) => {
      const planned = change.plan?.resources[0]?.resource;
      if (planned) planned.scope = "env_1";
      const asked: string[] = [];
      renderSheet(
        { kind: "plan", make: () => Promise.resolve({ change }) },
        {
          services: { listServices: () => ({ services: [aService(ports)] }) },
          domains: {
            listDomains: (req) => {
              asked.push(req.environment);
              return { domains };
            },
          },
          changes: { watchChange: feed<WatchChangeResponse>().stream },
        },
      );
      return asked;
    };

    it("leads with the domain, ahead of a port, and opens it over HTTPS", async () => {
      const user = userEvent.setup();
      const asked = arrivedWith(
        [aDomain("app.example.com", CertificateState.ISSUED)],
        [{ hostPort: 8080, containerPort: 80 }],
      );
      const done = await screen.findByRole("dialog", { name: "web is deployed" });
      const link = await within(done).findByRole("link", { name: "app.example.com" });
      expect(link).toHaveAttribute("href", "https://app.example.com");
      expect(asked).toContain("env_1");
      const rows = done.querySelectorAll(".plan-reach-row");
      expect(rows).toHaveLength(2);
      expect(rows[0]).toContainElement(link);
      await user.click(within(done).getByRole("button", { name: "Copy app.example.com" }));
      expect(await navigator.clipboard.readText()).toBe("https://app.example.com");
      await expectAccessible(document.body);
    });

    it("sends a visitor to a tunnel's domain over HTTPS, in its link and its copy, as what's in front ends TLS", async () => {
      const user = userEvent.setup();
      arrivedWith([aDomain("app.example.com", CertificateState.NONE, "", "web", "http://app.example.com")]);
      const done = await screen.findByRole("dialog", { name: "web is deployed" });
      const link = await within(done).findByRole("link", { name: "app.example.com" });
      expect(link).toHaveAttribute("href", "https://app.example.com");
      expect(link.closest(".plan-reach-row")).toHaveTextContent("TLS ends in front");
      await user.click(within(done).getByRole("button", { name: "Copy app.example.com" }));
      expect(await navigator.clipboard.readText()).toBe("https://app.example.com");
      expect(done).not.toHaveTextContent("Nothing outside");
    });

    it("gives http:// only to a route that answers anyone over plain HTTP, and says that's what it does", async () => {
      const user = userEvent.setup();
      const plain = aDomain("app.example.com", CertificateState.NONE, "", "web", "http://app.example.com");
      arrivedWith([
        { ...plain, routes: plain.routes.map((route) => ({ ...route, tls: "off", plainForAnyone: true })) },
      ]);
      const done = await screen.findByRole("dialog", { name: "web is deployed" });
      const link = await within(done).findByRole("link", { name: "app.example.com" });
      expect(link).toHaveAttribute("href", "http://app.example.com");
      expect(link.closest(".plan-reach-row")).toHaveTextContent("Plain HTTP");
      expect(link.closest(".plan-reach-row")).toHaveTextContent(
        "It answers anyone who can reach this server over plain HTTP",
      );
      await user.click(within(done).getByRole("button", { name: "Copy app.example.com" }));
      expect(await navigator.clipboard.readText()).toBe("http://app.example.com");
    });

    describe("when they can't be read", () => {
      const refused = () => {
        throw new ConnectError("the database is away", Code.Unavailable);
      };

      it("says so and reads them again when you ask, rather than saying nothing or that nothing reaches it", async () => {
        const user = userEvent.setup();
        let fails = true;
        const change = applied(aServicePlan());
        const planned = change.plan?.resources[0]?.resource;
        if (planned) planned.scope = "env_1";
        renderSheet(
          { kind: "plan", make: () => Promise.resolve({ change }) },
          {
            services: { listServices: () => ({ services: [aService([])] }) },
            domains: {
              listDomains: () =>
                fails ? refused() : { domains: [aDomain("app.example.com", CertificateState.ISSUED)] },
            },
            changes: { watchChange: feed<WatchChangeResponse>().stream },
          },
        );
        const done = await screen.findByRole("dialog", { name: "web is deployed" });
        expect(await within(done).findByText("Couldn't read the addresses this is reached at")).toBeVisible();
        expect(done).toHaveTextContent("The change itself applied.");
        expect(done).not.toHaveTextContent("Nothing outside");
        await expectAccessible(document.body);
        fails = false;
        await user.click(within(done).getByRole("button", { name: "Try again" }));
        expect(await within(done).findByRole("link", { name: "app.example.com" })).toBeVisible();
        expect(within(done).queryByText("Couldn't read the addresses this is reached at")).toBeNull();
      });

      it("still gives the ports it knows", async () => {
        const change = applied(aServicePlan());
        const planned = change.plan?.resources[0]?.resource;
        if (planned) planned.scope = "env_1";
        renderSheet(
          { kind: "plan", make: () => Promise.resolve({ change }) },
          {
            services: { listServices: () => ({ services: [aService([{ hostPort: 8080, containerPort: 80 }])] }) },
            domains: { listDomains: refused },
            changes: { watchChange: feed<WatchChangeResponse>().stream },
          },
        );
        const done = await screen.findByRole("dialog", { name: "web is deployed" });
        expect(await within(done).findByText("Couldn't read the addresses this is reached at")).toBeVisible();
        expect(await within(done).findByRole("link", { name: /:8080/ })).toBeVisible();
      });

      it("says so of a route it can't name, too, rather than waiting for what won't come", async () => {
        arrivedOnly(aRoutePlan(PlanAction.CREATE), refused);
        expect(await screen.findByText("Couldn't read the addresses this is reached at")).toBeVisible();
      });
    });

    it("says what its certificate is doing until it's there, with nothing to open", async () => {
      arrivedWith([
        aDomain("app.example.com", CertificateState.PENDING, "Waiting for the certificate authority to answer"),
      ]);
      const done = await screen.findByRole("dialog", { name: "web is deployed" });
      const row = (await within(done).findByText("app.example.com")).closest(".plan-reach-row");
      expect(row).toHaveTextContent("Waiting for the certificate authority to answer");
      expect(within(done).queryByRole("link", { name: /app\.example\.com/ })).toBeNull();
      expect(within(done).queryByRole("button", { name: "Copy app.example.com" })).toBeNull();
    });

    it("follows a certificate until it's there, as the sheet may well still be open", async () => {
      const change = applied(aServicePlan());
      const planned = change.plan?.resources[0]?.resource;
      if (planned) planned.scope = "env_1";
      let domains = [aDomain("app.example.com", CertificateState.PENDING, "Waiting for the certificate authority")];
      renderSheet(
        { kind: "plan", make: () => Promise.resolve({ change }) },
        {
          services: { listServices: () => ({ services: [aService([])] }) },
          domains: { listDomains: () => ({ domains }) },
          changes: { watchChange: feed<WatchChangeResponse>().stream },
        },
      );
      const done = await screen.findByRole("dialog", { name: "web is deployed" });
      await within(done).findByText("Waiting for the certificate authority");
      domains = [aDomain("app.example.com", CertificateState.ISSUED)];
      expect(await within(done).findByRole("link", { name: "app.example.com" }, { timeout: 7000 })).toBeVisible();
    }, 15_000);

    it("says why a certificate failed, as the server words it", async () => {
      arrivedWith([aDomain("app.example.com", CertificateState.FAILED, "DNS for app.example.com points elsewhere")]);
      const done = await screen.findByRole("dialog", { name: "web is deployed" });
      const row = (await within(done).findByText("app.example.com")).closest(".plan-reach-row");
      expect(row).toHaveClass("is-failed");
      expect(row).toHaveTextContent("DNS for app.example.com points elsewhere");
    });

    it("leaves out what sends to another app", async () => {
      arrivedWith(
        [aDomain("other.example.com", CertificateState.ISSUED, "", "other")],
        [{ hostPort: 8080, containerPort: 80 }],
      );
      const done = await screen.findByRole("dialog", { name: "web is deployed" });
      await within(done).findByRole("link", { name: /:8080/ });
      expect(done).not.toHaveTextContent("other.example.com");
    });

    const aRoutePlan = (action: PlanAction) => {
      const change = applied(aServicePlan());
      const resource = change.plan?.resources[0];
      const step = change.plan?.servers[0]?.steps[0];
      if (resource?.resource) resource.resource = { ...resource.resource, kind: "route", name: "app-example-com" };
      if (resource) resource.action = action;
      if (step?.resource) step.resource.kind = "route";
      if (step) step.action = action;
      return change;
    };

    const arrivedOnly = (change: Change, listDomains: () => { domains: ReturnType<typeof aDomain>[] }) => {
      const planned = change.plan?.resources[0]?.resource;
      if (planned) planned.scope = "env_1";
      renderSheet(
        { kind: "plan", make: () => Promise.resolve({ change }) },
        {
          services: { listServices: () => ({ services: [aService([])] }) },
          domains: { listDomains },
          changes: { watchChange: feed<WatchChangeResponse>().stream },
        },
      );
    };

    it("says where a change that is nothing but a route went", async () => {
      arrivedWith([aDomain("app.example.com", CertificateState.ISSUED, "", "web")], [], aRoutePlan(PlanAction.CREATE));
      const link = await screen.findByRole("link", { name: "app.example.com" });
      expect(link).toHaveAttribute("href", "https://app.example.com");
    });

    // On the panel's 30 s: where nothing keeps a read, whatever looks next reads again, which hides a missing refresh.
    describe("that the change itself adds", () => {
      const planned = (change: Change) => {
        const resource = change.plan?.resources[0]?.resource;
        if (resource) resource.scope = "env_1";
        return moved(change, { state: ChangeState.PLANNED });
      };
      const aRouteBeside = (change: Change, scope = "env_1") => {
        change.plan?.resources.push(
          create(PlannedResourceSchema, {
            id: "res_route",
            resource: { kind: "route", scope, name: "app-example-com" },
            action: PlanAction.CREATE,
          }),
        );
        return change;
      };

      it("shows it, though what was read at the review came before it", async () => {
        const user = userEvent.setup();
        const change = planned(aRoutePlan(PlanAction.CREATE));
        let domains: ReturnType<typeof aDomain>[] = [];
        let reads = 0;
        renderSheet(
          { kind: "plan", make: () => Promise.resolve({ change }) },
          {
            services: { listServices: () => ({ services: [aService([])] }) },
            domains: {
              listDomains: () => {
                reads += 1;
                return { domains };
              },
            },
            changes: {
              watchChange: feed<WatchChangeResponse>().stream,
              applyChange: () => {
                domains = [aDomain("app.example.com", CertificateState.ISSUED)];
                return { change: moved(change, { state: ChangeState.APPLIED }) };
              },
            },
          },
          { staleTime: 30_000 },
        );
        const review = await screen.findByRole("dialog", { name: "Review plan" });
        await waitFor(() => expect(reads).toBe(1));
        await user.click(within(review).getByRole("button", { name: /Apply plan/ }));
        expect(await screen.findByRole("link", { name: "app.example.com" })).toHaveAttribute(
          "href",
          "https://app.example.com",
        );
      });

      it("doesn't say nothing reaches the app while what it read is from before", async () => {
        const user = userEvent.setup();
        const change = planned(aRouteBeside(aServicePlan()));
        let answer: (domains: { domains: ReturnType<typeof aDomain>[] }) => void = () => {};
        let reads = 0;
        renderSheet(
          { kind: "plan", make: () => Promise.resolve({ change }) },
          {
            services: { listServices: () => ({ services: [aService([])] }) },
            domains: {
              listDomains: () => {
                reads += 1;
                if (reads === 1) return { domains: [] };
                return new Promise((resolve) => {
                  answer = resolve;
                });
              },
            },
            changes: {
              watchChange: feed<WatchChangeResponse>().stream,
              applyChange: () => ({ change: moved(change, { state: ChangeState.APPLIED }) }),
            },
          },
          { staleTime: 30_000 },
        );
        const review = await screen.findByRole("dialog", { name: "Review plan" });
        await waitFor(() => expect(reads).toBe(1));
        await user.click(within(review).getByRole("button", { name: /Apply plan/ }));
        const done = await screen.findByRole("dialog", { name: "web is deployed" });
        await waitFor(() => expect(reads).toBe(2));
        expect(done).not.toHaveTextContent("Nothing outside");
        act(() => answer({ domains: [aDomain("app.example.com", CertificateState.ISSUED)] }));
        expect(await within(done).findByRole("link", { name: "app.example.com" })).toBeVisible();
        expect(done).not.toHaveTextContent("Nothing outside");
      });

      it("reads each environment a plan spans, for what's in that one", async () => {
        const change = applied(aRouteBeside(aServicePlan(), "env_2"));
        const planned = change.plan?.resources[0]?.resource;
        if (planned) planned.scope = "env_1";
        const asked: string[] = [];
        renderSheet(
          { kind: "plan", make: () => Promise.resolve({ change }) },
          {
            services: { listServices: () => ({ services: [aService([{ hostPort: 8080, containerPort: 80 }])] }) },
            domains: {
              listDomains: (req) => {
                asked.push(req.environment);
                return req.environment === "env_2"
                  ? {
                      domains: [
                        aDomain("app.example.com", CertificateState.ISSUED),
                        // A service of this name there isn't the one this plan changes here, and neither is this route.
                        aDomain("elsewhere.example.com", CertificateState.ISSUED, "", "web"),
                      ].map((domain, i) =>
                        i === 0 ? domain : { ...domain, routes: [{ ...domain.routes[0], name: "other" }] },
                      ),
                    }
                  : { domains: [aDomain("web.example.com", CertificateState.ISSUED)] };
              },
            },
            changes: { watchChange: feed<WatchChangeResponse>().stream },
          },
        );
        const done = await screen.findByRole("dialog", { name: "web is deployed" });
        expect(await within(done).findByRole("link", { name: "app.example.com" })).toBeVisible();
        expect(within(done).getByRole("link", { name: "web.example.com" })).toBeVisible();
        expect(within(done).getByRole("link", { name: /:8080/ })).toBeVisible();
        expect(within(done).queryByRole("link", { name: "elsewhere.example.com" })).toBeNull();
        expect(asked).toEqual(expect.arrayContaining(["env_1", "env_2"]));
      });

      it("names it at once, from the plan, rather than leaving a gap until it's read", async () => {
        const user = userEvent.setup();
        const route = create(PlannedResourceSchema, {
          id: "res_route",
          resource: { kind: "route", scope: "env_1", name: "app-example-com" },
          action: PlanAction.CREATE,
          changes: [{ path: "domain", op: FieldOp.ADDED, to: "app.example.com" }],
        });
        const change = planned(aServicePlan());
        change.plan?.resources.push(route);
        let reads = 0;
        renderSheet(
          { kind: "plan", make: () => Promise.resolve({ change }) },
          {
            services: { listServices: () => ({ services: [aService([])] }) },
            domains: {
              listDomains: () => {
                reads += 1;
                return reads === 1 ? { domains: [] } : new Promise(() => {});
              },
            },
            changes: {
              watchChange: feed<WatchChangeResponse>().stream,
              applyChange: () => ({ change: moved(change, { state: ChangeState.APPLIED }) }),
            },
          },
          { staleTime: 30_000 },
        );
        const review = await screen.findByRole("dialog", { name: "Review plan" });
        await waitFor(() => expect(reads).toBe(1));
        // The room it'll take is there from the review on, with nothing to read or find in it.
        const held = review.querySelector(".plan-room-held");
        expect(held).toHaveAttribute("aria-hidden", "true");
        expect(held?.querySelector(".is-domain [data-words='app.example.com']")).not.toBeNull();
        expect(held?.textContent).toBe("");
        expect(held?.querySelector(".plan-reach-how")).toBeNull();
        await user.click(within(review).getByRole("button", { name: /Apply plan/ }));
        const done = await screen.findByRole("dialog", { name: "web is deployed" });
        const name = await within(done).findByText("app.example.com", { selector: ".plan-reach-at" });
        expect(name.closest(".plan-reach-row")).toHaveTextContent("Waiting for the server");
        expect(done.querySelector(".plan-room-held")).toBeNull();
      });
    });

    it("doesn't say a tunnel's domain is getting a certificate, and lets you go, if its server never reports", async () => {
      const user = userEvent.setup();
      const silent = {
        name: "app.example.com",
        url: "http://app.example.com",
        routes: [{ id: "res_route", name: "app-example-com", service: "web", port: 80, tls: "off" }],
        servers: [{ serverName: "hz-1", stale: false }],
      };
      const change = applied(aServicePlan());
      const planned = change.plan?.resources[0]?.resource;
      if (planned) planned.scope = "env_1";
      renderSheet(
        { kind: "plan", make: () => Promise.resolve({ change }) },
        {
          services: { listServices: () => ({ services: [aService([])] }) },
          domains: { listDomains: () => ({ domains: [silent] }) },
          changes: { watchChange: feed<WatchChangeResponse>().stream },
        },
      );
      const done = await screen.findByRole("dialog", { name: "web is deployed" });
      const name = await within(done).findByText("app.example.com");
      const row = name.closest(".plan-reach-row");
      expect(row).toHaveTextContent("Waiting for the server");
      expect(row).not.toHaveTextContent("certificate");
      expect(within(done).queryByRole("link", { name: /app\.example\.com/ })).toBeNull();
      await user.click(within(done).getByRole("button", { name: /Done/ }));
      await waitFor(() => expect(screen.queryByRole("dialog", { name: "web is deployed" })).toBeNull());
    });

    describe("behind a tunnel or a proxy, says what to do next", () => {
      const aTunnelPlan = (proxies: string[], extra: { path: string; to: string }[] = []) => {
        const change = applied(aServicePlan());
        const resource = change.plan?.resources[0];
        if (resource?.resource)
          resource.resource = { ...resource.resource, kind: "route", scope: "env_1", name: "app-example-com" };
        if (resource) {
          resource.action = PlanAction.CREATE;
          resource.changes = [
            { path: "domain", to: "app.example.com" },
            { path: "tls", to: "off" },
            ...proxies.map((to, i) => ({ path: `trusted_proxies[${i}]`, to })),
            ...extra,
          ].map((c) => create(FieldChangeSchema, { ...c, op: FieldOp.ADDED }));
        }
        const step = change.plan?.servers[0]?.steps[0];
        if (step?.resource) step.resource.kind = "route";
        return change;
      };
      const arrived = async (change: Change) => {
        renderSheet(
          { kind: "plan", make: () => Promise.resolve({ change }) },
          {
            services: { listServices: () => ({ services: [aService([])] }) },
            domains: {
              listDomains: () => ({
                domains: [aDomain("app.example.com", CertificateState.NONE, "", "web", "http://app.example.com")],
              }),
            },
            changes: { watchChange: feed<WatchChangeResponse>().stream },
          },
        );
        const row = (await screen.findByRole("link", { name: "app.example.com" })).closest(".plan-reach-row");
        return row as HTMLElement;
      };

      it("a tunnel: where to point it, what fails, and the guide", async () => {
        // As the server writes it, which is what a plan has.
        const row = await arrived(aTunnelPlan(["127.0.0.1/32"]));
        expect(row).toHaveTextContent(
          /point your tunnel's service at http:\/\/127\.0\.0\.1:80\. https:\/\/ there fails/,
        );
        expect(within(row).getByRole("link", { name: "See the guide" })).toHaveAttribute(
          "href",
          expect.stringContaining("docs/install.md#behind-cloudflare-tunnel"),
        );
      });

      it("Cloudflare's proxy: Flexible, and that a tunnel is better", async () => {
        const row = await arrived(aTunnelPlan(["cloudflare"]));
        expect(row).toHaveTextContent(/Flexible in Cloudflare.*open internet unencrypted.*a tunnel is the better way/);
      });

      it("another proxy: port 80, over http://", async () => {
        const row = await arrived(aTunnelPlan(["172.20.0.0/16"]));
        expect(row).toHaveTextContent(/point what's in front at port 80 on this server, over http:\/\//);
      });

      it("nothing, for one that answers anyone, which works as it is", async () => {
        const row = await arrived(aTunnelPlan([], [{ path: "plain_for_anyone", to: "true" }]));
        expect(row).not.toHaveTextContent("Next,");
      });
    });

    it("leads with the domain someone typed, not the name made from it", async () => {
      const change = aRoutePlan(PlanAction.CREATE);
      const resource = change.plan?.resources[0];
      if (resource)
        resource.changes = [create(FieldChangeSchema, { path: "domain", op: FieldOp.ADDED, to: "app.example.com" })];
      arrivedOnly(change, () => ({ domains: [] }));
      expect(await screen.findByRole("dialog", { name: /app\.example\.com/ })).toBeVisible();
      expect(screen.queryByRole("heading", { name: /app-example-com/ })).toBeNull();
    });

    it("names a route by its domain in what it warns of, too", async () => {
      const change = moved(aRoutePlan(PlanAction.CREATE), { state: ChangeState.PLANNED });
      const resource = change.plan?.resources[0];
      const access = create(HostAccessSchema, { kind: HostAccessKind.TRUSTED_PROXY, path: "127.0.0.1/32" });
      if (resource) {
        resource.changes = [create(FieldChangeSchema, { path: "domain", op: FieldOp.ADDED, to: "app.example.com" })];
        resource.hostAccess = [access];
      }
      const step = change.plan?.servers[0]?.steps[0];
      if (step) step.hostAccess = [access];
      renderSheet(
        { kind: "plan", make: () => Promise.resolve({ change }) },
        { changes: { watchChange: feed<WatchChangeResponse>().stream } },
      );
      const review = await screen.findByRole("dialog", { name: "Review plan" });
      expect(review).toHaveTextContent("app.example.com believes the proxy 127.0.0.1/32");
      expect(review).not.toHaveTextContent("app-example-com");
    });

    it("says nothing of what DNS said once the domain serves", async () => {
      const change = applied(aServicePlan());
      const step = change.plan?.servers[0]?.steps[0];
      if (step) {
        step.domainCheck = create(DomainCheckSchema, { domain: "app.example.com", state: DomainCheckState.NO_RECORD });
      }
      arrivedWith([aDomain("app.example.com", CertificateState.ISSUED)], [], change);
      const done = await screen.findByRole("dialog", { name: "web is deployed" });
      await within(done).findByRole("link", { name: "app.example.com" });
      await waitFor(() => expect(done).not.toHaveTextContent("its certificate can't come"));
    });

    it("keeps saying what DNS said while it hasn't served", async () => {
      const change = applied(aServicePlan());
      const step = change.plan?.servers[0]?.steps[0];
      if (step) {
        step.domainCheck = create(DomainCheckSchema, { domain: "app.example.com", state: DomainCheckState.NO_RECORD });
      }
      arrivedWith([aDomain("app.example.com", CertificateState.PENDING, "Asking")], [], change);
      const done = await screen.findByRole("dialog", { name: "web is deployed" });
      await within(done).findByText("Asking");
      expect(done).toHaveTextContent("its certificate can't come");
    });

    it("says plainly which app a domain leaves, and for which", async () => {
      const change = moved(aRoutePlan(PlanAction.UPDATE), { state: ChangeState.PLANNED });
      const step = change.plan?.servers[0]?.steps[0];
      if (step) step.moves = create(RouteMoveSchema, { where: "app.example.com", from: "api", to: "web" });
      renderSheet(
        { kind: "plan", make: () => Promise.resolve({ change }) },
        { changes: { watchChange: feed<WatchChangeResponse>().stream } },
      );
      const review = await screen.findByRole("dialog", { name: "Review plan" });
      expect(review).toHaveTextContent("app.example.com leaves api, and goes to web.");
    });

    it("says it goes to a port, when that's where it goes", async () => {
      const change = moved(aRoutePlan(PlanAction.UPDATE), { state: ChangeState.PLANNED });
      const step = change.plan?.servers[0]?.steps[0];
      if (step) step.moves = create(RouteMoveSchema, { where: "app.example.com", from: "api", toPort: 8080 });
      renderSheet(
        { kind: "plan", make: () => Promise.resolve({ change }) },
        { changes: { watchChange: feed<WatchChangeResponse>().stream } },
      );
      const review = await screen.findByRole("dialog", { name: "Review plan" });
      expect(review).toHaveTextContent("app.example.com leaves api, and goes to port 8080 on the server.");
    });

    describe("with many of them", () => {
      const many = (n: number, broken?: number) =>
        Array.from({ length: n }, (_, i) =>
          aDomain(
            `site-${i + 1}.example.com`,
            i === broken ? CertificateState.FAILED : CertificateState.ISSUED,
            i === broken ? "DNS points elsewhere" : "",
          ),
        );

      it("folds them after a few, so the plan is still there to see, and opens them when asked", async () => {
        const user = userEvent.setup();
        arrivedWith(many(14), [{ hostPort: 8080, containerPort: 80 }]);
        const done = await screen.findByRole("dialog", { name: "web is deployed" });
        await within(done).findByRole("link", { name: "site-1.example.com" });
        expect(done.querySelectorAll(".plan-reach-row.is-domain")).toHaveLength(3);
        const more = within(done).getByRole("button", { name: "and 12 more" });
        await expectAccessible(document.body);
        await user.click(more);
        expect(done.querySelectorAll(".plan-reach-row.is-domain")).toHaveLength(14);
        expect(within(done).getByRole("link", { name: /:8080/ })).toBeVisible();
        expect(within(done).queryByRole("button", { name: /more$/ })).toBeNull();
      });

      it("doesn't fold what fits", async () => {
        arrivedWith(many(3));
        const done = await screen.findByRole("dialog", { name: "web is deployed" });
        await within(done).findByRole("link", { name: "site-3.example.com" });
        expect(within(done).queryByRole("button", { name: /more$/ })).toBeNull();
      });

      it("leads with what's wrong, so folding doesn't hide it", async () => {
        arrivedWith(many(14, 11));
        const done = await screen.findByRole("dialog", { name: "web is deployed" });
        const first = await within(done).findByText("site-12.example.com", { selector: ".plan-reach-at" });
        expect(done.querySelector(".plan-reach-row")).toContainElement(first);
      });

      it("leads with what the change itself makes, ahead of what's wrong", async () => {
        const change = applied(aServicePlan());
        const planned = change.plan?.resources[0]?.resource;
        if (planned) planned.scope = "env_1";
        change.plan?.resources.push(
          create(PlannedResourceSchema, {
            id: "res_route",
            resource: { kind: "route", scope: "env_1", name: "site-14-example-com" },
            action: PlanAction.CREATE,
            changes: [{ path: "domain", op: FieldOp.ADDED, to: "site-14.example.com" }],
          }),
        );
        arrivedWith(many(14, 0), [], change);
        const done = await screen.findByRole("dialog", { name: "web is deployed" });
        const first = await within(done).findByText("site-14.example.com", {
          selector: "a.plan-reach-at code, code.plan-reach-at",
        });
        expect(done.querySelector(".plan-reach-row")).toContainElement(first);
        expect(done.querySelectorAll(".plan-reach-row")[1]).toHaveTextContent("site-1.example.com");
      });

      it("holds room at the review for what will show, and the line for the rest", async () => {
        const change = aServicePlan();
        const planned = change.plan?.resources[0]?.resource;
        if (planned) planned.scope = "env_1";
        renderSheet(
          { kind: "plan", make: () => Promise.resolve({ change }) },
          {
            services: { listServices: () => ({ services: [aService([])] }) },
            domains: { listDomains: () => ({ domains: many(14) }) },
            changes: { watchChange: feed<WatchChangeResponse>().stream },
          },
        );
        const review = await screen.findByRole("dialog", { name: "Review plan" });
        await waitFor(() => expect(review.querySelectorAll(".plan-room-held .is-domain")).toHaveLength(3));
        expect(review.querySelectorAll(".plan-room-held > .plan-reach-row:not(.is-domain)")).toHaveLength(1);
      });
    });

    describe("as a route goes", () => {
      const going = (find: VisitorsFind, where = "app.example.com", domain = "app.example.com") => {
        const change = moved(aRoutePlan(PlanAction.DELETE), { state: ChangeState.PLANNED });
        const resource = change.plan?.resources[0];
        if (resource)
          resource.changes = [create(FieldChangeSchema, { path: "domain", op: FieldOp.REMOVED, from: domain })];
        const step = change.plan?.servers[0]?.steps[0];
        if (step) step.goes = create(RouteGoesSchema, { where, domain, find });
        return change;
      };
      it("says how many addresses go, when several do", async () => {
        const change = going(VisitorsFind.REFUSED);
        const steps = change.plan?.servers[0]?.steps;
        const first = steps?.[0];
        if (first)
          steps?.push(
            create(PlanStepSchema, {
              ...first,
              resourceId: "res_other",
              goes: create(RouteGoesSchema, {
                where: "b.example.com",
                domain: "b.example.com",
                find: VisitorsFind.REFUSED,
              }),
            }),
          );
        renderSheet(
          { kind: "plan", make: () => Promise.resolve({ change }) },
          { changes: { watchChange: feed<WatchChangeResponse>().stream } },
        );
        const review = await screen.findByRole("dialog", { name: "Review plan" });
        expect(review).toHaveTextContent("visitors lose 2 addresses");
        expect(review).not.toHaveTextContent("no downtime expected");
      });

      it.each([
        [VisitorsFind.REFUSED, /gets a refused connection/],
        [VisitorsFind.TLS_ERROR, /gets a browser's connection error/],
        [VisitorsFind.NOT_FOUND, /What's in front gets a 404/],
        [VisitorsFind.REST, /gets what the rest of app\.example\.com serves/],
        [VisitorsFind.NOTHING_BEHIND, /What's in front can't connect for app\.example\.com/],
        [VisitorsFind.UNSPECIFIED, /stops reaching it/],
      ])("says what a visitor finds, and never that nothing goes down: %s", async (find, words) => {
        const change = going(find);
        renderSheet(
          { kind: "plan", make: () => Promise.resolve({ change }) },
          { changes: { watchChange: feed<WatchChangeResponse>().stream } },
        );
        const review = await screen.findByRole("dialog", { name: "Review plan" });
        expect(review).toHaveTextContent(words);
        expect(review).toHaveTextContent("visitors lose app.example.com");
        expect(review).not.toHaveTextContent("no downtime expected");
        // The button says what someone typed, too.
        expect(within(review).getByRole("button", { name: /^Delete app\.example\.com/ })).toBeVisible();
      });
    });

    it("says nothing of a route the change took away", async () => {
      arrivedWith([aDomain("app.example.com", CertificateState.ISSUED, "", "web")], [], aRoutePlan(PlanAction.DELETE));
      await screen.findByRole("dialog");
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(screen.queryByRole("link", { name: "app.example.com" })).toBeNull();
    });

    it("waits to say nothing reaches it until it has read the domains", async () => {
      const change = applied(aServicePlan());
      const planned = change.plan?.resources[0]?.resource;
      if (planned) planned.scope = "env_1";
      renderSheet(
        { kind: "plan", make: () => Promise.resolve({ change }) },
        {
          services: { listServices: () => ({ services: [aService([])] }) },
          domains: { listDomains: () => new Promise(() => {}) },
          changes: { watchChange: feed<WatchChangeResponse>().stream },
        },
      );
      const done = await screen.findByRole("dialog", { name: "web is deployed" });
      await new Promise((resolve) => setTimeout(resolve, 50));
      expect(done).not.toHaveTextContent("Nothing outside");
    });
  });

  it("says at once that a new app with no port can't be reached, before the list is read again", async () => {
    const change = applied(aServicePlan());
    const [resource] = change.plan?.resources ?? [];
    const step = change.plan?.servers[0]?.steps[0];
    if (!resource || !step) throw new Error("no step");
    resource.action = PlanAction.CREATE;
    step.action = PlanAction.CREATE;
    resource.changes = [create(FieldChangeSchema, { path: "app.image", op: FieldOp.ADDED, to: "nginx:1.28" })];
    renderSheet(
      { kind: "plan", make: () => Promise.resolve({ change }) },
      {
        services: { listServices: () => new Promise(() => {}) },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      },
    );
    const done = await screen.findByRole("dialog", { name: "web is deployed" });
    expect(await within(done).findByRole("button", { name: "Add a domain" })).toBeVisible();
  });

  it("waits for the list after a change to its ports, rather than saying nothing reaches what just got one", async () => {
    const change = applied(aServicePlan());
    const resource = change.plan?.resources[0];
    if (resource)
      resource.changes = [create(FieldChangeSchema, { path: "app.ports[0]", op: FieldOp.ADDED, to: "8080:80" })];
    let answer: (list: { services: MessageInitShape<typeof ServiceInfoSchema>[] }) => void = () => {};
    let reads = 0;
    renderSheet(
      { kind: "plan", make: () => Promise.resolve({ change }) },
      {
        services: {
          listServices: () => {
            reads += 1;
            if (reads === 1) return { services: [aService([])] };
            return new Promise((resolve) => {
              answer = resolve;
            });
          },
        },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      },
      { staleTime: 30_000 },
    );
    const done = await screen.findByRole("dialog", { name: "web is deployed" });
    await waitFor(() => expect(reads).toBeGreaterThanOrEqual(2));
    expect(done).not.toHaveTextContent("Nothing outside");
    expect(within(done).queryByRole("button", { name: "Add a port" })).toBeNull();
    act(() => answer({ services: [aService([{ hostPort: 8080, containerPort: 80 }])] }));
    expect(await within(done).findByRole("link", { name: /:8080/ })).toBeVisible();
  });

  describe("holds room for what its arrival will say", () => {
    it("a line for what a server says of a domain, which it knows already", async () => {
      const change = aServicePlan();
      const planned = change.plan?.resources[0]?.resource;
      if (planned) planned.scope = "env_1";
      renderSheet(
        { kind: "plan", make: () => Promise.resolve({ change }) },
        {
          services: { listServices: () => ({ services: [aService([])] }) },
          domains: {
            listDomains: () => ({
              domains: [
                {
                  name: "app.example.com",
                  url: "https://app.example.com",
                  routes: [{ id: "rte_1", name: "app", service: "web", port: 80 }],
                  servers: [
                    {
                      serverName: "hz-1",
                      status: { certificate: { state: CertificateState.PENDING, message: "Asking for it" } },
                    },
                  ],
                },
              ],
            }),
          },
          changes: { watchChange: feed<WatchChangeResponse>().stream },
        },
      );
      const review = await screen.findByRole("dialog", { name: "Review plan" });
      await waitFor(() =>
        expect(review.querySelector(".plan-room-held .plan-reach-how")).toHaveAttribute("data-words", "Asking for it"),
      );
    });

    const heldAtReview = async (change: Change, ports: MessageInitShape<typeof PortMappingSchema>[]) => {
      renderSheet(
        { kind: "plan", make: () => Promise.resolve({ change }) },
        {
          services: { listServices: () => ({ services: [aService(ports)] }) },
          changes: { watchChange: feed<WatchChangeResponse>().stream },
        },
      );
      const review = await screen.findByRole("dialog", { name: "Review plan" });
      return () => review.querySelector(".plan-room-held");
    };

    it("a row for each port the app has", async () => {
      const held = await heldAtReview(aServicePlan(), [
        { hostPort: 8080, containerPort: 80 },
        { hostPort: 8443, containerPort: 443 },
      ]);
      await waitFor(() => expect(held()?.querySelectorAll(".plan-reach-row")).toHaveLength(2));
    });

    it("and for one the plan adds", async () => {
      const change = aServicePlan();
      const resource = change.plan?.resources[0];
      if (resource)
        resource.changes = [create(FieldChangeSchema, { path: "app.ports[0]", op: FieldOp.ADDED, to: "8080:80" })];
      const held = await heldAtReview(change, []);
      await waitFor(() => expect(held()?.querySelectorAll(".plan-reach-row")).toHaveLength(1));
      expect(held()?.querySelector(".plan-unreached")).toBeNull();
    });

    it("and doesn't count a port twice, once the list has what the plan adds", async () => {
      const change = aServicePlan();
      const resource = change.plan?.resources[0];
      if (resource) {
        resource.baseRevision = 2n;
        resource.changes = [create(FieldChangeSchema, { path: "app.ports[0]", op: FieldOp.ADDED, to: "8080:80" })];
      }
      renderSheet(
        { kind: "plan", make: () => Promise.resolve({ change }) },
        {
          services: {
            listServices: () => ({
              services: [{ ...aService([{ hostPort: 8080, containerPort: 80 }]), revision: 3n }],
            }),
          },
          changes: { watchChange: feed<WatchChangeResponse>().stream },
        },
      );
      const review = await screen.findByRole("dialog", { name: "Review plan" });
      await waitFor(() => expect(review.querySelectorAll(".plan-room-held .plan-reach-row")).toHaveLength(1));
    });

    it("and none for one the plan takes away, which leaves saying nothing reaches it", async () => {
      const change = aServicePlan();
      const resource = change.plan?.resources[0];
      if (resource)
        resource.changes = [create(FieldChangeSchema, { path: "app.ports[0]", op: FieldOp.REMOVED, from: "8080:80" })];
      const held = await heldAtReview(change, [{ hostPort: 8080, containerPort: 80 }]);
      await waitFor(() => expect(held()?.querySelector(".plan-unreached")).not.toBeNull());
      expect(held()?.querySelector("[data-words]")).toHaveAttribute(
        "data-words",
        "Nothing outside hz-1 reaches web yet.",
      );
    });
  });

  it("doesn't call a change on record live, as it may not be any more", async () => {
    renderSheet(
      { kind: "change", id: "chg_web" },
      {
        changes: {
          getChange: () => ({ change: applied(aServicePlan()) }),
          watchChange: feed<WatchChangeResponse>().stream,
        },
      },
    );
    const record = await screen.findByRole("dialog", { name: "Applied" });
    expect(record).toHaveTextContent(/Applied .* by Mira Chen/);
    expect(record).toHaveTextContent("1 replaced");
  });

  it("deleting says what it does: it stops, for good, with Delete all the way and the danger button", async () => {
    const user = userEvent.setup();
    const deleting = aServicePlan();
    const plan = deleting.plan;
    const resource = plan?.resources[0];
    const server = plan?.servers[0];
    if (!plan || !resource || !server?.steps[0]) throw new Error("no step");
    plan.summary = create(PlanSummarySchema, { destroy: 1 });
    resource.action = PlanAction.DELETE;
    server.steps[0].action = PlanAction.DELETE;
    server.steps[0].downtime = undefined;
    server.downtime = undefined;
    deleting.summary = "Delete web";
    const watch = feed<WatchChangeResponse>();
    renderSheet(
      { kind: "plan", make: () => Promise.resolve({ change: deleting }) },
      { changes: { watchChange: watch.stream, applyChange: () => ({ change: deleting }) } },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    await within(sheet).findByText("to delete");
    // Said once, and in agreement with the footer beside it.
    expect(sheet.textContent?.match(/stops/g)).toHaveLength(1);
    expect(sheet).toHaveTextContent(/hz-1 · web stops, and you can undo this until \d+ \w+\./);
    expect(sheet).toHaveTextContent("Undo for 7 days");
    expect(sheet).not.toHaveTextContent("doesn't come back");
    expect(sheet).not.toHaveTextContent(/no downtime/i);
    expect(sheet).not.toHaveTextContent(/destroy/i);
    expect(sheet).toHaveTextContent("Delete");
    const apply = within(sheet).getByRole("button", { name: /Delete web/ });
    expect(apply).toHaveClass("dw-btn-danger");
    await user.click(apply);
    act(() => watch.push(watched(applied(deleting))));
    const done = await screen.findByRole("dialog", { name: "web is deleted" });
    expect(done).toHaveTextContent("1 deleted");
    expect(done).toHaveTextContent("Deleted");
    expect(done).not.toHaveTextContent(/destroy/i);
  });

  it("puts a failure in plain words, with what the server said only behind Details, and a way back", async () => {
    const user = userEvent.setup();
    const raw =
      "hz-1: can't create service web in env_01M6MCCCXQW6G67PMQDK7PVYAY: can't pull acme/web:1.0: pull access denied";
    const plain =
      "acme/web:1.0 can't be pulled. The name may be wrong, or it may be private, which Dockwell can't pull yet. Check the name, then try again.";
    const failed = moved(aServicePlan(), {
      state: ChangeState.FAILED,
      error: plain,
      rolledBackBy: "chg_back",
      steps: [aStep({ state: StepState.FAILED, message: plain, detail: raw })],
    });
    const closed = vi.fn();
    renderSheet(
      {
        kind: "plan",
        make: () => Promise.resolve({ change: failed }),
        retryLabel: "Edit and try again",
        onClose: closed,
      },
      { changes: { watchChange: feed<WatchChangeResponse>().stream } },
    );
    const sheet = await screen.findByRole("dialog", { name: "Couldn't replace web" });
    const notice = within(sheet).getByRole("alert");
    expect(notice.querySelector(".dw-callout-title")).toHaveTextContent("acme/web:1.0 can't be pulled");
    expect(notice).toHaveTextContent("Check the name, then try again.");
    // Said once, with no IDs on show, and nothing done yet to roll back.
    expect(sheet.textContent?.match(/can't be pulled/g)).toHaveLength(1);
    expect(notice).not.toHaveTextContent("Dockwell rolled back");
    const details = notice.querySelector("details") as HTMLDetailsElement;
    expect(details.open).toBe(false);
    expect(details).toHaveTextContent(raw);
    expect(within(sheet).getByText("What it set out to do")).toBeVisible();
    expect(sheet).not.toHaveTextContent("to replace");
    const back = within(sheet).getByRole("button", { name: /Edit and try again/ });
    expect(back).toHaveClass("dw-btn-primary");
    await expectAccessible(document.body);
    await user.click(back);
    expect(closed).toHaveBeenCalledOnce();
  });

  it("goes through with the keyboard alone: Apply, then whatever comes next, with focus there", async () => {
    const user = userEvent.setup();
    const watch = feed<WatchChangeResponse>();
    const planned = aServicePlan();
    const applies = vi.fn(() => ({ change: moved(planned, { state: ChangeState.APPLYING }) }));
    const closed = vi.fn();
    renderSheet(
      {
        kind: "plan",
        make: () => Promise.resolve({ change: planned }),
        doneLabel: "Open web",
        onClose: closed,
      },
      { changes: { watchChange: watch.stream, applyChange: applies } },
    );
    await screen.findByRole("button", { name: /Apply plan/ });
    // A key held down from the step before never applies a plan unseen.
    fireEvent.keyDown(window, { key: "Enter", ctrlKey: true, repeat: true });
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(applies).not.toHaveBeenCalled();
    await user.keyboard("{Control>}{Enter}{/Control}");
    await waitFor(() => expect(applies).toHaveBeenCalledOnce());
    const heading = await screen.findByRole("heading", { name: "Applying" });
    // The button it was on has gone, and focus stays in the sheet, so Escape and Tab still work.
    await waitFor(() => expect(heading).toHaveFocus());
    act(() => watch.push(watched(applied(planned))));
    const open = await screen.findByRole("button", { name: /Open web/ });
    await waitFor(() => expect(open).toHaveFocus());
    await user.keyboard("{Control>}{Enter}{/Control}");
    expect(closed).toHaveBeenCalledOnce();
  });

  it("offers an undo in the same sheet, reads like any plan, and Cancel goes back to what it would undo", async () => {
    const user = userEvent.setup();
    const change = applied(aServicePlan());
    const undoing = moved(aServicePlan(), { id: "chg_undo", summary: "Undo: Update web", reverts: "chg_web" });
    const dialogs = () => document.querySelectorAll(".dw-sheet");
    renderSheet(
      { kind: "plan", make: () => Promise.resolve({ change }) },
      {
        services: { listServices: () => ({ services: [aService([])] }) },
        changes: {
          watchChange: feed<WatchChangeResponse>().stream,
          undoChange: () => ({ change: undoing }),
          discardChange: () => ({}),
          getChange: () => ({ change }),
        },
      },
    );
    const done = await screen.findByRole("dialog", { name: "web is deployed" });
    const shell = dialogs()[0];
    await user.click(within(done).getByRole("button", { name: /Undo until/ }));
    const plan = await screen.findByRole("dialog", { name: "Review plan" });
    expect(plan).toHaveAccessibleDescription("Undo: Update web");
    await within(plan).findByText("to replace");
    expect(within(plan).getByRole("button", { name: /Apply plan/ })).toBeVisible();
    // One shell, one scrim.
    expect(dialogs()).toHaveLength(1);
    expect(dialogs()[0]).toBe(shell);
    expect(document.querySelectorAll(".dw-scrim")).toHaveLength(1);
    await user.click(within(plan).getByRole("button", { name: "Cancel" }));
    expect(await screen.findByRole("dialog", { name: "web is deployed" })).toBeVisible();
    expect(dialogs()[0]).toBe(shell);
  });

  it("has nothing to apply when a plan has nothing to do, and says so", async () => {
    const user = userEvent.setup();
    const nothing = webPlanned();
    if (nothing.plan) {
      nothing.plan.resources = [];
      nothing.plan.servers = [];
      nothing.plan.summary = create(PlanSummarySchema, {});
    }
    const discard = vi.fn(() => ({}));
    renderSheet(
      { kind: "plan", make: () => Promise.resolve({ change: nothing }) },
      { changes: { watchChange: feed<WatchChangeResponse>().stream, discardChange: discard } },
    );
    const sheet = await screen.findByRole("dialog", { name: "Nothing to change" });
    expect(sheet).toHaveTextContent("there is nothing to apply");
    expect(within(sheet).queryByRole("button", { name: /Apply/ })).toBeNull();
    expect(sheet).not.toHaveTextContent("to create");
    expect(sheet).not.toHaveTextContent("Undo");
    await expectAccessible(document.body);
    await user.click(within(sheet).getAllByRole("button", { name: "Close" }).at(-1) as HTMLElement);
    await waitFor(() => expect(discard).toHaveBeenCalledOnce());
  });

  it("can say there is nothing to do without a plan at all, in the words of whoever knows why", async () => {
    renderSheet(
      {
        kind: "nothing",
        summary: "Roll back web to r1",
        detail: "web already runs r1, so there is nothing to roll back.",
      },
      {},
    );
    const sheet = await screen.findByRole("dialog", { name: "Nothing to change" });
    expect(sheet).toHaveAccessibleDescription("Roll back web to r1");
    expect(sheet).toHaveTextContent("web already runs r1, so there is nothing to roll back.");
    expect(within(sheet).queryByRole("button", { name: /Apply/ })).toBeNull();
  });

  it("shows a fleet's steps by server, and how they run, once there is a fleet", async () => {
    const fleet = aServicePlan();
    const first = fleet.plan?.servers[0];
    if (!fleet.plan || !first) throw new Error("no server");
    fleet.plan.servers = [
      first,
      create(ServerPlanSchema, { ...first, serverId: "srv_2", serverName: "hz-2", offline: true }),
    ];
    renderSheet(
      { kind: "plan", make: () => Promise.resolve({ change: fleet }) },
      { changes: { watchChange: feed<WatchChangeResponse>().stream } },
    );
    const sheet = await screen.findByRole("dialog", { name: "Review plan" });
    const steps = await within(sheet).findByRole("region", { name: "Steps by server" });
    expect(steps).toHaveTextContent("hz-1");
    expect(steps).toHaveTextContent("hz-2");
    expect(steps).toHaveTextContent("Replace web app");
    expect(sheet).toHaveTextContent("Steps run one at a time on each server, servers side by side.");
    // Apply says why it can't yet, right beside it.
    expect(within(sheet).getByRole("button", { name: /Apply plan/ })).toBeDisabled();
    expect(sheet.querySelector(".dw-sheet-foot")).toHaveTextContent("hz-2 has to reconnect first");
  });

  it("reads in Chinese all the way to the arrival", async () => {
    await activateLocale("zh-CN");
    renderSheet(
      { kind: "plan", make: () => Promise.resolve({ change: applied(aServicePlan()) }) },
      {
        services: { listServices: () => ({ services: [aService([])] }) },
        changes: { watchChange: feed<WatchChangeResponse>().stream },
      },
    );
    const done = await screen.findByRole("dialog", { name: "web 已部署" });
    expect(done).toHaveTextContent("1 已替换");
    await within(done).findByText(/添加端口/);
    await expectAccessible(document.body);
  });
});
