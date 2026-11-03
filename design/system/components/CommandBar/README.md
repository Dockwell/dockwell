# CommandBar

The keyboard front door: ⌘K finds any server, app, page or action and runs it.

**You provide** `items` (`id`, `label`, `group`, optional `icon`, `hint`, `kbd`), `onSelect`, `open` with `onClose`, and the current `scope`.

- Labels are full sentences of intent ("Restart api-gateway in staging"), so typing any part of the intent finds them.
- Matching is fuzzy on characters in order; matched characters are bolded with a cobalt underline.
- Destructive or production-changing items open a plan; they never run straight from the bar.
- Arrow keys move, Enter runs, Esc closes. Groups show only while the query is empty.
