# Catalog artwork

These are the projects' own logos and published previews, stored locally so catalog browsing sends no requests to their hosts. They are separate works with their original rights retained, not Dockwell-authored illustrations. See [NOTICE.txt](NOTICE.txt) and the source notices in `licenses/`.

[manifest.json](manifest.json) records each file's source, publisher, upstream commit when available, SHA-256 of the original and bundled file, dimensions, and transformations. Logos fit inside a 128 by 128 canvas. Previews retain their aspect ratios and fit within 1120 by 760 pixels. RustFS uses one still frame of its published demonstration. No screen content has been redrawn.

To update artwork, fetch the recorded source or a new project-published source, inspect it, preserve the source's notices, and update the manifest's hashes. Keep captions accurate about included tools and compatible clients. Reference the resulting path from the entry's `icon` or `screenshots` fields. Do not add external image URLs to entries or loosen the panel's content security policy. Run the catalog tests and measure the panel with `scripts/check-panel-size.sh` after building.
