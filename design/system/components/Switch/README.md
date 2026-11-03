# Switch

Turns a setting on or off immediately, with no save step.

**You provide** `label`, an optional one-sentence `description` of the effect, and `checked` with `onChange` (or `defaultChecked`).

- Only for changes that apply at once. Settings behind a Save button use a checkbox or a form; anything that changes running workloads goes through a plan.
- The description says what happens when on, in plain words ("Every push to main builds and ships to production").
- A disabled switch explains why in its description (plan, permission, dependency).
