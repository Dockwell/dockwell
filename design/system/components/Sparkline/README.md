# Sparkline

A small line showing the shape of a series, without axes, for table cells, metrics and inline text.

**You provide** `data` and, when values have a known range, `min` and `max` (0 and 100 for percentages) so lines are comparable across rows.

- Ink is the default. Use `cobalt` for live traffic and a status tone only when the series itself is in that state (a degraded service's CPU).
- `threshold` draws a dashed warning line at a limit.
- Sparklines show shape, not values. Put the number beside it.
