// SPDX-License-Identifier: AGPL-3.0-only

import { timestampDate } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useQuery, useTransport } from "@connectrpc/connect-query";
import { type Member, MemberService, ResourceService, Role, StepUpAction, TokenRole } from "@dockwell/api";
import { Button, Callout, Loading, SelectField, Sheet, SkeletonFacts } from "@dockwell/ui";
import { plural } from "@lingui/core/macro";
import { Trans, useLingui } from "@lingui/react/macro";
import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { type ReactNode, useState } from "react";
import { authStateKey } from "../../app/api";
import { useTimeWords } from "../../lib/time";
import { PasskeyError } from "../../lib/webauthn";
import { useMay } from "../auth/may";
import { useAuthError } from "../auth/messages";
import { withStepUpIfAsked } from "../auth/stepUp";
import { useRoleName } from "../auth/user";
import { useControlPlaneNow } from "../servers/useFleet";
import { ActionMenu, type MenuAction } from "../shell/ActionMenu";
import { ReadFailed } from "../shell/ReadFailed";

/** The role's name in a passkey's target, as the control plane writes it. */
const ROLE_WORD: Partial<Record<Role, string>> = {
  [Role.ADMIN]: "admin",
  [Role.DEVELOPER]: "developer",
  [Role.VIEWER]: "viewer",
};

/** What someone holds on the whole install, as a role, or none for someone who holds projects alone. */
const installRole = (member: Member): Role => member.user?.role ?? Role.UNSPECIFIED;

type Action = "role" | "remove" | "transfer";

/**
 * What an admin or the owner may do about someone else, behind a menu on their
 * row: change what they hold, remove them, and for the owner, make them the
 * owner. Each opens a sheet that says what it ends before it asks for a
 * passkey (ADR 0049, "Managing members"). Nobody acts on themselves or on the
 * owner, so those rows have no menu.
 */
export function MemberMenu({ member }: { member: Member }) {
  const { menuFor, sheet } = useMemberActions();
  const { label, actions } = menuFor(member);
  return (
    <>
      <ActionMenu label={label} actions={actions} />
      {sheet}
    </>
  );
}

export function useMemberActions() {
  const { t } = useLingui();
  const may = useMay();
  const [doing, setDoing] = useState<{ member: Member; action: Action }>();
  const mayRole = may(MemberService.method.setMemberRole).allowed;
  const mayRemove = may(MemberService.method.removeMember).allowed;
  const mayTransfer = may(MemberService.method.transferOwnership).allowed;
  const close = () => setDoing(undefined);
  const select = (member: Member, action: Action) => setDoing({ member, action });
  const menuFor = (member: Member) => {
    const name = member.user?.name ?? "";
    const label = t`Manage ${name}`;
    const actions: MenuAction[] = [];
    if (member.you || installRole(member) === Role.OWNER) return { label, actions };
    if (mayRole) actions.push({ id: "role", label: t`Change role`, onSelect: () => select(member, "role") });
    if (mayTransfer)
      actions.push({ id: "transfer", label: t`Transfer ownership`, onSelect: () => select(member, "transfer") });
    if (mayRemove)
      actions.push({ id: "remove", label: t`Remove`, danger: true, onSelect: () => select(member, "remove") });
    return { label, actions };
  };
  const sheet =
    doing?.action === "role" ? (
      <RoleSheet member={doing.member} onClose={close} onRemove={() => select(doing.member, "remove")} />
    ) : doing?.action === "remove" ? (
      <RemoveSheet member={doing.member} onClose={close} />
    ) : doing?.action === "transfer" ? (
      <TransferSheet member={doing.member} onClose={close} />
    ) : null;
  return { menuFor, sheet };
}

/** Reads Members again, and for a transfer, who is signed in as what. */
function useReadAgain() {
  const queryClient = useQueryClient();
  return async (alsoSignIn = false) => {
    await queryClient.invalidateQueries({
      queryKey: createConnectQueryKey({ schema: MemberService.method.listMembers, cardinality: "finite" }),
    });
    if (alsoSignIn) await queryClient.invalidateQueries({ queryKey: authStateKey });
  };
}

/**
 * What a change does, as rows of what and what becomes of it, in place of a
 * paragraph (the design system's text budget).
 */
function Effects({ label, rows }: { label: string; rows: readonly (readonly [string, ReactNode])[] }) {
  return (
    <dl className="settings-facts members-removal" aria-label={label}>
      {rows.map(([what, becomes]) => (
        <div key={what} className="members-effect">
          <dt>{what}</dt>
          <dd>{becomes}</dd>
        </div>
      ))}
    </dl>
  );
}

