// SPDX-License-Identifier: AGPL-3.0-only

package website

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

const (
	// PHPService is the service that runs a PHP website's code, beside its web server.
	PHPService = "php"
	// PHPTarget is where a PHP website's folder is in both its containers, at the
	// same path, as the web server names each script to PHP by where it is. It's
	// where the image declares its volume, so a mount covers it and Docker makes none.
	PHPTarget = "/var/www/html"
	// phpPort is what PHP-FPM listens on.
	phpPort = 9000
	// defaultUploadMB is the most a request may send when a website says nothing.
	defaultUploadMB = 64
	// maxUploadMB bounds what a website may ask, past which a request is a disk's worth.
	maxUploadMB = 4096
	// DefaultPHPUser is who PHP runs as unless a website says: www-data in the images,
	// by number, as a folder's owner is.
	DefaultPHPUser = "82:82"
)

// Runtime is a version of PHP Dockwell offers: the release its image has, and
// the image, by the digest of its multi-platform index.
type Runtime struct {
	Version, Release, Image string
}

// PHPExtensions are what the images add to PHP's own: the official WordPress
// image's set, which most PHP sites need, with opcache tuned for a site.
var PHPExtensions = []string{"opcache", "mysqli", "gd", "intl", "imagick", "zip", "exif", "bcmath"}

// phpRuntimes are the versions of PHP a website can run, newest first: the official
// WordPress image's PHP-FPM on Alpine, at WordPress 7.1.3, pinned by the digest of
// each one's multi-platform index (ADR 0039). It's run with PHP's own entrypoint,
// so it never copies WordPress into a site's folder. Pin a new set to a release at
// least 14 days old.
var phpRuntimes = []Runtime{
	{"8.5", "8.5.12", "wordpress:7.1.3-php8.5-fpm-alpine@sha256:8697f1b7fbedab67817bfe66f1cc8da15fc8f0e556c516ffab151fe3d0b2116d"},
	{"8.4", "8.4.27", "wordpress:7.1.3-php8.4-fpm-alpine@sha256:c6c6f760a4aec3d16c192c45d52d1942646d41a985f49433757d595305018bfd"},
	{"8.3", "8.3.36", "wordpress:7.1.3-php8.3-fpm-alpine@sha256:82d93a279778e5307902813c402b9118521e8104459a0e350ed719f6d6c4d1ab"},
	{"8.2", "8.2.35", "wordpress:7.1.3-php8.2-fpm-alpine@sha256:8f223dd023f58fcab49a909899ec07ea058cf731fc0947fdee881285da74b9dc"},
}

// PHPRuntimes are the versions of PHP a website can run, newest first.
func PHPRuntimes() []Runtime { return slices.Clone(phpRuntimes) }

// PHPRuntime is a version's runtime, with ok false for one Dockwell doesn't offer.
func PHPRuntime(version string) (Runtime, bool) {
	i := slices.IndexFunc(phpRuntimes, func(r Runtime) bool { return r.Version == version })
	if i < 0 {
		return Runtime{}, false
	}
	return phpRuntimes[i], true
}

func checkPHP(p *resourcev1.WebsitePhp) error {
	folder := &resourcev1.WebsiteFolder{Root: p.GetRoot(), Index: p.GetIndex()}
	if err := checkFolder(folder); err != nil {
		return err
	}
	p.Root, p.Index = folder.GetRoot(), folder.GetIndex()
	p.Version = strings.TrimSpace(p.GetVersion())
	if p.GetVersion() == "" {
		p.Version = phpRuntimes[0].Version
	}
	if _, ok := PHPRuntime(p.GetVersion()); !ok {
		var offered []string
		for _, r := range phpRuntimes {
			offered = append(offered, r.Version)
		}
		return fmt.Errorf("PHP %s isn't one Dockwell offers. Choose one of %s", p.GetVersion(), strings.Join(offered, ", "))
	}
	if p.GetUploadLimitMb() > maxUploadMB {
		return fmt.Errorf("an upload limit of %d MiB is more than the %d MiB a website may take. Give a smaller one", p.GetUploadLimitMb(), maxUploadMB)
	}
	user, err := checkPHPUser(p.GetUser(), p.GetRoot())
	if err != nil {
		return err
	}
	p.User = user
	return nil
}

// checkPHPUser reads who PHP runs as, as "uid:gid", or a number alone for both,
// and writes it as Dockwell keeps it. Empty is DefaultPHPUser. Neither may be root's,
// however it's written, as anything a site's code does would be root's on the folder.
func checkPHPUser(user, root string) (string, error) {
	user = strings.TrimSpace(user)
	if user == "" {
		return DefaultPHPUser, nil
	}
	if !strings.Contains(user, ":") {
		user += ":" + user
	}
	uid, gid, err := resource.ParseOwner(user)
	if err != nil {
		return "", fmt.Errorf("PHP runs as a user and group by number, as uid:gid, such as 1000:1000, which stat -c %%u:%%g %s says of the folder's owner", root)
	}
	if uid == 0 || gid == 0 {
		return "", errors.New("PHP doesn't run as root, nor in root's group, as anything a site's code does would be root's on the folder. Give the folder owner's, such as 1000:1000")
	}
	return resource.FormatOwner(uid, gid), nil
}

