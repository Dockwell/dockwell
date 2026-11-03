# PlanDiff

Shows exactly what a change will do before it runs: what is created, changed, replaced or destroyed, and the impact.

**You provide** `resources` (`action`, `kind`, `name`, `changes` of `path`, `from`, `to`) and an `impact` line (servers affected, rollout, expected downtime).

- Every deploy, config edit, firewall rule and fleet action shows a plan first. The plan is the only path to Apply.
- `+` create, `~` update, `±` replace, `−` destroy, in diff tokens. Old values are struck through as well as coloured, so the diff reads in grayscale.
- Secrets render as hidden, never as values, even to admins.
- Destroyed resources start collapsed; everything else starts open.