/** A passkey prompt someone closed isn't a failure to say anything about. */
const dismissed = (err: unknown) => err instanceof PasskeyError && err.problem === "cancelled";

/**
 * Changes what someone holds in one place: a role on the whole install, or in
 * one project, or nothing there any more. It says what a lowering ends before
 * it asks for the passkey.
 */
function RoleSheet({
  member,
  onClose,
  onRemove,
}: {
  member: Member;
  onClose: () => void;
  /** Opens Remove in this sheet's place, for taking back the last thing someone holds. */
  onRemove: () => void;
}) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const readAgain = useReadAgain();
  const projects = useQuery(ResourceService.method.listResources, { kind: "project" }).data?.resources ?? [];
  const admin = useRoleName(Role.ADMIN);
  const developer = useRoleName(Role.DEVELOPER);
  const viewer = useRoleName(Role.VIEWER);
  const name = member.user?.name ?? "";
  const userId = member.user?.id ?? "";
  // Where they hold something now comes first: the install, or their first project.
  const [place, setPlace] = useState(
    installRole(member) !== Role.UNSPECIFIED ? "" : (member.grants[0]?.projectId ?? ""),
  );
  const heldThere =
    place === "" ? installRole(member) : (member.grants.find((g) => g.projectId === place)?.role ?? Role.UNSPECIFIED);
  const [role, setRole] = useState<Role>(heldThere);
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<unknown>();
  const rank: Record<Role, number> = {
    [Role.UNSPECIFIED]: 0,
    [Role.VIEWER]: 1,
    [Role.DEVELOPER]: 2,
    [Role.ADMIN]: 3,
    [Role.OWNER]: 4,
  };
  const lowers = rank[role] < rank[heldThere];
  const holdsElsewhere = member.grants.length + (installRole(member) !== Role.UNSPECIFIED ? 1 : 0) > 1;
  // Taking back the last thing they hold is removing them, which Remove does.
  const last = role === Role.UNSPECIFIED && heldThere !== Role.UNSPECIFIED && !holdsElsewhere;
  const same = role === heldThere;

  const submit = async () => {
    if (busy || same || last) return;
    setBusy(true);
    setProblem(undefined);
    try {
      const target = `${userId} ${ROLE_WORD[role] ?? "none"} ${place || "install"}`;
      const client = createClient(MemberService, transport);
      await withStepUpIfAsked(transport, StepUpAction.CHANGE_ROLE, target, (proof) =>
        client.setMemberRole({ userId, role, project: place, proof }),
      );
      await readAgain();
      onClose();
    } catch (err) {
      if (!dismissed(err)) setProblem(err);
      setBusy(false);
    }
  };

  const kept = t({ message: "Kept", context: "sessions and tokens a change leaves alone" });
  const effects: (readonly [string, ReactNode])[] = lowers
    ? [
        [t`Sessions`, t`Signed out everywhere`],
        [t`API tokens`, t`Revoked`],
        [t`Deploy on push`, t`Off where they turned it on`],
        ...(heldThere === Role.ADMIN ? ([[t`Invites they made`, t`Stop working`]] as const) : []),
      ]
    : [
        [t`Sessions`, kept],
        [t`API tokens`, kept],
        ...(role === Role.ADMIN && member.grants.length > 0
          ? ([[t`Roles in single projects`, t`Replaced by admin`]] as const)
          : []),
      ];
  return (
    <Sheet
      open
      title={t`Change what ${name} holds`}
      subtitle={member.user?.email ?? ""}
      onClose={onClose}
      footer={
        <>
          <span className="dw-sheet-note">
            <Trans>This asks for your passkey, and goes on Activity</Trans>
          </span>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="primary" loading={busy} disabled={same || last} onClick={() => void submit()}>
            <Trans>Change role</Trans>
          </Button>
        </>
      }
    >
      {problem ? (
        <Callout tone="critical" title={t`Nothing changed`}>
          {explain(problem)}
        </Callout>
      ) : null}
      <div className="members-sheet-fields">
        <SelectField
          label={t`Where`}
          value={place}
          onChange={(e) => {
            const next = e.target.value;
            setPlace(next);
            setRole(
              next === ""
                ? installRole(member)
                : (member.grants.find((g) => g.projectId === next)?.role ?? Role.UNSPECIFIED),
            );
          }}
          options={[
            { value: "", label: t`The whole install` },
            // An admin holds every project already, so a project's role is no place for them (ADR 0049).
            ...(installRole(member) === Role.ADMIN
              ? []
              : projects.map((p) => ({ value: p.id, label: p.displayName || p.resource?.name || p.id }))),
          ]}
        />
        <SelectField
          label={t`Role`}
          value={String(role)}
          onChange={(e) => setRole(Number(e.target.value) as Role)}
          options={[
            // An admin holds the whole install, never one project.
            ...(place === "" ? [{ value: String(Role.ADMIN), label: admin }] : []),
            { value: String(Role.DEVELOPER), label: developer },
            { value: String(Role.VIEWER), label: viewer },
            { value: String(Role.UNSPECIFIED), label: t`No role there` },
          ]}
        />
      </div>
      {last ? (
        <Callout
          tone="warning"
          title={t`That's the last thing ${name} holds`}
          actions={
            <Button size="sm" onClick={onRemove}>
              <Trans>Remove {name} instead</Trans>
            </Button>
          }
        />
      ) : same ? null : (
        <Effects label={t`What this changes for ${name}`} rows={effects} />
      )}
    </Sheet>
  );
}

