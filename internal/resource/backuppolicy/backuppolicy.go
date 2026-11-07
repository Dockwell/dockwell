// SPDX-License-Identifier: AGPL-3.0-only

// Package backuppolicy is the "backup.policy" kind: a schedule of backups of a
// volume, with how many to keep. It has revisions and plans like any kind, and runs
// on no server: the control plane keeps the schedule, as it keeps the record of
// backups. See docs/adr/0033-backups-of-volumes-and-databases.md.
package backuppolicy

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // a server's own zone files aren't always there

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	"github.com/dockwell/dockwell/internal/resource"
)

// Kind is the backup policy kind.
const Kind resource.Kind = "backup.policy"

// DefaultKeep is how many backups a policy keeps unless it says.
const DefaultKeep = 7

// MaxKeep bounds what a policy can keep, as each is a whole copy on the disk.
const MaxKeep = 1000

// Type is the kind.
type Type struct{}

var (
	_ resource.Type     = Type{}
	_ resource.Unplaced = Type{}
	_ resource.Referrer = Type{}
)

// Kind returns "backup.policy".
func (Type) Kind() resource.Kind { return Kind }

// Level is the environment, as the volume's.
func (Type) Level() resource.Level { return resource.LevelEnvironment }

// IDPrefix is "bpl".
func (Type) IDPrefix() string { return "bpl" }

// New returns an empty BackupPolicy.
func (Type) New() proto.Message { return &resourcev1.BackupPolicy{} }

// Unplaced says the control plane runs it.
func (Type) Unplaced() {}

// CheckName takes what fits in a URL, as a project's name does.
func (Type) CheckName(name string) error {
	return resource.CheckLabel("backup policy", name, "data-nightly")
}

var clock = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// Validate fills in its defaults, and refuses what can't run.
func (Type) Validate(spec proto.Message) error {
	p := spec.(*resourcev1.BackupPolicy)
	if p.GetVolume() == "" {
		return errors.New("a backup policy needs a volume, by name")
	}
	if p.GetEvery() == resourcev1.BackupPolicy_EVERY_UNSPECIFIED {
		p.Every = resourcev1.BackupPolicy_EVERY_DAY
	}
	if p.GetAt() == "" {
		p.At = "03:00"
	}
	if !clock.MatchString(p.GetAt()) {
		return fmt.Errorf("%q isn't a time of day. Use 24-hour HH:MM, such as 03:00", p.GetAt())
	}
	if p.GetWeekday() < 0 || p.GetWeekday() > 6 {
		return fmt.Errorf("weekday %d isn't a day. Use 0 for Sunday to 6 for Saturday", p.GetWeekday())
	}
	if p.GetEvery() != resourcev1.BackupPolicy_EVERY_WEEK {
		p.Weekday = 0
	}
	if p.GetTimeZone() == "" {
		p.TimeZone = "UTC"
	}
	if _, err := time.LoadLocation(p.GetTimeZone()); err != nil {
		return fmt.Errorf("%q isn't a time zone. Use a name such as Europe/Amsterdam", p.GetTimeZone())
	}
	if p.GetKeep() == 0 {
		p.Keep = DefaultKeep
	}
	if p.GetKeep() > MaxKeep {
		return fmt.Errorf("a policy keeps at most %d backups, and this one asks for %d, which are each a whole copy on the disk", MaxKeep, p.GetKeep())
	}
	return nil
}

// References names the volume, so it can't go while a policy backs it up.
func (Type) References(spec proto.Message) []resource.Reference {
	return []resource.Reference{{Kind: "volume", Name: spec.(*resourcev1.BackupPolicy).GetVolume(), Verb: "back up", Apart: true}}
}

// ReferenceKinds is volumes.
func (Type) ReferenceKinds() []resource.Kind { return []resource.Kind{"volume"} }

// Next is the first time a policy runs after a moment.
func Next(p *resourcev1.BackupPolicy, after time.Time) time.Time {
	zone, err := time.LoadLocation(p.GetTimeZone())
	if err != nil {
		zone = time.UTC
	}
	hour, _ := strconv.Atoi(p.GetAt()[:2])
	minute, _ := strconv.Atoi(p.GetAt()[3:])
	t := after.In(zone)
	switch p.GetEvery() {
	case resourcev1.BackupPolicy_EVERY_HOUR:
		// By the clock's minutes and real hours: a time of day means two moments as the
		// clocks go back, and none as they go forward.
		next := t.Truncate(time.Minute).Add(time.Duration(minute-t.Minute()) * time.Minute)
		if !next.After(after) {
			next = next.Add(time.Hour)
		}
		return next
	case resourcev1.BackupPolicy_EVERY_WEEK:
		days := (int(p.GetWeekday()) - int(t.Weekday()) + 7) % 7
		next := time.Date(t.Year(), t.Month(), t.Day()+days, hour, minute, 0, 0, zone)
		if !next.After(after) {
			next = time.Date(t.Year(), t.Month(), t.Day()+days+7, hour, minute, 0, 0, zone)
		}
		return next
	case resourcev1.BackupPolicy_EVERY_DAY, resourcev1.BackupPolicy_EVERY_UNSPECIFIED:
	}
	next := time.Date(t.Year(), t.Month(), t.Day(), hour, minute, 0, 0, zone)
	if !next.After(after) {
		next = time.Date(t.Year(), t.Month(), t.Day()+1, hour, minute, 0, 0, zone)
	}
	return next
}

