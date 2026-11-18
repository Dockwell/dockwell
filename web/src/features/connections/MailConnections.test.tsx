// SPDX-License-Identifier: AGPL-3.0-only

import { create, type MessageInitShape } from "@bufbuild/protobuf";
import {
  ChangeSchema,
  ChangeState,
  type CheckConnectionRequest,
  ConnectionInfoSchema,
  ConnectionReach,
  LinkKind,
  LinkPart,
  type ProposeChangeRequest,
  SmtpSecurity,
  type WatchChangeResponse,
  type WatchServersResponse,
} from "@dockwell/api";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { expectAccessible } from "../../test/a11y";
import { ENV, places, SERVER } from "../../test/apps";
import { type Api, feed, renderAt } from "../../test/render";
import { aServer, changed, snapshotComplete } from "../../test/servers";
import { mailPlaceOf, mailProvider, privateAddress, tlsPortFor } from "./mail";

function aRelay(init: MessageInitShape<typeof ConnectionInfoSchema> = {}) {
  return create(ConnectionInfoSchema, {
    id: "cnx_9",
    name: "resend",
    displayName: "Resend · notes@example.com",
    kind: LinkKind.SMTP,
    host: "smtp.resend.com",
    port: 587,
    security: SmtpSecurity.STARTTLS,
    sender: "notes@example.com",
    senderName: "Notes",
    reach: ConnectionReach.EVERY,
    uses: [
      {
        linkId: "lnk_9",
        link: "mail",
        environment: ENV,
        project: "prj_1",
        sets: { SMTP_HOST: LinkPart.HOST, SMTP_PASSWORD: LinkPart.PASSWORD, MAIL_FROM: LinkPart.SENDER },
        appIds: ["svc_outline"],
        apps: ["outline"],
      },
    ],
    ...init,
  } as MessageInitShape<typeof ConnectionInfoSchema>);
}

function planned() {
  return vi.fn((req: ProposeChangeRequest) => {
    void req;
    return { change: create(ChangeSchema, { id: "chg_1", state: ChangeState.PLANNED, plan: { fingerprint: "abc" } }) };
  });
}

function fleet(): Pick<Api, "servers"> {
  const watch = feed<WatchServersResponse>();
  watch.push(changed(aServer({ id: SERVER, name: "hz-1" })), snapshotComplete);
  return { servers: { watchServers: watch.stream } };
}

