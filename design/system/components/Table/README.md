# Table

Dense rows of resources with sortable meaning: tabular numbers, mono machine values, one status column.

**You provide** `columns` (`key`, `header`, optional `width`, `align`, `mono`, `muted`, `render`), `rows`, and `onRowClick` with `selectedKey` when rows open details.

- Put the name first, status second. Numbers align right. Machine values (images, IPs, domains) are `mono`.
- For a two-line first cell use `dw-cell`, `dw-cell-title` and `dw-cell-sub`.
- `comfortable` rows are 48px, `compact` 36px. Headers are sentence case in `caption`, never all caps.
- An empty table says what would appear and how to add the first one, through `empty`.
- While its first rows are on their way, `loading` draws `loadingRows` rows of shapes under the real header (as many as last time, where the page knows), and says `loadingLabel` once to assistive technology. Each column's shape follows the column, or its `skeleton`: a name wide, numbers narrow at the right, a status as a pill, a row's actions as a small button. Rows it already has stay while it loads again. `slow` is the one line a long wait gets, above the shapes.
