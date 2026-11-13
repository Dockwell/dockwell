// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import {
  type Invite,
  type Member,
  MemberService,
  Protection,
  type Resource,
  ResourceService,
  Role,
  SignInKind,
  StepUpAction,
} from "@dockwell/api";
import {
  Button,
  Callout,
  Icon,
  Panel,
  PHONE_QUERY,
  SelectField,
  Switch,
  Table,
  type TableColumn,
  TextField,
  useMediaQuery,
} from "@dockwell/ui";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { type FormEvent, type ReactNode, useState } from "react";

import { formatDateTime } from "../../lib/format";
import { useTimeWords } from "../../lib/time";
import { PasskeyError } from "../../lib/webauthn";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useSignedIn } from "../auth/useAuthState";
import { initials, useRoleName } from "../auth/user";
import { useChangeSheet } from "../changes/ChangeSheet";
import { useControlPlaneNow } from "../servers/useFleet";
import { Page } from "../shell/Page";
import { ReadFailed } from "../shell/ReadFailed";
import { CopyButton } from "../system/CopyCommand";
import "./Settings.css";

/**
 * Members, as Members.dc.html has them: who signs in, with their roles and how
 * they sign in, the invites that ask others to, and what each role may do
 * (ADR 0048, ADR 0049). With one person and no invite, the invite form waits
 * behind a button.
 */
export function MembersPage() {
  const { t } = useLingui();
  const list = useQuery(MemberService.method.listMembers, {});
  const phone = useMediaQuery(PHONE_QUERY);
  const members = list.data?.members ?? [];
  const invites = list.data?.invites ?? [];
  const count = members.length;
  const withPasskeys = members.filter((m) => m.signIn.includes(SignInKind.PASSKEY)).length;
  const solo = count <= 1 && invites.length === 0;
  const [inviting, setInviting] = useState(false);

  let people: ReactNode;
  if (list.error && !list.data) {
    people = (
      <ReadFailed
        error={list.error}
        title={t`Dockwell can't say who signs in`}
        actions={
          <Button size="sm" icon="refresh" loading={list.isFetching} onClick={() => void list.refetch()}>
            <Trans>Try again</Trans>
          </Button>
        }
      />
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
        {invites.map((i) => (
          <li key={i.id} className="tokens-item">
            <InviteFacts invite={i} />
          </li>
        ))}
      </ul>
    );
  } else {
    people = <MembersTable members={members} invites={invites} places={!solo} />;
  }

  const invitesWord =
    invites.length === 0 ? "" : invites.length === 1 ? t`1 open invite` : t`${invites.length} open invites`;
  const lead = !list.data
    ? undefined // never "nobody" while it loads or can't be read
    : count === 1
      ? invitesWord
        ? t`1 person signs in to this install · ${invitesWord}`
        : t`1 person signs in to this install, its owner`
      : invitesWord
        ? t`${count} people sign in to this install · ${invitesWord}`
        : t`${count} people sign in to this install`;

  return (
    <Page
      width="narrow"
      title={t`Members`}
      lead={lead}
      back="/settings"
      actions={
        solo && !inviting && list.data ? (
          <Button icon="plus" onClick={() => setInviting(true)}>
            <Trans>Invite someone</Trans>
          </Button>
        ) : undefined
      }
    >
      {list.data && (!solo || inviting) ? <InvitePanel members={members} /> : null}
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
        {solo && !inviting ? null : <RolesCan />}
        <SignIn member={members.find((m) => m.you)} />
      </div>
    </Page>
  );
}

/** The roles an invite offers, as the API names them; nobody is invited as owner. */
const INVITE_ROLES = [Role.ADMIN, Role.DEVELOPER, Role.VIEWER] as const;

const roleNames: Record<number, string> = {
  [Role.ADMIN]: "admin",
  [Role.DEVELOPER]: "developer",
  [Role.VIEWER]: "viewer",
};

/**
 * Invites someone by email with a role in a place, confirmed with a passkey
 * bound to all three, and shows the link it makes once: Dockwell sends no mail.
 * The link is at the panel's domain, so without one this says how to give it
 * one instead.
 */
