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
	"github.com/dockwell/dockwell/internal/docker"
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
	// DefaultPHPMemoryLimitMB is PHP's default memory_limit when unset.
	DefaultPHPMemoryLimitMB = 128
	// MinPHPMemoryLimitMB is the lowest allowed PHP memory_limit in MiB.
	MinPHPMemoryLimitMB = 32
	// MaxPHPMemoryLimitMB is the highest allowed PHP memory_limit in MiB.
	MaxPHPMemoryLimitMB = 4096
	// DefaultPHPMaxExecutionTimeS is PHP's default max_execution_time when unset.
	DefaultPHPMaxExecutionTimeS = 30
	// MinPHPMaxExecutionTimeS is the lowest allowed PHP max_execution_time in seconds.
	MinPHPMaxExecutionTimeS = 5
	// MaxPHPMaxExecutionTimeS is the highest allowed PHP max_execution_time in seconds.
	MaxPHPMaxExecutionTimeS = 3600
	// MinPHPPostMaxSizeMB is the lowest allowed PHP post_max_size in MiB.
	MinPHPPostMaxSizeMB = 1
	// MaxPHPPostMaxSizeMB is the highest allowed PHP post_max_size in MiB.
	MaxPHPPostMaxSizeMB = 4096
	// DefaultPHPFPMMaxChildren is the default PHP-FPM pm.max_children when unset.
	DefaultPHPFPMMaxChildren = 5
	// MinPHPFPMMaxChildren is the lowest allowed PHP-FPM pm.max_children.
	MinPHPFPMMaxChildren = 1
	// MaxPHPFPMMaxChildren is the highest allowed PHP-FPM pm.max_children.
	MaxPHPFPMMaxChildren = 200
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
	{"8.5", "8.5.12", docker.WebsitePHP85Image},
	{"8.4", "8.4.27", docker.WebsitePHP84Image},
	{"8.3", "8.3.36", docker.WebsitePHP83Image},
	{"8.2", "8.2.35", docker.WebsitePHP82Image},
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
	if m := p.GetMemoryLimitMb(); m != 0 && (m < MinPHPMemoryLimitMB || m > MaxPHPMemoryLimitMB) {
		return fmt.Errorf("PHP memory_limit must be between %d MiB and %d MiB", MinPHPMemoryLimitMB, MaxPHPMemoryLimitMB)
	}
	if s := p.GetMaxExecutionTimeS(); s != 0 && (s < MinPHPMaxExecutionTimeS || s > MaxPHPMaxExecutionTimeS) {
		return fmt.Errorf("PHP max_execution_time must be between %d s and %d s", MinPHPMaxExecutionTimeS, MaxPHPMaxExecutionTimeS)
	}
	if pm := p.GetPostMaxSizeMb(); pm != 0 && (pm < MinPHPPostMaxSizeMB || pm > MaxPHPPostMaxSizeMB) {
		return fmt.Errorf("PHP post_max_size must be between %d MiB and %d MiB", MinPHPPostMaxSizeMB, MaxPHPPostMaxSizeMB)
	}
	if mc := p.GetFpmMaxChildren(); mc != 0 && (mc < MinPHPFPMMaxChildren || mc > MaxPHPFPMMaxChildren) {
		return fmt.Errorf("PHP-FPM pool size (pm.max_children) must be between %d and %d", MinPHPFPMMaxChildren, MaxPHPFPMMaxChildren)
	}
	if len(p.GetDisabledExtensions()) > 0 {
		seen := map[string]bool{}
		var disabled []string
		for _, raw := range p.GetDisabledExtensions() {
			ext := strings.ToLower(strings.TrimSpace(raw))
			if ext == "" || seen[ext] {
				continue
			}
			if !slices.Contains(PHPExtensions, ext) {
				return fmt.Errorf("PHP extension %q isn't one of the image's optional extensions (%s)", raw, strings.Join(PHPExtensions, ", "))
			}
			seen[ext] = true
			disabled = append(disabled, ext)
		}
		slices.Sort(disabled)
		p.DisabledExtensions = disabled
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

// postMaxMB is PHP's post_max_size in MiB, defaulting to uploadMB.
func postMaxMB(p *resourcev1.WebsitePhp) uint32 {
	return cmp.Or(p.GetPostMaxSizeMb(), uploadMB(p))
}

// requestBodyMaxMB is the larger of upload_max_filesize and post_max_size so Caddy
// never rejects a request PHP would accept.
func requestBodyMaxMB(p *resourcev1.WebsitePhp) uint32 {
	return max(uploadMB(p), postMaxMB(p))
}

// phpBootScript prepares optional FPM pool overrides and extension filters in /tmp
// before handing off to docker-php-entrypoint.
const phpBootScript = `set -eu
fpm_conf="$1"
disabled="$2"
shift 2
if [ -n "$fpm_conf" ]; then
	printf '%s\n' "$fpm_conf" >/tmp/php-fpm.conf
	set -- "$@" -y /tmp/php-fpm.conf
fi
if [ -n "$disabled" ]; then
	rm -rf /tmp/php-conf.d && mkdir -p /tmp/php-conf.d
	for f in /usr/local/etc/php/conf.d/*.ini; do
		[ -f "$f" ] || continue
		base=$(basename "$f")
		skip=0
		for ext in $disabled; do
			case "$base" in
				"docker-php-ext-${ext}.ini") skip=1 ;;
				"opcache-recommended.ini") [ "$ext" = "opcache" ] && skip=1 ;;
			esac
		done
		[ "$skip" = 0 ] && cp "$f" "/tmp/php-conf.d/$base"
	done
	export PHP_INI_SCAN_DIR=/tmp/php-conf.d
fi
exec docker-php-entrypoint "$@"`

func phpFPMOverride(maxChildren uint32) string {
	if maxChildren == 0 {
		return ""
	}
	n := int(maxChildren)
	minSpare := max(1, min(n, n/4))
	maxSpare := max(minSpare, min(n, (n*3+3)/4))
	start := max(minSpare, min(maxSpare, (minSpare+maxSpare)/2))
	return fmt.Sprintf("include=/usr/local/etc/php-fpm.conf\n[www]\npm = dynamic\npm.max_children = %d\npm.start_servers = %d\npm.min_spare_servers = %d\npm.max_spare_servers = %d",
		n, start, minSpare, maxSpare)
}

// phpServing is what serves a PHP website: its web server, which serves its files
// and passes PHP to PHP-FPM beside it, on a network of their own.
func phpServing(w *resourcev1.Website, out *Serving) {
	p := w.GetPhp()
	out.Route.Service, out.Route.Port = WebService, Port
	out.Networks[SiteNetwork] = &resourcev1.Network{}
	web := webService(phpConfig(w))
	app := web.GetApp()
	user := cmp.Or(p.GetUser(), DefaultPHPUser)
	app.Mounts = []*resourcev1.Mount{{
		Source: &resourcev1.Mount_HostPath{HostPath: p.GetRoot()}, Target: PHPTarget, ReadOnly: true,
		// A folder that isn't there is made as PHP's, which writes in it.
		MakeOwner: user,
	}}
	out.Services[WebService] = web

	rt, _ := PHPRuntime(p.GetVersion())
	upLimit := strconv.FormatUint(uint64(uploadMB(p)), 10) + "M"
	postLimit := strconv.FormatUint(uint64(postMaxMB(p)), 10) + "M"
	phpArgs := []string{
		"php-fpm", "-d", "upload_max_filesize=" + upLimit, "-d", "post_max_size=" + postLimit,
		// No .user.ini: what a site's folder holds doesn't change how PHP runs it.
		"-d", "user_ini.filename=",
	}
	if m := p.GetMemoryLimitMb(); m != 0 {
		phpArgs = append(phpArgs, "-d", "memory_limit="+strconv.FormatUint(uint64(m), 10)+"M")
	}
	if s := p.GetMaxExecutionTimeS(); s != 0 {
		phpArgs = append(phpArgs, "-d", "max_execution_time="+strconv.FormatUint(uint64(s), 10))
	}
	if slices.Contains(p.GetDisabledExtensions(), "opcache") {
		phpArgs = append(phpArgs, "-d", "opcache.enable=0")
	}
	entrypoint := []string{"docker-php-entrypoint"}
	command := phpArgs
	if p.GetFpmMaxChildren() != 0 || len(p.GetDisabledExtensions()) > 0 {
		entrypoint = []string{"/bin/sh", "-c", phpBootScript, "php"}
		command = append([]string{phpFPMOverride(p.GetFpmMaxChildren()), strings.Join(p.GetDisabledExtensions(), " ")}, phpArgs...)
	}

	out.Services[PHPService] = &resourcev1.Service{Runs: &resourcev1.Service_App{App: &resourcev1.Container{
		Image:      rt.Image,
		Entrypoint: entrypoint,
		Command:    command,
		// FPM's master too, not root: it takes no capability, and binds 9000.
		User:     user,
		CapDrop:  []string{"ALL"},
		Networks: []string{SiteNetwork},
		// Read-write, as a site's code writes uploads and caches beside itself.
		Mounts: []*resourcev1.Mount{{
			Source: &resourcev1.Mount_HostPath{HostPath: p.GetRoot()}, Target: PHPTarget, MakeOwner: user,
		}},
		// Where PHP keeps an upload until the script takes it, in memory, as big as the
		// upload/post limit and a little more.
		Tmpfs:       []*resourcev1.Tmpfs{{Target: "/tmp", Size: strconv.FormatUint(uint64(requestBodyMaxMB(p))+16, 10) + "M"}},
		Healthcheck: &resourcev1.Healthcheck{Tcp: &resourcev1.TcpCheck{Port: phpPort}},
	}}}
}

// phpConfig serves the folder's files, and passes what ends .php, or what isn't a
// file, as the front controller, to PHP-FPM.
func phpConfig(w *resourcev1.Website) string {
	p := w.GetPhp()
	var b strings.Builder
	b.WriteString(globals)
	fmt.Fprintf(&b, "\n:%d {\n", Port)
	b.WriteString("\tlog {\n\t\toutput stderr\n\t\tformat console\n\t}\n")
	fmt.Fprintf(&b, "\troot * %s\n", PHPTarget)
	b.WriteString("\tencode zstd gzip\n")
	// In MiB, as PHP's M is, so both turn away the same request.
	fmt.Fprintf(&b, "\trequest_body {\n\t\tmax_size %dMiB\n\t}\n", requestBodyMaxMB(p))
	writeRules(&b, w.GetRules())
	b.WriteString(wellKnown)
	b.WriteString("\thandle {\n")
	custom := strings.TrimSpace(w.GetCustomCaddy()) != ""
	indent := "\t\t"
	if custom {
		b.WriteString("\t\troute {\n")
		writeCustomCaddy(&b, w.GetCustomCaddy(), "\t\t\t")
		indent = "\t\t\t"
	}
	// Nothing under a folder that starts with a dot runs or is sent, such as .git's.
	fmt.Fprintf(&b, "%s@dotted path_regexp /\\.\n%srespond @dotted 404\n", indent, indent)
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
	fmt.Fprintf(&b, "%sphp_fastcgi %s:%d {\n", indent, PHPService, phpPort)
	fmt.Fprintf(&b, "%s\tindex %s\n", indent, script)
	fmt.Fprintf(&b, "%s\ttry_files %s\n", indent, strings.Join(tries, " "))
	fmt.Fprintf(&b, "%s}\n", indent)
	// What PHP doesn't run is sent as it is, but scripts' other kinds and the copies
	// tools leave of them.
	fmt.Fprintf(&b, "%sfile_server {\n%s\tindex %s\n%s\thide .* %s\n%s}\n", indent, indent, strings.Join(files, " "), indent, strings.Join(Hidden, " "), indent)
	if custom {
		b.WriteString("\t\t}\n")
	}
	b.WriteString("\t}\n}\n")
	return b.String()
}
