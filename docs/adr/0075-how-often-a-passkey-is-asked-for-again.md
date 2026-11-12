# 0075. How often a passkey is asked for again

- Status: Proposed
- Date: 2026-11-12

## Context

A step-up is a fresh passkey assertion that confirms one action, on one target, in one session, once, within a minute ([ADR 0022](0022-secrets-encrypted-at-rest.md#revealing-a-value-and-changing-how-you-sign-in-a-step-up)). Since then the list has grown to twenty-one actions: revealing a secret, applying a plan as good as root ([ADR 0021](0021-the-docker-driver.md#host-access)), saving, making, moving, deleting, restoring and uploading a file that's a way back onto a server ([ADR 0032](0032-files-on-a-server.md), [ADR 0045](0045-a-file-manager-on-a-server.md)), opening a terminal ([ADR 0037](0037-a-terminal-on-a-server.md)), ending a process, opening an app's web UI ([ADR 0061](0061-opening-an-apps-web-ui-through-the-panel.md)), making an API token, inviting someone, adding a server, setting the panel's domain and signing in on it ([ADR 0063](0063-the-panels-domain-set-from-the-panel.md)), approving someone else's change in a protected environment ([ADR 0049](0049-roles-and-invites-by-project.md)), and the sign-in methods themselves ([ADR 0011](0011-claim-and-sign-in.md)).

A solo owner working through a server's files, terminal and secrets taps their passkey many times an hour. The owner asked (#522) to be able to opt out. ADR 0032 named the cost already: a prompt that comes often teaches people to tap through the one that guards something. GitHub's sudo mode is the common answer: one confirmation covers sensitive actions for a while.

What has to hold whatever anyone chooses:

- A stolen session still can't make itself a way to sign in, which outlives its revocation, nor quietly change how often it's asked.
- An approval in a protected environment is still a person, just now, for that plan: that is what protection is for.
- A team's owner can keep everyone at a level they choose.
- It's enforced by the control plane, and an API token gets nothing from it.

## Decision

### A choice per person

Each person chooses, in Profile and security, how often their passkey is asked for again (`AuthService.SetStepUpPolicy`, `StepUpPolicy`):

| Choice | What confirms an action |
| --- | --- |
| **Each time** (`EACH_TIME`, the default) | A step-up for that action, as before. |
| **Once an hour** (`HOURLY`) | A passkey in this session within the last hour: signing in with one, or any step-up. |
| **Only when I sign in** (`AT_SIGN_IN`) | Signing in, for the session's life, where it began with a passkey or an authenticator code; otherwise as for once an hour. |

- **The window is the session's.** `sessions.passkey_at` (migration 0035) is when a passkey last confirmed the session: a passkey sign-in sets it, as claiming and accepting an invite do, since each makes a passkey with user verification, and every step-up sets it again in the transaction that checks the assertion. The hour runs from then and doesn't slide with use, so "once an hour" asks at most once an hour while someone works. A restart keeps it.
- **A sign-in that confirms** is one with a passkey, a claim, an invite, or an authenticator code. A recovery code and a link from `dockwell recover` don't: they're for getting back in, each is a security event, and the person adds a passkey next, which then opens the hour.
- **The choice is the person's, on every session they have,** open ones included. Choosing a looser one is a moment to revoke sessions you don't know, which Profile shows beside it.
- **Changing it asks for the passkey each time** (`STEP_UP_ACTION_SET_STEP_UP_POLICY`, 240, on the person), whichever way it goes. The step-up that confirms it sets `passkey_at`, so a new "once an hour" starts its hour then.
- **It isn't managed state.** As with passkeys and codes (ADR 0011), it's access: no revision, and `auth.step_up_policy` in the audit log names the old choice and the new.

### What asks each time, whatever the choice

Each of these needs a proof just now, for itself, as before:

- **Changing how you sign in:** adding or removing a passkey, setting up or turning off codes, and making new recovery codes. Otherwise a stolen session makes itself a way back in, and every later check passes with it.
- **Changing the choice, or the install's floor** (`STEP_UP_ACTION_SET_STEP_UP_FLOOR`, 241). Otherwise a stolen session loosens its own checks first.
- **Approving someone else's change in a protected environment** (`STEP_UP_ACTION_APPROVE_CHANGE`, 114), which includes a change a narrowed token planned ([ADR 0071](0071-api-tokens-narrowed-to-projects-and-a-role.md)). The approval is the point of protection, and it's bound to the plan's fingerprint.

Profile says so in one line beside the choice.

### An install-wide floor

The owner sets the least anyone may choose, in Settings → Security (`Settings.step_up_floor`, `SettingsService.SetStepUpFloor`): any of the three (the default, as a solo owner is the common case), at least once an hour, or each time.

- **What applies is the stricter** of a person's choice and the floor. A choice below a floor raised later reads as the floor, and Profile says the install asks at least that, with who sets it.
- **Choosing below the floor is refused** (`below_floor`), and the panel offers those choices off, with why (ADR 0059).
- **Only the owner sets it,** in a session, with a passkey each time, as a check in the handler, as for the panel's domain (ADR 0063). It's on the activity log as `settings.step_up_floor`, with what it replaced (ADR 0042). It applies at once, to every session.

### Enforced where the proof is checked

The control plane checks it in `auth.Service.HoldStepUp` and `UseStepUp`, where every proof is spent, so no method needs a rule of its own:

- **An empty proof passes when the session covers the action:** a signed-in session, an action not in the list above, and the window open for what applies. Otherwise it's refused with `needs_step_up`, as before, and the person steps up. A proof given still works as before, and opens the hour.
- **Only a session.** A call with an API token is refused before the window is read, and no method marked `needs_session` opens to a token.
- **Where a write of the action's own already holds a transaction,** as revealing a secret and making a token do, the window is read and recorded in it (`UseStepUpIn`), so what confirmed it is on record with what it did, or neither is.
- **Where the agent decides** whether a passkey is needed, it decides as before, and the control plane answers for the window:
  - a save sends the confirmed path the panel has from `ReadFile` where the window covers it, as it does with a proof;
  - making, moving, deleting, restoring and uploading call the agent again, once, with the paths confirmed, when it asks for a passkey and the window covers them;
  - a shell in a container opens again, confirmed, when the agent finds it as good as root and the window covers it.

  The agent's protocol doesn't change: it still writes only where its own walk ends at the path confirmed.
- **Nothing asked, nothing missed.** A covered action isn't "asked for a passkey", so it never becomes `*_unconfirmed` on Activity.
- **The panel's step-up stays as it is.** Every action already calls without a proof first, and asks for a passkey only when the answer is `needs_step_up`. A covered action doesn't answer that, so no passkey dialog opens and closes.

### The audit log says how each was confirmed

| Action | Detail |
| --- | --- |
| `auth.step_up` | with passkey `pky_…`, from where, as before |
| `auth.step_up_hour` | within an hour of the passkey at a time, from where |
| `auth.step_up_sign_in` | by signing in, how and when, from where |

Each names the action and its target, as a step-up does. What the action records itself stays: `secret.reveal`, `token.create`, a change's apply and so on.

### The API

- `AuthService.GetStepUpPolicy` returns what you chose, the floor, what applies, and this session's window: when it opened and when it ends. `SetStepUpPolicy` takes the choice and a proof. Neither is open to a token, as with all of `AuthService`.
- `SettingsService.SetStepUpFloor` takes the floor and a proof; `GetSettings` returns it.

### What each choice accepts

- **Each time:** nothing new. A stolen session still does what a session does without a passkey (plans and applies most changes), as [sign-in.md](../security/sign-in.md#known-gaps) says.
- **Once an hour:** whoever has the session within an hour of its last passkey, through a stolen cookie or an unlocked browser left open, reveals secrets, opens root shells, applies root changes, makes tokens and invites people without a tap, until the hour ends.
- **Only when I sign in:** the same for the session's whole life, up to 7 days, and a session begun with a phished authenticator code gets it too. A token or an invite it makes outlives the session.
- **A floor that allows all three:** each member may accept the above for themselves.

### What this changes in earlier records

- ADR 0011 and 0022: a step-up confirms one action once for people who choose "Each time"; for others, the session's window can stand in, except for the sign-in methods. ADR 0022's "a passkey sign-in confirms nothing" still holds for adding a passkey.
- ADR 0032 and 0045: a write that's a way back in asks as before, and a session's window can stand in for its passkey. A save's ten minutes stay for "Each time".
- ADR 0037: a terminal takes a passkey each time it opens, or the session's window.
- ADR 0049: approving someone else's protected change asks each time, whatever anyone chooses.
- ADR 0061 and 0063: opening an app's UI, setting the panel's domain and signing in on it take the session's window as any other step-up does.

## Consequences

- A solo owner can work through files, a terminal and secrets with one tap an hour, or one at sign-in.
- Two columns and a setting: `users.step_up_policy`, `sessions.passkey_at` (migration 0035), and `auth.step_up_floor`. Two step-up actions (240 and 241), three methods, and four audit actions.
- Every step-up writes the session's row, in the transaction that already stores the passkey's count.
- A team that wants today's behaviour sets the floor to "Each time", one change.
- A covered action costs one transaction to read the floor and write its record. Someone on "Each time" pays nothing: their choice is on the principal, and the floor isn't read.
- Changing one person's choice for them, or showing each member's choice in Members, isn't decided here.