describe("a mail server's connection", () => {
  it("reads which service a host is at, and makes each service's host from what's picked", () => {
    expect(mailPlaceOf("email-smtp.eu-west-1.amazonaws.com")).toEqual({ provider: "ses", region: "eu-west-1" });
    expect(mailPlaceOf("smtp.eu.mailgun.org")).toEqual({ provider: "mailgun", region: "eu" });
    expect(mailPlaceOf("smtp.resend.com").provider).toBe("resend");
    expect(mailPlaceOf("mail.example.com").provider).toBe("mail");
    expect(mailProvider("ses").host("us-east-2")).toBe("email-smtp.us-east-2.amazonaws.com");
    // Use TLS goes only to a port that takes TLS (the re-check's N4).
    const starttls = SmtpSecurity.STARTTLS;
    expect(tlsPortFor(mailProvider("ses"), starttls, 2587)).toBe(2465);
    expect(tlsPortFor(mailProvider("resend"), starttls, 587)).toBe(465);
    expect(tlsPortFor(mailProvider("mailgun"), starttls, 2525)).toBe(465);
    expect(tlsPortFor(mailProvider("postmark"), starttls, 587)).toBeUndefined();
    expect(tlsPortFor(mailProvider("mail"), starttls, 2525)).toBe(465);
    expect(tlsPortFor(mailProvider("mail"), starttls, 4650)).toBe(4650);
    // Without TLS a password goes only to a private address, judged by its range (the review's H1).
    // Parsed as the server parses it, so each spelling reads the same (the re-check's N2).
    for (const host of [
      "10.0.0.5",
      "192.168.1.2",
      "172.20.0.4",
      "127.0.0.1",
      "100.64.0.9",
      "fd00::25",
      "[::1]",
      "::ffff:10.0.0.5",
      "::ffff:a00:5",
      "0:0:0:0:0:0:0:1",
      "fd00::25%eth0",
      "fc00:0:0:0:0:0:0:1",
    ]) {
      expect(privateAddress(host), host).toBe(true);
    }
    for (const host of [
      "mail",
      "mail.internal",
      "2001:db8::25",
      "203.0.113.7",
      "172.32.0.1",
      "fe80::1",
      "fd::1",
      "fc0::1",
      "010.0.0.5",
      "10.999.0.1",
      "::ffff:203.0.113.5",
      "::10.0.0.5",
      "0.0.0.0",
      "::",
      "1::2::3",
    ]) {
      expect(privateAddress(host), host).toBe(false);
    }
  });

  it("checks a mail server once its key is left, signing in and sending nothing, and plans it", async () => {
    const user = userEvent.setup();
    const checks: CheckConnectionRequest[] = [];
    const proposed = planned();
    renderAt("/connections", {
      ...fleet(),
      resources: places(),
      connections: {
        listConnections: () => ({ connections: [aRelay()] }),
        checkConnection: (req) => {
          checks.push(req);
          return req.smtp?.password === "re_right"
            ? { passed: true, server: "hz-1" }
            : { passed: false, step: "sign-in", code: "535 5.7.8" };
        },
      },
      changes: { proposeChange: proposed, watchChange: feed<WatchChangeResponse>().stream },
    });
    await user.click(await screen.findByRole("button", { name: "Add connection" }));
    await user.click(
      within(await screen.findByRole("dialog", { name: "Add a connection" })).getByRole("button", { name: "Resend" }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Add a connection" });
    expect(within(sheet).getByText("A mail server, for apps to send mail through")).toBeVisible();
    expect(within(sheet).queryByRole("textbox", { name: "User" })).toBeNull();
    await user.type(within(sheet).getByRole("textbox", { name: "Sends as" }), "notes@example.com");
    await user.type(within(sheet).getByLabelText("API key"), "re_wrong");
    // Nothing signs in while a person types: a server counts each wrong try (the review's M2).
    await new Promise((r) => setTimeout(r, 900));
    expect(checks).toHaveLength(0);
    await user.tab();
    expect(await within(sheet).findByText("Sign-in refused", {}, { timeout: 3000 })).toBeVisible();
    // Leaving it again, unchanged, tries nothing more.
    await user.click(within(sheet).getByLabelText("API key"));
    await user.tab();
    expect(checks).toHaveLength(1);
    expect(within(sheet).getByText("It answered 535 5.7.8. Check the user and password.")).toBeVisible();
    const review = within(sheet).getByRole("button", { name: "Review plan" });
    expect(review).toBeDisabled();
    await user.clear(within(sheet).getByLabelText("API key"));
    await user.type(within(sheet).getByLabelText("API key"), "re_right");
    expect(review).toBeDisabled();
    await user.click(within(sheet).getByRole("button", { name: "Check" }));
    expect(await within(sheet).findByText("Sign-in works", {}, { timeout: 3000 })).toBeVisible();
    expect(within(sheet).getByText("Checked from hz-1. It took the sign-in, and Dockwell sent no mail.")).toBeVisible();
    expect(checks.at(-1)?.smtp).toMatchObject({
      host: "smtp.resend.com",
      port: 587,
      security: SmtpSecurity.STARTTLS,
      user: "resend",
      password: "re_right",
    });
    expect(checks.at(-1)?.s3).toBeUndefined();
    await user.click(review);
    await waitFor(() => expect(proposed).toHaveBeenCalled());
    const edit = proposed.mock.calls[0]?.[0].edits[0];
    expect(edit?.resource).toMatchObject({ kind: "connection", name: "resend-2" });
    expect(edit?.spec?.kind.value).toMatchObject({
      kind: LinkKind.SMTP,
      smtp: { host: "smtp.resend.com", user: "resend", password: "re_right", sender: "notes@example.com" },
    });
  });

  it("offers the service's own other port where the server can't reach the port, and checks again on it", async () => {
    const user = userEvent.setup();
    const checks: CheckConnectionRequest[] = [];
    renderAt("/connections", {
      ...fleet(),
      resources: places(),
      connections: {
        listConnections: () => ({ connections: [aRelay()] }),
        checkConnection: (req) => {
          checks.push(req);
          return req.smtp?.port === 2525
            ? { passed: true, server: "hz-1" }
            : { passed: false, step: "connect", code: "timeout", server: "hz-1" };
        },
      },
    });
    await user.click(await screen.findByRole("button", { name: "Add connection" }));
    await user.click(
      within(await screen.findByRole("dialog", { name: "Add a connection" })).getByRole("button", { name: "Postmark" }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Add a connection" });
    await user.type(within(sheet).getByLabelText("Server token"), "pm-token");
    await user.tab();
    expect(
      await within(sheet).findByText(
        "This server can't reach port 587, so apps on it can't send mail through it.",
        {},
        { timeout: 3000 },
      ),
    ).toBeVisible();
    // Postmark's server token is its user and its password both.
    expect(checks.at(-1)?.smtp).toMatchObject({ host: "smtp.postmarkapp.com", user: "pm-token", password: "pm-token" });
    await user.click(within(sheet).getByRole("button", { name: "Use port 2525" }));
    expect(await within(sheet).findByText("Sign-in works", {}, { timeout: 3000 })).toBeVisible();
    expect(checks.at(-1)?.smtp?.port).toBe(2525);
  });

  // The review's M1: Resend and Amazon SES take 2587 for STARTTLS, not 2525.
  it("offers Amazon SES its own 2587, and Use TLS where a port took the connection and said nothing", async () => {
    const user = userEvent.setup();
    const checks: CheckConnectionRequest[] = [];
    renderAt("/connections", {
      ...fleet(),
      resources: places(),
      connections: {
        listConnections: () => ({ connections: [aRelay()] }),
        checkConnection: (req) => {
          checks.push(req);
          if (req.smtp?.port === 587) return { passed: false, step: "connect", code: "timeout", server: "hz-1" };
          if (req.smtp?.security === SmtpSecurity.STARTTLS) return { passed: false, step: "hello", code: "timeout" };
          return { passed: true, server: "hz-1" };
        },
      },
    });
    await user.click(await screen.findByRole("button", { name: "Add connection" }));
    await user.click(
      within(await screen.findByRole("dialog", { name: "Add a connection" })).getByRole("button", {
        name: "Amazon SES",
      }),
    );
    const sheet = await screen.findByRole("dialog", { name: "Add a connection" });
    await user.type(within(sheet).getByRole("textbox", { name: "User" }), "AKIDEXAMPLE");
    await user.type(within(sheet).getByLabelText("Password"), "ses-password");
    await user.tab();
    expect(await within(sheet).findByText("Can't reach port 587", {}, { timeout: 3000 })).toBeVisible();
    expect(within(sheet).queryByRole("button", { name: "Use port 2525" })).toBeNull();
    await user.click(within(sheet).getByRole("button", { name: "Use port 2587" }));
    expect(await within(sheet).findByText("No greeting", {}, { timeout: 3000 })).toBeVisible();
    // It took the connection, so it was reached as set: nothing plans unchecked.
    expect(within(sheet).getByRole("button", { name: "Review plan" })).toBeDisabled();
    expect(checks.at(-1)?.smtp).toMatchObject({ host: "email-smtp.us-east-1.amazonaws.com", port: 2587 });
    await user.click(within(sheet).getByRole("button", { name: "Use TLS" }));
    expect(await within(sheet).findByText("Sign-in works", {}, { timeout: 3000 })).toBeVisible();
    expect(checks.at(-1)?.smtp).toMatchObject({ port: 2465, security: SmtpSecurity.TLS });
    expect(within(sheet).getByText("Checked from hz-1. It took the sign-in, and Dockwell sent no mail.")).toBeVisible();
  });

  it("lists a mail server by its service and who mail comes from", async () => {
    renderAt("/connections", {
      ...fleet(),
      resources: places(),
      connections: { listConnections: () => ({ connections: [aRelay()] }) },
    });
    const row = await screen.findByRole("row", { name: /resend/ });
    expect(row).toHaveTextContent("Resend · notes@example.com");
    expect(row).toHaveTextContent("1 app");
  });

  it("shows one: where it is and who mail comes from, never who signs in, and no prefix", async () => {
    const { container } = renderAt("/connections/resend", {
      ...fleet(),
      resources: places(),
      connections: { listConnections: () => ({ connections: [aRelay()] }) },
    });
    expect(await screen.findByRole("heading", { level: 1, name: "resend" })).toBeVisible();
    const details = screen.getByRole("region", { name: "Details" });
    expect(details).toHaveTextContent("smtp.resend.com");
    expect(details).toHaveTextContent("587");
    expect(details).toHaveTextContent("STARTTLS");
    expect(details).toHaveTextContent("Notes <notes@example.com>");
    expect(within(details).getByRole("button", { name: "Replace password" })).toBeVisible();
    const usedBy = screen.getByRole("region", { name: "Used by" });
    expect(usedBy).toHaveTextContent("SMTP_PASSWORD");
    expect(usedBy).toHaveTextContent("password");
    expect(within(usedBy).queryByRole("columnheader", { name: "Objects under" })).toBeNull();
    // A mail server is set up from an app's own card.
    expect(screen.queryByRole("button", { name: "Use in an app" })).toBeNull();
    await expectAccessible(container);
  });
});
