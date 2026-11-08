// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { type Member, MemberService, Role, SignInKind } from "@dockwell/api";
import { Button, Callout, Icon, Panel, PHONE_QUERY, Table, type TableColumn, useMediaQuery } from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { Link } from "@tanstack/react-router";
import type { ReactNode } from "react";

import { sentence } from "../../lib/errors";
import { useTimeWords } from "../../lib/time";
import { initials, useRoleName } from "../auth/user";
import { useControlPlaneNow } from "../servers/useFleet";
import { Page } from "../shell/Page";
import "./Settings.css";

const ADR = "https://github.com/Dockwell/dockwell/blob/main/docs/adr/0048-members-as-far-as-one-owner-goes.md";

/**
 * Members, as Members.dc.html has them, as far as one owner goes: who can sign
 * in, their role and how they sign in, what the owner may do, and that inviting
 * comes later (ADR 0048).
 */
export function MembersPage() {
  const { t } = useLingui();
  const list = useQuery(MemberService.method.listMembers, {});
  const phone = useMediaQuery(PHONE_QUERY);
  const members = list.data?.members ?? [];
  const count = members.length;
  const withPasskeys = members.filter((m) => m.signIn.includes(SignInKind.PASSKEY)).length;

  let people: ReactNode;
  if (list.error && !list.data) {
    people = (
      <Callout
        tone="critical"
        title={t`Dockwell can't say who signs in`}
        actions={
          <Button size="sm" icon="refresh" loading={list.isFetching} onClick={() => void list.refetch()}>
            <Trans>Try again</Trans>
          </Button>
        }
      >
        {sentence(list.error)}
      </Callout>
    );
  } else if (!list.data) {
    people = (
      <div className="tokens-skeleton" aria-busy="true">
        <span className="dw-sr">{t`Loading who signs in`}</span>
      </div>
    );
  } else if (phone) {
    people = (
      <ul className="dw-panel tokens-list" aria-label={t`Who signs in`}>
        {members.map((m) => (
          <li key={m.user?.id} className="tokens-item">
            <MemberFacts member={m} />
          </li>
        ))}
      </ul>
    );
  } else {
    people = <MembersTable members={members} />;
  }

  return (
    <Page
      width="narrow"
      title={t`Members`}
      lead={
        // Said once the list is read, never as nobody while it loads or can't be read.
        list.data
          ? count === 1
            ? t`1 person signs in to this install, its owner · inviting others comes later`
            : t`${count} people sign in to this install`
          : undefined
      }
      back="/settings"
    >
      <Callout
        tone="info"
        title={t`Inviting someone comes in a later release`}
        actions={
          <Link to="/settings/api-tokens" className="dw-btn dw-btn-secondary dw-btn-sm">
            <Icon name="key" size={14} />
            <span>
              <Trans>API tokens</Trans>
            </span>
          </Link>
        }
      >
        <Trans>
          An install has one person today, the owner who claimed it, as nothing yet tells an admin, a developer or a
          viewer apart from them. Scripts and integrations sign in with API tokens.{" "}
          <a href={ADR} target="_blank" rel="noreferrer" className="cp-link">
            What members and roles need
          </a>
        </Trans>
      </Callout>
      <section className="tokens-section" aria-labelledby="members-people">
        <div className="cp-section-head">
          <h2 id="members-people" className="dw-text-heading">
            <Trans>People</Trans>
          </h2>
          {list.data ? (
            <span className="settings-meta">
              {withPasskeys === count
                ? t`all sign in with a passkey`
                : t`${withPasskeys} of ${count} sign in with a passkey`}
            </span>
          ) : null}
        </div>
        {people}
      </section>
      <div className="members-panels">
        <OwnerCan />
        <SignIn member={members.find((m) => m.you)} />
      </div>
    </Page>
  );
}

/** A person's avatar, name and email, with "You" beside the person asking. */
function Person({ member }: { member: Member }) {
  const { t } = useLingui();
  const name = member.user?.name ?? "";
  return (
    <span className="members-person">
      <span className="members-avatar" aria-hidden="true">
        {initials(name)}
      </span>
      <span className="dw-cell">
        <span className="dw-cell-title">
          {name}
          {member.you ? <span className="members-you">{t`You`}</span> : null}
        </span>
        <span className="dw-cell-sub">{member.user?.email}</span>
      </span>
    </span>
  );
}

/**
 * How a person signs in, in a few words: yours in full, as it's yours to see, and
 * anyone else's by its kinds alone (ADR 0048).
 */
function useSignInWords(): (member: Member) => string {
  const { t } = useLingui();
  return (m) => {
    if (m.you) {
      const keys = m.passkeys === 1 ? t`1 passkey` : t`${m.passkeys} passkeys`;
      if (m.codesLocked) return t`${keys}, with codes locked`;
      return m.authenticator ? t`${keys} and an authenticator app` : keys;
    }
    const passkey = m.signIn.includes(SignInKind.PASSKEY);
    const app = m.signIn.includes(SignInKind.AUTHENTICATOR_APP);
    if (passkey && app) return t`Passkey and authenticator app`;
    if (passkey) return t`Passkey`;
    if (app) return t`Authenticator app`;
    return t`No way in that works now`;
  };
}

