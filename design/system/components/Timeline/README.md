# Timeline

One line of time that correlates deploys, config changes, alerts, servers and backups, with undo where it is possible.

**You provide** `events` (`kind`, `title`, `time`, optional `status`, `tone`, `revision`, `actor`, `meta`, `undoable`) and `onUndo`.

- Titles are past-tense facts for finished work ("Deployed api-gateway") and present-progressive for work in flight ("Deploying web").
- Mark an event with `tone` only when it is an alert or an outcome worth noticing. Routine events stay neutral.
- `undoable` events offer Undo (or a specific verb like Roll back), which opens a plan for the reverse change.
