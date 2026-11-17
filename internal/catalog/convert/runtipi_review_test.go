// SPDX-License-Identifier: AGPL-3.0-only

package convert

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runtipiOne writes one Runtipi app, with the files given beside its Compose file,
// and converts it as a server does.
func runtipiOne(t *testing.T, id, compose string, files map[string]string) Result {
	t.Helper()
	dir := t.TempDir()
	at := filepath.Join(dir, "apps", id)
	files["config.json"] = `{ "name": "` + id + `", "available": true, "port": 8214, "id": "` + id + `", "version": "1.0.0", "categories": ["media"], "short_desc": "x", "description": "x", "source": "https://example.com", "form_fields": [], "supported_architectures": ["amd64", "arm64"] }`
	files["docker-compose.yml"] = compose
	for rel, body := range files {
		p := filepath.Join(at, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	store := storeOf(t, FormatRuntipi)
	src := Source{Name: store.Name, Repository: "https://example.com/appstore", Commit: strings.Repeat("a", 40), License: store.License}
	l := listingOf(t, store, dir, nil)
	// The shipped files a container mounts are read in a round of their own, by their
	// paths in the store, as a server reads them.
	var asked []string
	l.ReadMore = func(_ context.Context, paths []string) (map[string][]byte, error) {
		out := map[string][]byte{}
		for _, p := range paths {
			asked = append(asked, p)
			b, err := os.ReadFile(filepath.Join(dir, p))
			if err != nil {
				return nil, err
			}
			out[p] = b
		}
		return out, nil
	}
	run, err := OnServer(t.Context(), store, l, src, committed, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range asked {
		if !strings.HasPrefix(p, "apps/"+id+"/data/") {
			t.Errorf("the round asked for %s, outside the app's data folder", p)
		}
	}
	return resultsByID(run)[id]
}

// A file Runtipi's store ships in the app's data/ folder, which Runtipi copies into
// ${APP_DATA_DIR}/data as it installs, is a file Dockwell writes where the app binds
// it, as the other stores' are, never an empty volume over it (#570's Runtipi
// review, H1).
func TestRuntipisShippedFileIsAFileDockwellWrites(t *testing.T) {
	r := runtipiOne(t, "photos", `services:
  photos:
    image: example/proxy:1.0
    volumes:
      - ${APP_DATA_DIR}/data/proxy/nginx.conf:/etc/nginx/nginx.conf:ro
      - ${APP_DATA_DIR}/data/cache:/cache
    ports:
      - ${APP_PORT}:80
`, map[string]string{"data/proxy/nginx.conf": "events {}\nhttp { server { listen 80; } }\n", "data/cache/.gitkeep": ""})
	if r.Entry == nil {
		t.Fatalf("left out: %s", r.LeftOut)
	}
	c := r.Entry.GetCompose()
	if !strings.Contains(c, "events {}") || !strings.Contains(c, "target: /etc/nginx/nginx.conf") || strings.Contains(c, "nginx.conf:/etc/nginx") {
		t.Errorf("the file its store ships isn't the file it mounts:\n%s", c)
	}
	// A folder that holds only a placeholder is one the app fills: a volume.
	if !strings.Contains(c, ":/cache") {
		t.Errorf("its cache isn't a volume:\n%s", c)
	}
}

// What Runtipi does with its files beyond a copy leaves the app out, saying so: a
// folder of them the app writes to, a binding of the whole folder they land in, and
// a .template Runtipi fills in with the app's values.
func TestRuntipisShippedFilesItCantCarry(t *testing.T) {
	for name, c := range map[string]struct {
		compose string
		files   map[string]string
		words   string
	}{
		"a folder the app writes to": {
			"services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - ${APP_DATA_DIR}/data/config:/config\n",
			map[string]string{"data/config/settings.yml": "theme: dark\n"},
			"it ships files into data/config, a folder the app writes to",
		},
		"the whole folder": {
			"services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - ${APP_DATA_DIR}:/app\n",
			map[string]string{"data/settings.yml": "theme: dark\n"},
			"it binds ${APP_DATA_DIR}, the folder Runtipi copies the files its store ships into as it installs",
		},
		"a template": {
			"services:\n  a:\n    image: example/a:1.0\n    volumes:\n      - ${APP_DATA_DIR}/data/users.yml:/users.yml:ro\n",
			map[string]string{"data/users.yml.template": "user: {{ADMIN}}\n"},
			"it ships data/users.yml.template, which Runtipi fills in with the app's values as it installs",
		},
	} {
		r := runtipiOne(t, "a", c.compose, c.files)
		if r.Entry != nil || r.Class != ClassShipped || !strings.Contains(r.LeftOut, c.words) {
			t.Errorf("%s: class %q, left out %q, want %q", name, r.Class, r.LeftOut, c.words)
		}
	}
}

// TubeArchivist runs as user 1000 on a volume Docker makes root's, so it's left out,
// for its volume's owner (#570's Runtipi review, H2, and recheck, N4).
func TestTubeArchivistIsLeftOut(t *testing.T) {
	r := runtipiOne(t, "tubearchivist", `services:
  tubearchivist:
    image: example/archive:1.0
    user: "1000:1000"
    volumes:
      - ${APP_DATA_DIR}/data/cache:/cache
    ports:
      - ${APP_PORT}:8000
`, map[string]string{})
	if r.Entry != nil || r.Class != ClassCantSay || !strings.Contains(r.LeftOut, "its volume at /cache would be root's") {
		t.Errorf("tubearchivist: class %q, left out %q", r.Class, r.LeftOut)
	}
}

// The server's address at a port other than the app's is a container reaching the
// server, not a browser opening the app, so it leaves the app out (#570's Runtipi
// review, M2).
func TestRuntipisServerAddressAtAnotherPortLeavesItOut(t *testing.T) {
	out, err := runtipiCompose([]byte("services:\n  a:\n    image: example/a:1.0\n    environment:\n      - OLLAMA_URL=http://${INTERNAL_IP}:11434\n"), "a")
	if err == nil || !strings.Contains(err.Error(), "it names the server's address, OLLAMA_URL=http://${INTERNAL_IP}:11434") {
		t.Errorf("a container's address of the server: %v\n%s", err, out)
	}
}

// A list of hosts that names only Runtipi's own leaves the app out, rather than
// written empty (#570's Runtipi review, L1).
func TestRuntipisHostListOfItsOwnLeavesItOut(t *testing.T) {
	out, err := runtipiCompose([]byte("services:\n  a:\n    image: example/a:1.0\n    environment:\n      - ALLOWED_HOSTS=a.${LOCAL_DOMAIN},${APP_LOCAL_DOMAIN}\n"), "a")
	if err == nil || !strings.Contains(err.Error(), "it accepts only hosts Runtipi's own proxy answers") {
		t.Errorf("a list of Runtipi's own hosts: %v\n%s", err, out)
	}
}

// A server's fetch lists what Runtipi ships in an app's data/ folder, so what it
// binds is read, and nothing else deeper in the app's folder.
func TestRuntipisFetchListsItsDataFolder(t *testing.T) {
	store := storeOf(t, FormatRuntipi)
	for p, want := range map[string]bool{
		"apps":                       true,
		"apps/notes":                 true,
		"apps/notes/metadata":        true,
		"apps/notes/data":            true,
		"apps/notes/data/proxy":      true,
		"apps/notes/data/a/b/c":      true,
		"apps/notes/data/a/b/c/d/e":  false,
		"apps/notes/other/deeper":    false,
		"apps/notes/metadata/deeper": false,
	} {
		if got := store.Descend(p, strings.Count(p, "/")+1); got != want {
			t.Errorf("Descend(%s) = %v, want %v", p, got, want)
		}
	}
}

// An app whose image writes its own starting files into an empty folder keeps the
// empty volume it had, where its store's files would otherwise leave it out: a
// folder of them it writes to, or the whole folder they land in (#570's Runtipi
// recheck, N1).
func TestARuntipiAppThatFillsItsOwnFolderKeepsItsVolume(t *testing.T) {
	for id, compose := range map[string]string{
		"homeassistant-1": "services:\n  homeassistant-1:\n    image: example/ha:1.0\n    volumes:\n      - ${APP_DATA_DIR}/data/config:/config\n    ports:\n      - ${APP_PORT}:8123\n",
		"searxng":         "services:\n  searxng:\n    image: example/searxng:1.0\n    volumes:\n      - ${APP_DATA_DIR}/data:/etc/searxng\n    ports:\n      - ${APP_PORT}:8080\n",
	} {
		r := runtipiOne(t, id, compose, map[string]string{"data/config/configuration.yaml": "default_config:\n", "data/settings.yml": "use_default_settings: true\n"})
		if r.Entry == nil {
			t.Errorf("%s was left out: %s", id, r.LeftOut)
			continue
		}
		if c := r.Entry.GetCompose(); strings.Contains(c, "content:") || !strings.Contains(c, "volumes:") {
			t.Errorf("%s doesn't keep its empty volume:\n%s", id, c)
		}
	}
}
