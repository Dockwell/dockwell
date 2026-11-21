// SPDX-License-Identifier: AGPL-3.0-only

package docker

// PHP-FPM pins for websites. Keep the runtime's release and pin in step when
// refreshing a version. The agent also protects these local images in cleanup.
const (
	// WebsitePHP85Image is the pinned PHP 8.5 FPM image for websites.
	WebsitePHP85Image = "wordpress:7.1.3-php8.5-fpm-alpine@sha256:8697f1b7fbedab67817bfe66f1cc8da15fc8f0e556c516ffab151fe3d0b2116d"
	// WebsitePHP84Image is the pinned PHP 8.4 FPM image for websites.
	WebsitePHP84Image = "wordpress:7.1.3-php8.4-fpm-alpine@sha256:c6c6f760a4aec3d16c192c45d52d1942646d41a985f49433757d595305018bfd"
	// WebsitePHP83Image is the pinned PHP 8.3 FPM image for websites.
	WebsitePHP83Image = "wordpress:7.1.3-php8.3-fpm-alpine@sha256:82d93a279778e5307902813c402b9118521e8104459a0e350ed719f6d6c4d1ab"
	// WebsitePHP82Image is the pinned PHP 8.2 FPM image for websites.
	WebsitePHP82Image = "wordpress:7.1.3-php8.2-fpm-alpine@sha256:8f223dd023f58fcab49a909899ec07ea058cf731fc0947fdee881285da74b9dc"
)
