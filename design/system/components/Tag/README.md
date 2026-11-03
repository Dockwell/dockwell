# Tag

A key:value label or selector, set in mono because machines read it.

**You provide** `label` and `value` (`region` and `eu`), or plain `children`. Add `onRemove` when the tag is part of an editable selector.

- Use for server labels, pool membership, branches, IPs and filter chips.
- `tone="cobalt"` marks a tag the system manages (a pool, a build server). Everything else is neutral.
- Tags never carry status. Use StatusBadge for state.
