# Sheet

A panel that slides in from the right for plans, settings and details, keeping the page visible behind it.

**You provide** `title`, `subtitle`, `children`, a `footer` with actions (the primary action on the right), `open` and `onClose`.

- Plans always open in a Sheet: PlanDiff in the body, Cancel and Apply plan in the footer, and a note on duration and undo window at the footer's left.
- The sheet floats 8px from the edges with `radius-lg` and `shadow-sheet`. Esc and the scrim close it; unsaved edits ask first.
- Default width is `sheet-width` (560px). Wider content belongs on a page.