/** Removes someone, after saying what goes with them: read from the control plane, not guessed. */
function RemoveSheet({ member, onClose }: { member: Member; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const readAgain = useReadAgain();
  const time = useTimeWords();
  const now = useControlPlaneNow();
  const name = member.user?.name ?? "";
  const userId = member.user?.id ?? "";
  const plan = useQuery(MemberService.method.planMemberRemoval, { userId }, { retry: false, gcTime: 0 });
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<unknown>();
  const [left, setLeft] = useState<readonly string[]>();

  const submit = async () => {
    if (busy) return;
    setBusy(true);
    setProblem(undefined);
    try {
      const client = createClient(MemberService, transport);
      const gone = await withStepUpIfAsked(transport, StepUpAction.REMOVE_MEMBER, userId, (proof) =>
        client.removeMember({ userId, proof }),
      );
      await readAgain();
      // A change that couldn't be discarded stays planned, which is worth a word before closing.
      if (gone.changesLeft.length > 0) setLeft(gone.changesLeft);
      else onClose();
    } catch (err) {
      if (!dismissed(err)) setProblem(err);
    } finally {
      setBusy(false);
    }
  };

  const p = plan.data;
  return (
    <Sheet
      open
      title={t`Remove ${name}`}
      subtitle={member.user?.email ?? ""}
      onClose={onClose}
      footer={
        left ? (
          <Button variant="primary" onClick={onClose}>
            <Trans>Done</Trans>
          </Button>
        ) : (
          <>
            <span className="dw-sheet-note">
              <Trans>This asks for your passkey, and goes on Activity</Trans>
            </span>
            <Button variant="ghost" onClick={onClose}>
              <Trans>Cancel</Trans>
            </Button>
            <Button variant="danger" icon="trash" loading={busy} disabled={!p} onClick={() => void submit()}>
              <Trans>Remove {name}</Trans>
            </Button>
          </>
        )
      }
    >
      {left ? (
        <Callout
          tone="warning"
          title={t`${name} is removed`}
          actions={
            <Link to="/activity" className="dw-btn dw-btn-sm">
              <span>
                <Trans>Open Activity</Trans>
              </span>
            </Link>
          }
        >
          {plural(left.length, {
            one: "# change they planned couldn't be discarded and stays planned.",
            other: "# changes they planned couldn't be discarded and stay planned.",
          })}
        </Callout>
      ) : (
        <>
          {problem ? (
            <Callout tone="critical" title={t`Nobody was removed`}>
              {explain(problem)}
            </Callout>
          ) : null}
          {plan.error ? (
            <ReadFailed
              error={plan.error}
              title={t`Dockwell can't say what removing ${name} would end`}
              onRetry={() => void plan.refetch()}
              retrying={plan.isFetching}
            />
          ) : !p ? (
            <Loading label={t`Reading what this would end`}>
              <SkeletonFacts count={4} layout="rows" />
            </Loading>
          ) : (
            <dl className="settings-facts members-removal" aria-label={t`What removing ${name} ends`}>
              <dt>{t`Sign-in`}</dt>
              <dd>{t`Passkeys and codes deleted`}</dd>
              <dt>{t`Sessions`}</dt>
              <dd>
                {p.sessions === 0
                  ? t`None open`
                  : plural(p.sessions, { one: "# browser is signed out", other: "# browsers are signed out" })}
              </dd>
              {/* Another way they signed in, at a provider (ADR 0095): said only where there's one. */}
              {p.links.length > 0 ? (
                <>
                  <dt>{t`Provider accounts unlinked`}</dt>
                  <dd>
                    <ul className="members-removal-list">
                      {p.links.map((link) => (
                        <li key={link.provider}>
                          {link.provider}
                          <span className="members-muted">
                            {" · "}
                            {link.account}
                          </span>
                        </li>
                      ))}
                    </ul>
                  </dd>
                </>
              ) : null}
              <dt>{t`API tokens revoked`}</dt>
              <dd>
                {p.tokens.length === 0 ? (
                  t`None`
                ) : (
                  <ul className="members-removal-list">
                    {p.tokens.map((token) => {
                      // Where each acts and as what, as the API tokens page says it (ADR 0071).
                      const where = token.projects.map((place) => place.name || t`a project that's gone`).join(", ");
                      const as =
                        token.role === TokenRole.DEVELOPER
                          ? t`as a developer at most`
                          : token.role === TokenRole.VIEWER
                            ? t`as a viewer at most`
                            : "";
                      return (
                        <li key={token.id}>
                          {token.name}
                          <span className="members-muted">
                            {" · "}
                            {where ? t`in ${where}` : t`every project they hold`}
                            {as ? ` · ${as}` : null}
                            {" · "}
                            {token.lastUsedTime
                              ? t`last used ${time.ago(timestampDate(token.lastUsedTime), now)}`
                              : t`never used`}
                          </span>
                        </li>
                      );
                    })}
                  </ul>
                )}
              </dd>
              {/* Deploy on push acts for whoever turned it on, so it goes with them (ADR 0046). */}
              {p.hooks.length > 0 ? (
                <>
                  <dt>{t`Deploy on push turned off`}</dt>
                  <dd>
                    <ul className="members-removal-list">
                      {p.hooks.map((hook) => (
                        <li key={hook.serviceId}>{hook.serviceName}</li>
                      ))}
                    </ul>
                  </dd>
                </>
              ) : null}
              <dt>{t`Invites revoked`}</dt>
              <dd>
                {p.invites.length === 0 ? (
                  t`None`
                ) : (
                  <ul className="members-removal-list">
                    {p.invites.map((invite) => (
                      <li key={invite.id}>{invite.email}</li>
                    ))}
                  </ul>
                )}
              </dd>
              <dt>{t`Planned changes discarded`}</dt>
              <dd>
                {p.changes.length === 0 ? (
                  t`None`
                ) : (
                  <ul className="members-removal-list">
                    {p.changes.map((change) => (
                      <li key={change.id}>{change.summary}</li>
                    ))}
                  </ul>
                )}
              </dd>
              <dt>{t`Apps and tasks they set up`}</dt>
              <dd>{t`Keep running`}</dd>
              <dt>{t`Their name on Activity`}</dt>
              <dd>{t`Stays`}</dd>
            </dl>
          )}
        </>
      )}
    </Sheet>
  );
}

/** Makes someone else the owner, which only the owner does: it says what changes for both first. */
function TransferSheet({ member, onClose }: { member: Member; onClose: () => void }) {
  const { t } = useLingui();
  const transport = useTransport();
  const explain = useAuthError();
  const readAgain = useReadAgain();
  const name = member.user?.name ?? "";
  const userId = member.user?.id ?? "";
  const [busy, setBusy] = useState(false);
  const [problem, setProblem] = useState<unknown>();

  const submit = async () => {
    if (busy) return;
    setBusy(true);
    setProblem(undefined);
    try {
      const client = createClient(MemberService, transport);
      await withStepUpIfAsked(transport, StepUpAction.TRANSFER_OWNERSHIP, userId, (proof) =>
        client.transferOwnership({ userId, proof }),
      );
      await readAgain(true);
      onClose();
    } catch (err) {
      if (!dismissed(err)) setProblem(err);
      setBusy(false);
    }
  };

  return (
    <Sheet
      open
      title={t`Make ${name} the owner`}
      subtitle={member.user?.email ?? ""}
      onClose={onClose}
      footer={
        <>
          <span className="dw-sheet-note">
            <Trans>This asks for your passkey, and goes on Activity</Trans>
          </span>
          <Button variant="ghost" onClick={onClose}>
            <Trans>Cancel</Trans>
          </Button>
          <Button variant="danger" loading={busy} onClick={() => void submit()}>
            <Trans>Transfer ownership</Trans>
          </Button>
        </>
      }
    >
      {problem ? (
        <Callout tone="critical" title={t`Ownership didn't move`}>
          {explain(problem)}
        </Callout>
      ) : null}
      <Effects
        label={t`What this changes`}
        rows={[
          [name, t`Becomes the owner`],
          [t`You`, t`Become an admin, and stay signed in`],
          [t`Your CLI access on the control server`, t`Revoked`],
          [t`Undoing this`, t`Only ${name} can`],
        ]}
      />
    </Sheet>
  );
}