function InvitePanel({ members }: { members: readonly Member[] }) {
  const { t, i18n } = useLingui();
  const transport = useTransport();
  const queryClient = useQueryClient();
  const explain = useAuthError();
  const domain = useSignedIn()?.domain ?? "";
  const projects = useQuery(ResourceService.method.listResources, { kind: "project" });
  const admin = useRoleName(Role.ADMIN);
  const developer = useRoleName(Role.DEVELOPER);
  const viewer = useRoleName(Role.VIEWER);
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<Role>(Role.DEVELOPER);
  const [project, setProject] = useState("");
  const [invalid, setInvalid] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<unknown>();
  const [sent, setSent] = useState<{ email: string; link: string; until?: Date }>();
  // Before anyone else signs in, the owner checks which environments are
  // protected, as that decides whose changes wait (ADR 0049).
  const first = members.length <= 1;
  const [protectionChecked, setProtectionChecked] = useState(false);
  const [protectionUnchecked, setProtectionUnchecked] = useState(false);

  if (!domain) {
    return (
      <Callout tone="info" title={t`Invites need the panel's domain`}>
        <Trans>
          The person you invite accepts at it, and their passkey is made there, as an SSH tunnel is yours alone. Set{" "}
          <code>DOCKWELL_DOMAIN</code> for the service to invite someone.
        </Trans>
      </Callout>
    );
  }

  const list = projects.data?.resources ?? [];
  // A place to choose shows once there's more than one project, and an admin
  // holds the whole install (ADR 0049).
  const choosePlace = list.length > 1 && role !== Role.ADMIN;
  const place = choosePlace ? project : "";
  const submit = async (event: FormEvent) => {
    event.preventDefault();
    // As typed: the control plane writes it its one way, in small letters, for the passkey's
    // target and for the invite alike, so the two can't differ over a letter such as İ.
    const address = email.trim();
    if (!/^[^\s@]+@[^\s@]+$/.test(address)) {
      setInvalid(t`Enter an email address, such as ada@example.com.`);
      return;
    }
    if (members.some((m) => m.user?.email.toLowerCase() === address.toLowerCase())) {
      setInvalid(t`${address} signs in here already.`);
      return;
    }
    setInvalid(undefined);
    if (first && !protectionChecked) {
      setProtectionUnchecked(true);
      return;
    }
    setBusy(true);
    setProblem(undefined);
    try {
      const target = `${address} ${roleNames[role]} ${place || "install"}`;
      const client = createClient(MemberService, transport);
      const res = await withStepUpIfAsked(transport, StepUpAction.INVITE, target, (proof) =>
        client.createInvite({ email: address, role, project: place, proof }),
      );
      setSent({
        // As the control plane wrote it, which is who the invite is for.
        email: res.invite?.email || address,
        link: res.link,
        until: res.invite?.expireTime ? timestampDate(res.invite.expireTime) : undefined,
      });
      setEmail("");
      await queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: MemberService.method.listMembers, cardinality: "finite" }),
      });
    } catch (err) {
      setProblem(err instanceof PasskeyError && err.problem === "cancelled" ? undefined : err);
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="dw-panel members-invite" aria-label={t`Invite people`}>
      {first ? (
        <ProtectionCheck
          projects={list}
          checked={protectionChecked}
          unchecked={protectionUnchecked && !protectionChecked}
          onCheck={(checked) => {
            setProtectionChecked(checked);
            setProtectionUnchecked(false);
          }}
        />
      ) : null}
      <form className="members-invite-form" onSubmit={(e) => void submit(e)} noValidate>
        <div className="members-invite-email">
          <TextField
            label={t`Invite by email`}
            type="email"
            icon="mail"
            placeholder="name@example.com"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            autoComplete="off"
            error={invalid}
          />
        </div>
        <SelectField
          label={t`Role`}
          value={String(role)}
          onChange={(e) => setRole(Number(e.target.value) as Role)}
          options={INVITE_ROLES.map((r) => ({
            value: String(r),
            label: r === Role.ADMIN ? admin : r === Role.DEVELOPER ? developer : viewer,
          }))}
        />
        <SelectField
          label={t`Projects`}
          value={place}
          disabled={!choosePlace}
          onChange={(e) => setProject(e.target.value)}
          options={[
            { value: "", label: t`This install` },
            ...(choosePlace
              ? list.map((p) => ({ value: p.id, label: p.displayName || p.resource?.name || p.id }))
              : []),
          ]}
        />
        <Button type="submit" variant="primary" icon="plus" loading={busy}>
          <Trans>Invite</Trans>
        </Button>
      </form>
      <p className="members-invite-note">
        <Trans>
          They accept at <code>{domain}</code> with a passkey. Dockwell sends no mail: send them the link it shows.
          Inviting asks for your passkey.
        </Trans>
      </p>
      {problem ? (
        <Callout tone="critical" title={t`Nobody was invited`}>
          {explain(problem)}
        </Callout>
      ) : null}
      {sent ? (
        <Callout
          tone="healthy"
          title={t`Send this link to ${sent.email}`}
          actions={<CopyButton text={sent.link} label={t`Copy link`} copiedLabel={t`Link copied`} />}
        >
          <code className="members-link">{sent.link}</code>{" "}
          {sent.until ? (
            <Trans>
              It works once, until {formatDateTime(sent.until, i18n.locale)}, for whoever has it. Dockwell can't show it
              again.
            </Trans>
          ) : null}
        </Callout>
      ) : null}
    </section>
  );
}

