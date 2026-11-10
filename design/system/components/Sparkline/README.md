# Sparkline

A small line showing the shape of a series, without axes, for table cells, metrics and inline text.

**You provide** `data` and, when values have a known range, `min` and `max` (0 and 100 for percentages) so lines are comparable across rows.

- Ink is the default. Use `cobalt` for live traffic and a status tone only when the series itself is in that state (a degraded service's CPU).
- `threshold` draws a dashed warning line at a limit.
- `null` in `data` is a gap, where nothing was heard: the line breaks there, and a point alone between gaps is a dot. Never draw a gap as zero, which reads as a calm stretch.
- `peaks` gives each point's highest beside `data`'s averages, as over an hour of history: a light band from the line up to it, so a point that averaged 20 and touched 100 doesn't read as calm.
- Sparklines show shape, not values. Put the number beside it.
