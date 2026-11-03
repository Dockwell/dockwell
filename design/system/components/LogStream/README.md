# LogStream

Live, searchable log lines that follow new output and pause the moment someone scrolls up to read.

**You provide** `lines` (`time`, `source`, `level`, `text`), `title`, `live` while streaming, and an optional `toolbar` for filters.

- Following stops automatically when the reader scrolls up; the header shows Paused and the button switches to Follow.
- Warnings and errors carry a glyph in their own column, and errors get a faint critical band, so level reads without colour.
- Times are wall-clock with milliseconds in the viewer's time zone. Source names the replica or server.