// phpIndex is the files a PHP website looks for in a folder, in order.
func phpIndex(p *resourcev1.WebsitePhp) []string {
	if len(p.GetIndex()) > 0 {
		return p.GetIndex()
	}
	return []string{"index.php", "index.html"}
}

// uploadMB is the most a PHP website's request may send.
func uploadMB(p *resourcev1.WebsitePhp) uint32 {
	return cmp.Or(p.GetUploadLimitMb(), defaultUploadMB)
}

// phpServing is what serves a PHP website: its web server, which serves its files
// and passes PHP to PHP-FPM beside it, on a network of their own.
func phpServing(p *resourcev1.WebsitePhp, out *Serving) {
	out.Route.Service, out.Route.Port = WebService, Port
	out.Networks[SiteNetwork] = &resourcev1.Network{}
	web := webService(phpConfig(p))
	app := web.GetApp()
	user := cmp.Or(p.GetUser(), DefaultPHPUser)
	app.Mounts = []*resourcev1.Mount{{
		Source: &resourcev1.Mount_HostPath{HostPath: p.GetRoot()}, Target: PHPTarget, ReadOnly: true,
		// A folder that isn't there is made as PHP's, which writes in it.
		MakeOwner: user,
	}}
	out.Services[WebService] = web

	rt, _ := PHPRuntime(p.GetVersion())
	limit := strconv.FormatUint(uint64(uploadMB(p)), 10) + "M"
	out.Services[PHPService] = &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image: rt.Image,
		// PHP's own entrypoint: the image's would copy WordPress into a folder with none.
		Entrypoint: []string{"docker-php-entrypoint"},
		Command: []string{
			"php-fpm", "-d", "upload_max_filesize=" + limit, "-d", "post_max_size=" + limit,
			// No .user.ini: what a site's folder holds doesn't change how PHP runs it.
			"-d", "user_ini.filename=",
		},
		// FPM's master too, not root: it takes no capability, and binds 9000.
		User:     user,
		CapDrop:  []string{"ALL"},
		Networks: []string{SiteNetwork},
		// Read-write, as a site's code writes uploads and caches beside itself.
		Mounts: []*resourcev1.Mount{{
			Source: &resourcev1.Mount_HostPath{HostPath: p.GetRoot()}, Target: PHPTarget, MakeOwner: user,
		}},
		// Where PHP keeps an upload until the script takes it, in memory, as big as the
		// upload limit and a little more.
		Tmpfs:       []*resourcev1.Tmpfs{{Target: "/tmp", Size: strconv.FormatUint(uint64(uploadMB(p))+16, 10) + "M"}},
		Healthcheck: &resourcev1.Healthcheck{Tcp: &resourcev1.TcpCheck{Port: phpPort}},
	}}}
}

// phpConfig serves the folder's files, and passes what ends .php, or what isn't a
// file, as the front controller, to PHP-FPM.
func phpConfig(p *resourcev1.WebsitePhp) string {
	var b strings.Builder
	b.WriteString(globals)
	fmt.Fprintf(&b, "\n:%d {\n", Port)
	b.WriteString("\tlog {\n\t\toutput stderr\n\t\tformat console\n\t}\n")
	fmt.Fprintf(&b, "\troot * %s\n", PHPTarget)
	b.WriteString("\tencode zstd gzip\n")
	// In MiB, as PHP's M is, so both turn away the same request.
	fmt.Fprintf(&b, "\trequest_body {\n\t\tmax_size %dMiB\n\t}\n", uploadMB(p))
	b.WriteString(wellKnown)
	b.WriteString("\thandle {\n")
	// Nothing under a folder that starts with a dot runs or is sent, such as .git's.
	b.WriteString("\t\t@dotted path_regexp /\\.\n\t\trespond @dotted 404\n")
	// PHP's own index is the first that ends .php, and what the web server serves as a
	// folder's index never does, so a script's source is never sent as a file.
	script, files := "index.php", []string{}
	if i := slices.IndexFunc(phpIndex(p), func(n string) bool { return strings.HasSuffix(n, ".php") }); i >= 0 {
		script = phpIndex(p)[i]
	}
	for _, name := range phpIndex(p) {
		if !strings.HasSuffix(name, ".php") {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		files = []string{"index.html"}
	}
	// A folder's own index comes before the front controller, as nginx's try_files has
	// it, which php_fastcgi alone doesn't: it would send /docs/ to index.php.
	tries := []string{"{path}", "{path}/" + script}
	for _, name := range files {
		tries = append(tries, "{path}/"+name)
	}
	if p.GetFilesOnly() {
		tries = append(tries, "=404")
	} else {
		tries = append(tries, script)
	}
	fmt.Fprintf(&b, "\t\tphp_fastcgi %s:%d {\n", PHPService, phpPort)
	fmt.Fprintf(&b, "\t\t\tindex %s\n", script)
	fmt.Fprintf(&b, "\t\t\ttry_files %s\n", strings.Join(tries, " "))
	b.WriteString("\t\t}\n")
	// What PHP doesn't run is sent as it is, but scripts' other kinds and the copies
	// tools leave of them.
	fmt.Fprintf(&b, "\t\tfile_server {\n\t\t\tindex %s\n\t\t\thide .* %s\n\t\t}\n", strings.Join(files, " "), strings.Join(Hidden, " "))
	b.WriteString("\t}\n}\n")
	return b.String()
}
