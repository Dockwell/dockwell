# Tabs

Moves between sections of one resource, such as a service's overview, deployments, logs and settings.

**You provide** `tabs` (`id`, `label`, optional `count` and `countTone`) and `value` with `onChange`.

- Tabs change what is shown below them, never what the page is about. Use the rail or ScopeBar for that.
- Counts are numbers only. Use `countTone` when the count is a problem (open alerts), never for totals.
- The active tab gets an ink rule; keep labels to one or two words.