// DestinationKind is the backup destination kind.
const DestinationKind resource.Kind = "backup.destination"

// Destination is the kind of S3-compatible storage backups go to. It lives in the
// organization, on the control plane alone, as what signs in to it goes to a server
// only for the call that needs it.
type Destination struct{}

var (
	_ resource.Type    = Destination{}
	_ resource.Rotates = Destination{}
	_ resource.Keeper  = Destination{}
)

// Kind returns "backup.destination".
func (Destination) Kind() resource.Kind { return DestinationKind }

// Level is the organization: any environment's backups can use one.
func (Destination) Level() resource.Level { return resource.LevelOrganization }

// IDPrefix is "bds".
func (Destination) IDPrefix() string { return "bds" }

// New returns an empty BackupDestination.
func (Destination) New() proto.Message { return &resourcev1.BackupDestination{} }

// RotatesSensitive says setting what signs in always plans as a change, and is never
// compared with what's there, so a plan can't confirm a guess at it (ADR 0022).
func (Destination) RotatesSensitive() {}

// Keep holds on to a destination's passphrase, as every copy there was sealed with it,
// and a new one would leave them all unopenable. An edit that leaves it out keeps it. One
// that gives one is refused, whatever it gives, so a refusal never says whether a guess
// at it is right (ADR 0022).
func (Destination) Keep(current, desired proto.Message) (resource.Kept, error) {
	was, ok := current.(*resourcev1.BackupDestination)
	is, same := desired.(*resourcev1.BackupDestination)
	if !ok || !same {
		return resource.Kept{}, nil
	}
	if is.GetPassphrase() != "" {
		return resource.Kept{}, errors.New("a destination keeps the passphrase it was made with, as every copy there is sealed with it, so an update can't set one. Leave it out to keep it. To change it, remove the destination and add it again: copies already there then open only with the old passphrase, through dockwell open-backup or by adding it back with that one")
	}
	is.Passphrase = was.GetPassphrase()
	return resource.Kept{Reason: "kept_passphrase", Words: "keeps the passphrase it was made with, as every copy there is sealed with it"}, nil
}

// CheckNew asks for a passphrase. An update can leave it out, which Keep fills in, so
// Validate can't.
func (Destination) CheckNew(spec proto.Message) error {
	d := spec.(*resourcev1.BackupDestination) //nolint:forcetypeassert // Validate saw it
	if len(d.GetPassphrase()) < minPassphrase {
		return fmt.Errorf("a destination's passphrase needs at least %d characters, as it's all that keeps a backup from whoever holds the bucket. Let the panel make one, or use a long one of your own", minPassphrase)
	}
	return nil
}

// CheckName takes what fits in a URL.
func (Destination) CheckName(name string) error {
	return resource.CheckLabel("backup destination", name, "r2")
}

// Validate refuses what can't work, in words that hold nothing of what signs in.
func (Destination) Validate(spec proto.Message) error {
	d := spec.(*resourcev1.BackupDestination)
	u, err := url.Parse(d.GetEndpoint())
	switch {
	case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
		return errors.New("a destination's endpoint is the storage's address, such as https://<account>.r2.cloudflarestorage.com")
	case u.Scheme == "http" && !loopback(u.Hostname()):
		return errors.New("a destination's endpoint has to use HTTPS, as what signs in goes with each request. Only an address on this server may use plain HTTP")
	case d.GetBucket() == "":
		return errors.New("a destination needs a bucket")
	case d.GetAccessKey() == "" || d.GetSecretKey() == "":
		return errors.New("a destination needs an access key and its secret")
	}
	d.Prefix = strings.Trim(d.GetPrefix(), "/")
	if d.GetRegion() == "" {
		d.Region = "auto"
	}
	return nil
}

// minPassphrase is the shortest passphrase a destination takes.
const minPassphrase = 16

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
