# Toast

Confirms a change that just happened and keeps the way back one keystroke away.

**You provide** `title` (past tense, matching the button that triggered it), an optional `description`, an `action` (usually Undo with `⌘Z`), `onClose`, and `duration` when it should dismiss itself.

- Success toasts dismiss after `duration`; the timer pauses while hovered or focused. Failure toasts stay until closed.
- Never put information only in a toast. The Timeline records everything a toast announces.
- Stack toasts bottom-right in `dw-toaster`, newest at the bottom, at most three.
