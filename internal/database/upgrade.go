// SPDX-License-Identifier: AGPL-3.0-only

package database

import (
	"context"
	"encoding/json"
	"errors"
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
	// From and To are the releases its tags name, such as "7.0" and "8.0", or a
	// major alone, such as "8", where a tag names no more.
	From, To string
	// RaisesFeatures says the database's feature compatibility version goes up to
	// the release it runs before it moves, as MongoDB moves only from data at its
	// own release. Lowers says it moves back, which only works while its data uses
	// nothing newer than where it goes.
	RaisesFeatures, Lowers bool
	// Dumps says its data moves by a dump of the release it runs, loaded into the
	// new one on a new volume, as PostgreSQL's, MySQL's and MariaDB's do up a release
	// series (ADR 0080). Volume is the volume its data is on now, which stays as it
	// is, and Target where the new release has the new one.
	Dumps          bool
	Volume, Target string
}

// lastRapid is the newest rapid release of a major, which the next major moves from
// as well as from its .0, as MongoDB's upgrade path says: 9.0 from 8.0 or 8.3.
var lastRapid = map[int]int{8: 3}

// movesOn says the next major release moves from data at major.minor.
func movesOn(major, minor int) bool { return minor == 0 || minor == lastRapid[major] }

// startsFrom names what the next major after major moves from, for people: "8.0 or
// 8.3".
func startsFrom(major int) string {
	if r, ok := lastRapid[major]; ok {
		return fmt.Sprintf("%d.0 or %d.%d", major, major, r)
	}
	return fmt.Sprintf("%d.0", major)
}

// MoveOf says what a change from one spec to another does to the database it runs:
// nothing, where it isn't one, keeps its release, or its tags name none; a move;
// or why it's refused. PostgreSQL, MySQL and MariaDB move up a release series by a
// dump into a new volume (seriesMove). MongoDB moves one major release
// at a time, each from a feature compatibility version equal to the release it's
// leaving, which the next takes only from some releases, and back one at most
// (ADR 0038). A tag can name less than what runs, as mongo:8 runs 8.3: the agent
// asks the server before it replaces it, and refuses there what the tags hid.
func MoveOf(name string, current, desired *resourcev1.Service) (*Move, error) {
	was, ok := FromSpec(current)
	if !ok {
		return nil, nil
	}
	now, ok := FromSpec(desired)
	if ok && now.Engine == was.Engine && was.Engine.MovesBySeries() {
		return seriesMove(name, was, current, desired)
	}
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
		if minor, ok := Minor(from); ok && !movesOn(a, minor) {
			return nil, fmt.Errorf("%s runs MongoDB %s, and %d.0 moves only from data at %s. Change its image to mongo:%s first, then to %s once that's healthy",
				name, m.From, b, startsFrom(a), lastOf(a), to)
		}
		m.RaisesFeatures = true
		return m, nil
	case b == a-1:
		m.Lowers = true
		return m, nil
	case b > a:
		return nil, fmt.Errorf("%s runs MongoDB %s, and MongoDB moves one major release at a time, each from data at the release it leaves. "+
			"Change its image to mongo:%d.0 first, then to %s once that's healthy", name, m.From, a+1, to)
	}
	return nil, fmt.Errorf("%s runs MongoDB %s, which goes back one major release at most. Change its image to mongo:%d.0 first", name, m.From, a-1)
}

// lastOf is the newest release of a major the next moves from.
func lastOf(major int) string {
	if r, ok := lastRapid[major]; ok {
		return fmt.Sprintf("%d.%d", major, r)
	}
	return fmt.Sprintf("%d.0", major)
}

// Release is the release an image's tag names, as MongoDB's feature compatibility
// version says one: "7.0" for mongo:7.0 or mongo:7.0.12, and "7" for mongo:7, which
// runs whatever 7.x is newest.
func Release(image string) string {
	parts := strings.SplitN(Version(image), ".", 3)
	if len(parts) < 2 {
		return strconv.Itoa(Major(image))
	}
	return parts[0] + "." + parts[1]
}

// Minor is the minor release an image's tag names, if it names one.
func Minor(image string) (int, bool) {
	parts := strings.SplitN(Version(image), ".", 3)
	if len(parts) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(parts[1])
	return n, err == nil
}

// releaseOf reads "8.2.1" or "8.2" as its major and minor.
func releaseOf(version string) (major, minor int, ok bool) {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(parts[1])
	return major, minor, err1 == nil && err2 == nil
}

// raiseWait is how long raising MongoDB's feature compatibility version gets.
const raiseWait = 2 * time.Minute