/** When a person was last active: now, a time ago, or not signed in anywhere. */
function useLastActive(): (member: Member) => string {
  const { t } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow(60_000);
  return (m) => {
    if (m.you) return t`Now`;
    if (!m.lastActiveTime) return t`Not signed in`;
    return words.ago(timestampDate(m.lastActiveTime), now);
  };
}

function RoleTag({ role }: { role: Role }) {
  const name = useRoleName(role);
  return <span className="dw-tag dw-tag-words members-role">{name || "–"}</span>;
}

/** A row of the table: a member, by their user's ID. */
interface Row {
  id: string;
  member: Member;
}

function MembersTable({ members }: { members: readonly Member[] }) {
  const { t } = useLingui();
  const signIn = useSignInWords();
  const lastActive = useLastActive();
  const rows: Row[] = members.map((member) => ({ id: member.user?.id ?? "", member }));
  const columns: TableColumn<Row>[] = [
    { key: "who", header: t`Member`, render: ({ member: m }) => <Person member={m} /> },
    {
      key: "role",
      header: t`Role`,
      width: 112,
      render: ({ member: m }) => <RoleTag role={m.user?.role ?? Role.UNSPECIFIED} />,
    },
    {
      key: "signin",
      header: t`Sign-in`,
      width: 260,
      render: ({ member: m }) => (
        <span className="audit-source">
          <Icon name="key" size={14} />
          {signIn(m)}
        </span>
      ),
    },
    { key: "active", header: t`Last active`, width: 128, align: "right", render: ({ member: m }) => lastActive(m) },
    {
      key: "act",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 180,
      align: "right",
      render: ({ member: m }) =>
        m.you ? (
          <Link to="/profile" className="dw-btn dw-btn-ghost dw-btn-sm">
            <span>
              <Trans>Profile and security</Trans>
            </span>
          </Link>
        ) : null,
    },
  ];
  return <Table columns={columns} rows={rows} caption={t`Who signs in to this install`} />;
}

/** A person as a phone shows them: a stacked row. */
function MemberFacts({ member }: { member: Member }) {
  const { t } = useLingui();
  const signIn = useSignInWords();
  const lastActive = useLastActive();
  return (
    <div className="tokens-facts">
      <Person member={member} />
      <dl className="settings-facts tokens-facts-list">
        <dt>{t`Role`}</dt>
        <dd>
          <RoleTag role={member.user?.role ?? Role.UNSPECIFIED} />
        </dd>
        <dt>{t`Sign-in`}</dt>
        <dd>{signIn(member)}</dd>
        <dt>{t`Last active`}</dt>
        <dd>{lastActive(member)}</dd>
      </dl>
    </div>
  );
}

/** What the owner may do, which is everything, as the only role there is. */
function OwnerCan() {
  const { t } = useLingui();
  const rows = [
    t`Deploy, and apply any plan`,
    t`Manage servers and their files`,
    t`Make and revoke API tokens`,
    t`Read the audit log`,
    t`Change the install's settings`,
  ];
  return (
    <Panel title={t`What the owner can do`} flush className="settings-panel">
      <table className="dw-table dw-table-compact members-can">
        <caption className="dw-sr">{t`What the owner can do`}</caption>
        <thead>
          <tr>
            <th scope="col">{t`Permission`}</th>
            <th scope="col" className="members-col">
              {t`Owner`}
            </th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row}>
              <th scope="row">{row}</th>
              <td className="members-col">
                <Icon name="check" size={16} label={t`Yes`} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="members-note">
        <Trans>
          Everything, on every server. Admins, developers and viewers, each seeing and doing less and scoped by project,
          come with members.
        </Trans>
      </p>
    </Panel>
  );
}

/** How the person asking signs in, and where they change it. */
function SignIn({ member }: { member: Member | undefined }) {
  const { t } = useLingui();
  const addresses = member?.passkeyAddresses ?? [];
  return (
    <Panel
      title={t`How you sign in`}
      className="settings-panel"
      actions={
        <Link to="/profile" className="cp-head-link">
          <Trans>Profile and security</Trans>
        </Link>
      }
    >
      <dl className="settings-facts" aria-busy={member === undefined}>
        <dt>{t`Passkeys`}</dt>
        <dd>
          {member ? member.passkeys : "–"}
          {addresses.map((a) => (
            <span key={a}>
              {" · "}
              <code>{a}</code>
            </span>
          ))}
        </dd>
        <dt>{t`Authenticator app`}</dt>
        <dd>
          {member
            ? member.codesLocked
              ? t`Locked after too many wrong codes, until you lift it in Profile and security`
              : member.authenticator
                ? member.recoveryCodesLeft === 1
                  ? t`On, 1 recovery code left`
                  : t`On, ${member.recoveryCodesLeft} recovery codes left`
                : t`Off`
            : "–"}
        </dd>
        <dt>{t`Signed in on`}</dt>
        <dd>{member ? (member.sessions === 1 ? t`1 browser` : t`${member.sessions} browsers`) : "–"}</dd>
        <dt>{t`Single sign-on`}</dt>
        <dd>{t`OIDC comes in a later release`}</dd>
        <dt>{t`Last resort`}</dt>
        <dd>
          <Trans>
            <code>dockwell recover</code> on the control server
          </Trans>
        </dd>
      </dl>
    </Panel>
  );
}