/**
 * Each environment and whether it's protected, for the owner to check before
 * anyone else signs in (ADR 0049): a developer's change in a protected one
 * waits for an admin or the owner. Protecting one, or no longer, is a change
 * like any other, planned in the plan sheet.
 */
function ProtectionCheck({
  projects,
  checked,
  unchecked,
  onCheck,
}: {
  projects: readonly Resource[];
  checked: boolean;
  /** Inviting was asked for before the box was ticked. */
  unchecked: boolean;
  onCheck: (checked: boolean) => void;
}) {
  const { t } = useLingui();
  const sheet = useChangeSheet();
  const explain = useAuthError();
  const environments = useQuery(ResourceService.method.listResources, { kind: "environment" });
  const several = projects.length > 1;
  const projectName = (id: string) => {
    const p = projects.find((r) => r.id === id);
    return p ? p.displayName || p.resource?.name || id : id;
  };
  const named = (env: Resource) => {
    const name = env.displayName || env.resource?.name || env.id;
    return several ? t`${name} in ${projectName(env.resource?.scope ?? "")}` : name;
  };
  const change = (env: Resource) => {
    const where = named(env);
    sheet.open({
      kind: "propose",
      summary: env.protected ? t`Stop protecting ${where}` : t`Protect ${where}`,
      edits: [
        {
          resource: { kind: "environment", scope: env.resource?.scope ?? "", name: env.resource?.name ?? "" },
          // An environment's spec is its display name and its protection, so
          // this keeps the one and changes the other.
          spec: {
            kind: {
              case: "environment",
              value: {
                displayName: env.displayName,
                protection: env.protected ? Protection.OPEN : Protection.PROTECTED,
              },
            },
          },
        },
      ],
    });
  };

  let list: ReactNode;
  if (environments.error && !environments.data) {
    list = (
      <p className="members-problem" role="alert">
        {t`Dockwell can't list the environments: ${explain(environments.error)}`}{" "}
        <Button size="sm" variant="ghost" icon="refresh" onClick={() => void environments.refetch()}>
          <Trans>Try again</Trans>
        </Button>
      </p>
    );
  } else if (!environments.data) {
    list = (
      <p className="members-muted" aria-busy="true">
        {t`Loading the environments`}
      </p>
    );
  } else {
    list = (
      <ul className="members-protection-list" aria-label={t`Environments`}>
        {environments.data.resources.map((env) => (
          <li key={env.id} className="members-protection-item">
            <span className="members-protection-name">{named(env)}</span>
            {/* As Settings draws it: switching opens a plan, and it shows the change once applied. */}
            <Switch
              checked={env.protected}
              onChange={() => change(env)}
              aria-label={t`Protect ${named(env)}`}
              label={env.protected ? t`Protected` : t`Not protected`}
            />
          </li>
        ))}
      </ul>
    );
  }

  return (
    <fieldset className="members-protection">
      <legend className="members-protection-head">{t`Before anyone else signs in`}</legend>
      <p className="members-invite-note">
        <Trans>
          A developer's change in a protected environment waits for an admin or the owner, who applies it with a
          passkey. Check that each environment is as you want it. This is a reminder, shown until someone else signs in:
          Dockwell keeps no record of the tick.
        </Trans>
      </p>
      {list}
      <label className="members-protection-ok">
        <input
          type="checkbox"
          checked={checked}
          aria-invalid={unchecked || undefined}
          onChange={(e) => onCheck(e.target.checked)}
        />
        <Trans>I've checked which environments are protected</Trans>
      </label>
      {unchecked ? (
        <p className="members-problem" role="alert">
          {t`Check each environment's protection first, then tick the box: it decides whose changes wait.`}
        </p>
      ) : null}
    </fieldset>
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

/** A row of the table: a member by their user's ID, or an open invite by its ID. */
type Row = { id: string; member: Member; invite?: undefined } | { id: string; invite: Invite; member?: undefined };

/** Where an invite gives its role: the whole install, which has no project ID, or its project by name. */
function useInvitePlace(): (invite: Invite) => string {
  const { t } = useLingui();
  return (invite) => (invite.projectId === "" ? t`All projects` : invite.projectName || t`A project that's gone`);
}

/** Where someone holds a role: the whole install, or their projects by name. */
function usePlacesOf(): (m: Member) => string {
  const { t } = useLingui();
  return (m) => {
    if ((m.user?.role ?? Role.UNSPECIFIED) !== Role.UNSPECIFIED) return t`All projects`;
    return m.grants.map((g) => g.projectName).join(", ") || "–";
  };
}

/** Someone's role: on the whole install, or the highest they hold in a project. */
function roleOf(m: Member): Role {
  const install = m.user?.role ?? Role.UNSPECIFIED;
  if (install !== Role.UNSPECIFIED) return install;
  return m.grants.some((g) => g.role === Role.DEVELOPER) ? Role.DEVELOPER : (m.grants[0]?.role ?? Role.UNSPECIFIED);
}

/** An open invite, as a person-to-be: the email, and who invited them, when. */
function InvitedPerson({ invite }: { invite: Invite }) {
  const { t } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow(60_000);
  const when = invite.createTime ? words.ago(timestampDate(invite.createTime), now) : "";
  return (
    <span className="members-person">
      <span className="members-avatar members-avatar-invite" aria-hidden="true">
        <Icon name="mail" size={14} />
      </span>
      <span className="dw-cell">
        <span className="dw-cell-title">{invite.email}</span>
        <span className="dw-cell-sub members-invited">{t`Invited by ${invite.invitedBy} ${when}`}</span>
      </span>
    </span>
  );
}

const DAY = 86_400_000;

/** When an open invite stops working. */
function useExpires(): (invite: Invite) => string {
  const { t } = useLingui();
  const words = useTimeWords();
  const now = useControlPlaneNow(60_000);
  return (i) => {
    if (!i.expireTime) return "";
    // To the nearest day once it's a day or more off, as a week reads "7 days", not "6 days".
    const left = timestampDate(i.expireTime).getTime() - now;
    return t`Expires in ${words.span(left >= DAY ? Math.round(left / DAY) * DAY : left)}`;
  };
}

/** Stops an open invite working, once a second press says so: its link stops at once, and can't come back. */
function RevokeInvite({ invite }: { invite: Invite }) {
  const { t } = useLingui();
  const transport = useTransport();
  const queryClient = useQueryClient();
  const explain = useAuthError();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<string>();
  const revoke = async () => {
    setBusy(true);
    setProblem(undefined);
    try {
      await createClient(MemberService, transport).revokeInvite({ id: invite.id });
      await queryClient.invalidateQueries({
        queryKey: createConnectQueryKey({ schema: MemberService.method.listMembers, cardinality: "finite" }),
      });
    } catch (err) {
      setProblem(explain(err));
      setConfirming(false);
    } finally {
      setBusy(false);
    }
  };
  return (
    <>
      {confirming ? (
        <>
          <Button
            variant="danger"
            size="sm"
            loading={busy}
            onClick={() => void revoke()}
            aria-label={t`Revoke the invite for ${invite.email}: its link stops working`}
          >
            <Trans>Revoke</Trans>
          </Button>
          <Button variant="ghost" size="sm" onClick={() => setConfirming(false)}>
            <Trans>Keep</Trans>
          </Button>
        </>
      ) : (
        <Button
          variant="ghost"
          size="sm"
          icon="x"
          onClick={() => setConfirming(true)}
          aria-label={t`Revoke the invite for ${invite.email}`}
        >
          <Trans>Revoke</Trans>
        </Button>
      )}
      {problem ? (
        <span className="members-problem" role="alert">
          {problem}
        </span>
      ) : null}
    </>
  );
}

function MembersTable({
  members,
  invites,
  places,
}: {
  members: readonly Member[];
  invites: readonly Invite[];
  /** Whether to show where each holds their role: not while one person signs in, alone (ADR 0049). */
  places: boolean;
}) {
  const { t } = useLingui();
  const signIn = useSignInWords();
  const lastActive = useLastActive();
  const placesOf = usePlacesOf();
  const invitePlace = useInvitePlace();
  const expires = useExpires();
  const rows: Row[] = [
    ...members.map((member) => ({ id: member.user?.id ?? "", member })),
    ...invites.map((invite) => ({ id: invite.id, invite })),
  ];
  const columns: TableColumn<Row>[] = [
    {
      key: "who",
      header: t`Member`,
      render: (r) => (r.member ? <Person member={r.member} /> : <InvitedPerson invite={r.invite} />),
    },
    {
      key: "role",
      header: t`Role`,
      width: 112,
      render: (r) => <RoleTag role={r.member ? roleOf(r.member) : r.invite.role} />,
    },
    ...(places
      ? [
          {
            key: "projects",
            header: t`Projects`,
            width: 140,
            render: (r: Row) => (r.member ? placesOf(r.member) : invitePlace(r.invite)),
          },
        ]
      : []),
    {
      key: "signin",
      header: t`Sign-in`,
      width: 220,
      render: (r) =>
        r.member ? (
          <span className="audit-source">
            <Icon name="key" size={14} />
            {signIn(r.member)}
          </span>
        ) : (
          <span className="members-muted">{t`Not accepted yet`}</span>
        ),
    },
    {
      key: "active",
      header: t`Last active`,
      width: 140,
      align: "right",
      render: (r) => (r.member ? lastActive(r.member) : expires(r.invite)),
    },
    {
      key: "act",
      header: <span className="dw-sr">{t`Actions`}</span>,
      width: 180,
      align: "right",
      render: (r) =>
        r.invite ? (
          <RevokeInvite invite={r.invite} />
        ) : r.member.you ? (
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
  const placesOf = usePlacesOf();
  return (
    <div className="tokens-facts">
      <Person member={member} />
      <dl className="settings-facts tokens-facts-list">
        <dt>{t`Role`}</dt>
        <dd>
          <RoleTag role={roleOf(member)} />
        </dd>
        <dt>{t`Projects`}</dt>
        <dd>{placesOf(member)}</dd>
        <dt>{t`Sign-in`}</dt>
        <dd>{signIn(member)}</dd>
        <dt>{t`Last active`}</dt>
        <dd>{lastActive(member)}</dd>
      </dl>
    </div>
  );
}

/** An open invite as a phone shows it. */
function InviteFacts({ invite }: { invite: Invite }) {
  const { t } = useLingui();
  const invitePlace = useInvitePlace();
  const expires = useExpires();
  return (
    <div className="tokens-facts">
      <InvitedPerson invite={invite} />
      <dl className="settings-facts tokens-facts-list">
        <dt>{t`Role`}</dt>
        <dd>
          <RoleTag role={invite.role} />
        </dd>
        <dt>{t`Projects`}</dt>
        <dd>{invitePlace(invite)}</dd>
        <dt>{t`Expires`}</dt>
        <dd>{expires(invite)}</dd>
      </dl>
      <RevokeInvite invite={invite} />
    </div>
  );
}

/** What each role may do, as ADR 0049's table has it. */
/** The roles RolesCan compares, in its columns' order. */
const ROLE_COLUMNS = ["owner", "admin", "developer", "viewer"] as const;

function RolesCan() {
  const { t } = useLingui();
  const owner = useRoleName(Role.OWNER);
  const admin = useRoleName(Role.ADMIN);
  const developer = useRoleName(Role.DEVELOPER);
  const viewer = useRoleName(Role.VIEWER);
  const yes = <Icon name="check" size={16} label={t`Yes`} />;
  const no = <span className="members-muted">{t`No`}</span>;
  const theirs = <span>{t`Their projects`}</span>;
  const rows: [string, ReactNode, ReactNode, ReactNode, ReactNode][] = [
    [t`Read apps, plans, activity and logs`, yes, yes, theirs, theirs],
    [t`Plan and apply in open environments`, yes, yes, theirs, no],
    [t`Apply in protected environments`, yes, yes, <span key="w">{t`Waits for an admin`}</span>, no],
    [t`Reveal a secret's value`, yes, yes, <span key="o">{t`In open environments`}</span>, no],
    [t`Make projects, manage servers and settings`, yes, yes, no, no],
    [t`Invite people and change roles`, yes, <span key="u">{t`Up to admin`}</span>, no, no],
    [t`Transfer ownership`, yes, no, no, no],
  ];
  return (
    <Panel title={t`What each role can do`} flush className="settings-panel">
      <table className="dw-table dw-table-compact members-can">
        <caption className="dw-sr">{t`What each role can do`}</caption>
        <thead>
          <tr>
            <th scope="col">{t`Permission`}</th>
            {[owner, admin, developer, viewer].map((name) => (
              <th key={name} scope="col" className="members-col">
                {name}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map(([row, ...cells]) => (
            <tr key={row}>
              <th scope="row">{row}</th>
              {cells.map((cell, i) => (
                <td key={ROLE_COLUMNS[i]} className="members-col">
                  {cell}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
      <p className="members-note">
        <Trans>
          Everyone with a role reads apps, logs and activity in their projects. A developer's plan in a protected
          environment waits for an admin or the owner.
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