// BeforeReplace asks a MongoDB about to be replaced by another release what it
// runs and what its data uses (ADR 0038). Moving up, it refuses a release the next
// doesn't move from, and raises the feature compatibility version to the release
// it runs; setting it to what it is changes nothing. Moving down, it refuses data
// that uses newer features than where it goes. One that isn't running can't be
// asked: the release it moves to says, as it starts, whether it reads the data.
//
// PostgreSQL, MySQL and MariaDB are held to the release series they run, as their
// images say it (sameSeries).
func (r Reader) BeforeReplace(ctx context.Context, old *docker.Container, spec *resourcev1.Container) error {
	e := Recognise(old.Config.Image)
	if e != nil && e.MovesBySeries() && Recognise(spec.GetImage()) == e {
		return r.sameSeries(ctx, e, old, spec)
	}
	if e == nil || e.Kind != dockwellv1.DatabaseEngine_DATABASE_ENGINE_MONGODB || Recognise(spec.GetImage()) != e || !old.State.Running {
		return nil
	}
	to := Major(spec.GetImage())
	if to == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, raiseWait)
	defer cancel()
	port := PortOf(e, old.Config.Cmd)
	said, err := r.mongoSays(ctx, old.ID, e, port)
	if err != nil {
		return err
	}
	major, minor, ok := releaseOf(said.Version)
	if !ok || to == major {
		return nil
	}
	runs := fmt.Sprintf("%d.%d", major, minor)
	if to < major {
		if Above(said.FCV, Release(spec.GetImage())) {
			return fmt.Errorf("its data uses MongoDB %s's features, which %s can't run, so it stays on %s. "+
				"Lower its feature compatibility version to where it goes first, with setFeatureCompatibilityVersion and confirm: true, then apply this again", said.FCV, Release(spec.GetImage()), runs)
		}
		return nil
	}
	if to != major+1 {
		return fmt.Errorf("it runs MongoDB %s, and MongoDB moves one major release at a time. Change its image to mongo:%d.0 first", runs, major+1)
	}
	if !movesOn(major, minor) {
		return fmt.Errorf("it runs MongoDB %s, and %d.0 moves only from data at %s. Change its image to mongo:%s first", runs, to, startsFrom(major), lastOf(major))
	}
	resource.Progress(ctx, fmt.Sprintf("Raising MongoDB's feature compatibility version to %s, before it moves to %s", runs, Release(spec.GetImage())))
	cmd := append([]string{"env", "DW_RELEASE=" + runs}, withPort("DW_PORT", port, mongoRaise)...)
	errs := &capped{max: 4 << 10}
	code, err := r.Client.Exec(ctx, old.ID, cmd, docker.Streams{Stdin: strings.NewReader(mongoRaiseScript), Stderr: errs})
	return mongoFailed(e, runs, code, err, strings.TrimSpace(errs.String()))
}

// Above says data at a feature compatibility version, such as "8.0", uses features
// newer than a release, such as "7.0", or "7" for its .0, runs.
func Above(fcv, release string) bool {
	fm, fn, ok := releaseOf(fcv)
	if !ok {
		return false
	}
	major, minor, _ := strings.Cut(release, ".")
	rm, err := strconv.Atoi(major)
	if err != nil {
		return false
	}
	rn, _ := strconv.Atoi(minor)
	return fm > rm || fm == rm && fn > rn
}

// mongoRelease is what a MongoDB says of its release and its data.
type mongoRelease struct {
	Version string `json:"version"`
	FCV     string `json:"fcv"`
}

// mongoSays asks a MongoDB the release it runs (buildInfo) and the features its
// data may use, as a reading signs in.
func (r Reader) mongoSays(ctx context.Context, id string, e *Engine, port uint32) (mongoRelease, error) {
	out := &capped{max: 64 << 10}
	errs := &capped{max: 4 << 10}
	code, err := r.Client.Exec(ctx, id, withPort("DW_PORT", port, mongoClient), docker.Streams{Stdin: strings.NewReader(mongoReleaseScript), Stdout: out, Stderr: errs})
	if err := mongoFailed(e, "", code, err, strings.TrimSpace(errs.String())); err != nil {
		return mongoRelease{}, err
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var s mongoRelease
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &s); err != nil || s.Version == "" {
		return mongoRelease{}, errors.New("can't read the release MongoDB runs from what it said, so nothing changed. Check it's healthy, then apply this again")
	}
	return s, nil
}

// mongoFailed says why asking MongoDB before a move failed, in words, or nothing.
func mongoFailed(e *Engine, raising string, code int, err error, said string) error {
	what := "say what it runs before it moves"
	if raising != "" {
		what = "raise its feature compatibility version to " + raising + " before it moves"
	}
	switch {
	case err != nil:
		return fmt.Errorf("MongoDB can't %s: %w", what, err)
	case refusedSignIn(said):
		return fmt.Errorf("MongoDB refused Dockwell's sign-in with what its environment holds, so it can't %s. Dockwell signs in as its image does, with %s", what, e.SignsInWith())
	case code != 0:
		return fmt.Errorf("MongoDB didn't %s, with code %d: %s. Nothing else changed; check what it says, then apply this again", what, code, scrubbed(said))
	}
	return nil
}

// mongoReleaseScript prints the release MongoDB runs and its feature compatibility
// version, as one line of JSON.
const mongoReleaseScript = `const env = process.env;
const admin = new Mongo("mongodb://127.0.0.1:" + (env.DW_PORT || "27017") + "/?directConnection=true&serverSelectionTimeoutMS=10000").getDB("admin");
if (env.MONGO_INITDB_ROOT_USERNAME) admin.auth(env.MONGO_INITDB_ROOT_USERNAME, env.MONGO_INITDB_ROOT_PASSWORD || "");
const f = admin.runCommand({ getParameter: 1, featureCompatibilityVersion: 1 });
print(JSON.stringify({ version: admin.runCommand({ buildInfo: 1 }).version, fcv: f.ok ? f.featureCompatibilityVersion.version : "" }));
`

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
