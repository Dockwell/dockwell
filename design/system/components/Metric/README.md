# Metric

One live number with its unit, its recent shape and a line of context.

**You provide** `label`, `value` (formatted, short: "11.2k"), `unit`, optional `data` for the sparkline, `threshold` for a limit line, and a `caption` that says what the number covers.

- Values use tabular figures, so they do not shift as they update. Round to what a person can act on (38.4%, not 38.417%).
- Use `deltaTone` only when the change is a problem or a recovery. A neutral change stays ink-muted.
- Four Metrics in a row is the most a view should carry; more belongs on a dashboard.
