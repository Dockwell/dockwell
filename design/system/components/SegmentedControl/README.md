# SegmentedControl

Switches between two to five views of the same content.

**You provide** `options` (`value`, `label`, optional `icon` and `count`), `value` with `onChange`, and a `label` naming the group for screen readers.

- Use for environment, time range or layout switches. More than five options or options of different kinds belong in Tabs or a select.
- Arrow keys move between options; only the selected option is in the tab order.
- The selected segment is raised with `shadow-float` on `surface-raised`; the track is `surface-sunken`. Outer radius equals inner radius plus padding.
