# TextField

A labelled single-line input with an optional hint, error, prefix, suffix or icon.

**You provide** `label`, the input props (`value`, `onChange`, `placeholder`), and `mono` for anything a machine parses: paths, hosts, image tags, commands.

- Labels sit above the field in sentence case. Mark the uncommon case: `optional` rather than an asterisk on every required field.
- Hints say what happens with the value ("Dockwell waits for 200 before sending traffic"). Errors replace the hint and say how to fix the problem, never just that it exists.
- Use `prefix` or `suffix` for fixed parts of a value (`github.com/`, `.preview.acme.dev`) so people type only the variable part.
- Validate on blur or submit, not on every keystroke.
