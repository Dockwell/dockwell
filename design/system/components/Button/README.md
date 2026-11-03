# Button

Runs an action; `primary` is ink, not a hue, and appears at most once per view.

**You provide** the label (a verb that names the result: "Apply plan", "Restart", "Remove server"), an optional `icon`, and `kbd` when the action has a shortcut.

- Use `primary` for the one action the view exists for (Deploy, Apply plan). Everything else is `secondary` or `ghost`.
- Use `danger` only for actions that destroy data or capacity, and only inside a confirmation step or a plan.
- While work runs, set `loading` and change the verb to its progressive form ("Deploy" becomes "Deploying"). Keep the width stable.
- An icon-only button needs `aria-label`. Prefer a labelled button in toolbars wider than 480px.
- The same action keeps its name everywhere: a button "Roll back" produces a toast "Rolled back to r127", never "Reverted".
