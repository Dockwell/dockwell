-- Each person's own time zone for how the panel shows times and where new
-- schedules start: an IANA zone name, or empty for the browser's own (#834).
ALTER TABLE users ADD COLUMN time_zone TEXT NOT NULL DEFAULT '';
