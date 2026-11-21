# Skeleton

The shape of what is on its way, drawn where it will land and at its size, so nothing moves when it arrives. A screen that waits shows these, never a lone sentence (interaction principle 7 in the [brand book](../../README.md)).

**You provide** a `shape`: `line` (with the `text` style it stands for: `title`, `heading`, `body`, `label`, `caption`, `code`, `code-sm` or `metric`), `block` (a panel, chart, field or button, with `height`), `pill` (a status badge or tag) or `circle` (an icon or glyph, with `size`), and a `width` where the real thing has one.

- Wrap a section's shapes in `Loading`, with a `label` such as "Loading images". The region is `aria-busy` and says the label once, politely, after the shapes' delay; the label is never shown.
- Give `Loading` a `slow` line where a wait can run long, in the product's voice and naming what the wait is: "Docker is measuring disk use on this server". It shows in place after 3 seconds, above the shapes, so it is in sight however many there are. No spinner, and never a second line.
- A table on its way is a `Table` with `loading`, not a skeleton of its own: it draws each column's shape under the real header.
- `SkeletonRows` draws a list's rows (phone lists, lists that aren't tables): a title over a sub line, with an optional `lead` icon or mark and an `end` pill, value or chevron.
- `SkeletonFacts` draws label and value pairs, in a `grid` (a detail page's facts) or in `rows` (facts in an aside).
- `SkeletonCard` draws a panel: its real `title` where it is known, then lines, a block of `height`, or its own shapes.
- `SkeletonField` draws a form field whose choices come from a read (a server, a connection, a storage picker): its label over its control, at their sizes. A field the person types in draws for real at once.
- `SkeletonTabs` draws a tab row whose tabs depend on what loads. Tabs that don't, draw for real at once.
- Shapes wait 150 ms before they show, so a fast answer never flashes them, then pulse slowly in `skeleton`. Under `prefers-reduced-motion` they hold still.
- Show what you already know for real: a page's head, tabs, toolbar, filters and a card's title draw at once and stay usable. A refetch keeps the last data; only its refresh control shows that it is working.
- A failed read replaces the shapes with a `Callout`: the fact, the consequence and the fix, with Retry.
