# FleetGrid

Every server as one tile, grouped by region or pool, so the fleet's state reads in a single glance.

**You provide** `groups` (each with `label`, `meta`, `servers`), or a flat `servers` list, plus `selectedId` and `onSelect` to open a server.

- Healthy tiles stay quiet (a light healthy tint); only problems get full colour and a glyph. A calm grid means a calm fleet.
- The readout under the grid names the hovered or focused server, its state, load and IP, so no tooltip covers neighbouring tiles.
- Use `size="sm"` beyond about 200 servers per group. Beyond 2,000 servers, group by pool and show counts instead.
- Every tile is a button with an accessible name; arrow keys are not needed because Tab order follows the grid.
