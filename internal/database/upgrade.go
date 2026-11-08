// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/internal/docker"
	"github.com/dockwell/dockwell/internal/resource"
)

// Move is a database moving from one release to another, as a change to its
// service's image does.
type Move struct {
	Engine *Engine
	// From and To are the releases, such as "7.0" and "8.0".
	From, To string
	// RaisesFeatures says the database's feature compatibility version goes up to
	// From before it moves, as MongoDB moves only from data at its own release.
	RaisesFeatures bool
}

// MoveOf says what a change from one spec to another does to the database it runs:
// nothing, where it isn't one, keeps its release, or its tags name none; a move;
// or why it's refused. Only MongoDB has rules for it: it moves one major release
// at a time, each from a feature compatibility version equal to the release it's
// leaving, and back one at most (ADR 0038).
func MoveOf(name string, current, desired *resourcev1.Service) (*Move, error) {
	was, ok := FromSpec(current)
	if !ok {
		return nil, nil
	}
	now, ok := FromSpec(desired)
	if !ok || now.Engine != was.Engine || was.Engine.Kind != dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB {
		return nil, nil
	}
	from, to := current.GetApp().GetImage(), desired.GetApp().GetImage()
	a, b := Major(from), Major(to)
	if a == 0 || b == 0 || a == b {
		return nil, nil
	}
	m := &Move{Engine: was.Engine, From: Release(from), To: Release(to)}
	switch {
	case b == a+1:
		m.RaisesFeatures = true
		return m, nil
	case b == a-1:
		return m, nil
	case b > a:
		return nil, fmt.Errorf("%s runs MongoDB %s, and MongoDB moves one major release at a time, each from data at the release it leaves. "+
			"Change its image to mongo:%d.0 first, then to %s once that's healthy", name, m.From, a+1, to)
	}
	return nil, fmt.Errorf("%s runs MongoDB %s, which goes back one major release at most. Change its image to mongo:%d.0 first", name, m.From, a-1)
}

// Release is the release an image's tag names, as MongoDB's feature compatibility
// version says one: "7.0" for mongo:7, mongo:7.0 or mongo:7.0.12.
func Release(image string) string {
	parts := strings.SplitN(Version(image), ".", 3)
	if len(parts) < 2 {
		return strconv.Itoa(Major(image)) + ".0"
	}
	return parts[0] + "." + parts[1]
}

// raiseWait is how long raising MongoDB's feature compatibility version gets.
const raiseWait = 2 * time.Minute

// BeforeReplace raises a MongoDB's feature compatibility version to the release
// it runs, where its container is about to be replaced by the next release's, as
// MongoDB moves only from data at the release it leaves (ADR 0038). Setting it to
// what it is changes nothing. One that isn't running can't be asked: the next
// release says, as it starts, whether it reads the data.
func (r Reader) BeforeReplace(ctx context.Context, old *docker.Container, spec *resourcev1.Container) error {
	e := Recognise(old.Config.Image)
	if e == nil || e.Kind != dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB || Recognise(spec.GetImage()) != e || !old.State.Running {
		return nil
	}
	from, to := Major(old.Config.Image), Major(spec.GetImage())
	if from == 0 || to != from+1 {
		return nil
	}
	release := Release(old.Config.Image)
	resource.Progress(ctx, fmt.Sprintf("Raising MongoDB's feature compatibility version to %s, before it moves to %s", release, Release(spec.GetImage())))
	ctx, cancel := context.WithTimeout(ctx, raiseWait)
	defer cancel()
	cmd := append([]string{"env", "DW_RELEASE=" + release}, withPort("DW_PORT", PortOf(e, old.Config.Cmd), mongoRaise)...)
	errs := &capped{max: 4 << 10}
	code, err := r.Client.Exec(ctx, old.ID, cmd, docker.Streams{Stdin: strings.NewReader(mongoRaiseScript), Stderr: errs})
	said := strings.TrimSpace(errs.String())
	switch {
	case err != nil:
		return fmt.Errorf("can't raise MongoDB's feature compatibility version to %s before it moves on: %w", release, err)
	case refusedSignIn(said):
		return fmt.Errorf("MongoDB refused Dockwell's sign-in with what its environment holds, so its feature compatibility version can't go up to %s before it moves on. "+
			"Dockwell signs in as its image does, with %s", release, e.SignsInWith())
	case code != 0:
		return fmt.Errorf("MongoDB didn't raise its feature compatibility version to %s, with code %d: %s. Nothing else changed; check what it says, then apply this again",
			release, code, scrubbed(said))
	}
	return nil
}

// mongoRaise runs mongosh on mongoRaiseScript, as a reading does, with longer to do it.
var mongoRaise = []string{"sh", "-c", mongoSignIn +
	`d="$(mktemp -d)" || exit 1; HOME="$d" timeout 110 mongosh --quiet --norc --nodb --file /dev/stdin; c=$?; rm -rf "$d"; exit "$c"`}

// mongoRaiseScript sets the feature compatibility version to DW_RELEASE, with the
// confirmation MongoDB asks for from 7.0.
const mongoRaiseScript = `const env = process.env;
const admin = new Mongo("mongodb://127.0.0.1:" + (env.DW_PORT || "27017") + "/?directConnection=true&serverSelectionTimeoutMS=10000").getDB("admin");
if (env.MONGO_INITDB_ROOT_USERNAME) admin.auth(env.MONGO_INITDB_ROOT_USERNAME, env.MONGO_INITDB_ROOT_PASSWORD || "");
const cmd = { setFeatureCompatibilityVersion: env.DW_RELEASE };
if (Number(env.DW_RELEASE.split(".")[0]) >= 7) cmd.confirm = true;
const r = admin.runCommand(cmd);
if (!r.ok) throw new Error(r.errmsg || "it refused");
`
