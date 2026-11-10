# 0059. What the panel knows of what you may do

- Status: Accepted
- Date: 2026-11-10

## Context

Roles are checked on the server (ADR 0048, ADR 0049): each method says the lowest role that may call it in an open environment and in a protected one, and what its objects are, and the interceptor checks that against the caller's grants where the call's objects are. The engine then checks each item of a change.

The panel knew less. The sign-in state carried one role, the caller's on the whole install, which is empty for someone who holds projects alone. So the panel offered what the API then refused: Cancel build to a project's viewer, or to a developer in a protected environment, and a server's Updates and Firewall tabs to someone below an admin. Each screen that guessed did so in its own way, and none could know whether an environment was protected.

The rule is the same as everywhere else: the panel uses only the public API, and nothing is UI-only. Whatever the panel knows about what you may do, a script can know too, and it must be what the server enforces, not a copy that drifts.

## Decision

**The API says two more things, and the panel works the rest out from the schema.**

1. **The sign-in state carries the caller's grants.** `GetAuthStateResponse.grants` lists each grant the signed-in person holds: a role, and the project it is on, or no project for the whole install. `user.role` stays what it was, the role on the install.
2. **A resource says whether it is protected.** `Resource.protected` is true for an environment that is protected, resolved as the server resolves it: what its spec says, or by its name for one from before protection was written. The panel already reads the environments, so no call is added.
3. **Who may call a method is read from the method.** The generated client holds each method's `min_role`, `min_role_protected`, `reach` and `effect`, the same options the interceptor reads. One helper in the panel, `may`, answers "may I call this method here" from them and from 1 and 2. No table of roles is written by hand, so the panel's answer changes when the schema does.

**What `may` answers** is the interceptor's two checks, in its order:

- The lowest role the method needs anywhere, against the highest the caller holds anywhere.
- For a method whose objects are a project's, and a place given: the role held at that place, a grant on its project or on the install, against what the method needs there, `min_role_protected` where the environment is protected. A place in no project is an admin's. A place the caller holds nothing in is out of reach.

A method whose objects are the install's is an admin's by the schema's own rule, and one that reads what's shared or named needs only the first check.

**What it does not answer** is what the engine decides from a plan's contents: host access, a project's own lifecycle, a name another project holds. A plan says that itself, in `Change.waits`, and the plan sheet shows it. `may` never says yes where the server says no for the method and place; it can say yes to planning a change whose plan then waits for an admin, which is the design (ADR 0049).

**How the panel shows a no.** One rule, from the content rules' "disabled controls say why nearby":

- **An action on something you can see is shown disabled, with the reason.** A button, a menu item or a switch stays where it is, at the disabled opacity, and says who can do it: "Only an admin or the owner can do this", or "This environment is protected, so an admin or the owner does this". You learn the thing exists and whom to ask.
- **A way to a place you can't read is left out.** A tab, a rail entry or a command-bar entry whose screen's read you may not call isn't drawn, as a list leaves out what's out of your reach. Opening its address still shows the server's refusal, in the words the panel already has for it.
- **The command bar leaves out both kinds:** an entry is something to run, and one that can't run is noise in a search.

**Every method has a decision.** `web/src/features/auth/methods.ts` names each method of the API once, as one of: `open` (before sign-in, or your own account), `read` (a read whose list the server narrows, or whose refusal the screen shows), `may` (the panel asks `may` before it offers it) or `unused` (the panel doesn't call it). A test walks the generated services and fails on a method with no decision, on a decision for a method that's gone, on a call the panel makes to a method marked unused, and on a write, or a read that's an admin's, that the panel calls without asking `may` for it. So a new method fails the panel's tests until someone decides.

## Consequences

- The sign-in state grows by a few grants per person. It is read at sign-in and when the panel wakes, as before. A grant changed while the panel is open shows at the next read; until then the server still refuses what the panel wrongly offers, with the words it had.
- An API token's caller learns the same from the same two reads, though `GetAuthState` answers for a session: a token's maker reads their grants in the panel.
- The plan sheet's waiting and approval keep coming from the server's `waits` and `approves`; `may` doesn't replace them.
- Screens stop guessing from the install role. `useMay` is the one way to ask.

## Alternatives

- **A new call, "may I", answered by the server per method and object.** It would be exact, engine checks included for a plan, but a screen with twenty controls would ask twenty questions, and the answer for a plan already exists on the plan.
- **A hand-written table of what each role sees.** It is what drifted before there was a table at all.
- **Hide everything you can't do.** A viewer would see an app with no way to tell that it can be deployed by someone else, and support questions start with "where is the button".
