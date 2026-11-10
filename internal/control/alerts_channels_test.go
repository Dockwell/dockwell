// SPDX-License-Identifier: AGPL-3.0-only

package control

import (
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
	dockwellv1 "github.com/dockwell/dockwell/gen/dockwell/v1"
	"github.com/dockwell/dockwell/gen/dockwell/v1/dockwellv1connect"
	"github.com/dockwell/dockwell/internal/control/alerts"
	"github.com/dockwell/dockwell/internal/resource/alert"
	"github.com/dockwell/dockwell/internal/store"
	"github.com/dockwell/dockwell/internal/store/storetest"
)

// A test of a channel says how the channel is only when it tested the channel as
// it's kept, not a form someone hasn't planned; each person or token sends six a
// minute; and every test is on Activity, with who sent it, to what type and host,
// and whether it arrived.
func TestTestingAChannel(t *testing.T) { storetest.Each(t, testTestingAChannel) }

func testTestingAChannel(t *testing.T, backend store.Backend) {
	f := startFull(t, backend, nil)
	b := signedIn(t, f)
	c := changeClient(b, b.origin)
	person := dockwellv1connect.NewAlertServiceClient(b, b.origin, connect.WithProtoJSON())
	good, gone := newHook(t), newHook(t)
	gone.code = http.StatusNotFound
	made, err := c.ProposeChange(t.Context(), connect.NewRequest(&dockwellv1.ProposeChangeRequest{Summary: "Add the ops webhook", Edits: []*dockwellv1.Edit{{
		Resource: &dockwellv1.ResourceRef{Kind: string(alert.ChannelKind), Name: "ops"},
		Spec: &resourcev1.Spec{Kind: &resourcev1.Spec_AlertChannel{AlertChannel: &resourcev1.AlertChannel{
			To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: gone.srv.URL + "/hook", Secret: "s3cret"}},
		}}},
	}}}))
	if err != nil {
		t.Fatal(err)
	}
	apply(t, c, made.Msg.GetChange())
	lastSaid := func() string {
		t.Helper()
		res, err := person.ListAlertChannels(t.Context(), connect.NewRequest(&dockwellv1.ListAlertChannelsRequest{}))
		if err != nil || len(res.Msg.GetChannels()) != 1 {
			t.Fatalf("ListAlertChannels = %v, %v", res, err)
		}
		return res.Msg.GetChannels()[0].GetLast().GetSaid()
	}

	// A form that isn't the channel as kept, say with a URL that works, says nothing
	// of the channel, which still fails.
	tested, err := person.TestAlertChannel(t.Context(), connect.NewRequest(&dockwellv1.TestAlertChannelRequest{
		Name:    "ops",
		Channel: &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: good.srv.URL + "/hook", Secret: "an0ther"}}},
	}))
	if err != nil || !tested.Msg.GetDelivery().GetOk() {
		t.Fatalf("testing a form = %v, %v", tested, err)
	}
	if said := lastSaid(); said != "" {
		t.Errorf("a form's test is recorded on the channel as it's kept: %q", said)
	}
	// The channel as kept, or a form that Keep makes it, is.
	if _, err := person.TestAlertChannel(t.Context(), connect.NewRequest(&dockwellv1.TestAlertChannelRequest{Name: "ops"})); err != nil {
		t.Fatal(err)
	}
	if said := lastSaid(); said != "404 Not Found" {
		t.Errorf("the channel's test is recorded as %q", said)
	}
	if _, err := person.TestAlertChannel(t.Context(), connect.NewRequest(&dockwellv1.TestAlertChannelRequest{
		Name:    "ops",
		Channel: &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: gone.srv.URL + "/hook"}}},
	})); err != nil {
		t.Fatal(err)
	}

	// Six a minute for a token; a person is counted apart.
	token := createToken(t, b, dockwellv1.TokenScope_TOKEN_SCOPE_READ_WRITE)
	script := dockwellv1connect.NewAlertServiceClient(bearer{token: token, rt: b.client.Transport}, b.origin, connect.WithProtoJSON())
	form := &resourcev1.AlertChannel{To: &resourcev1.AlertChannel_Webhook{Webhook: &resourcev1.AlertWebhook{Url: good.srv.URL + "/hook"}}}
	for i := range alerts.TestsPerMinute {
		if _, err := script.TestAlertChannel(t.Context(), connect.NewRequest(&dockwellv1.TestAlertChannelRequest{Channel: form})); err != nil {
			t.Fatalf("test %d: %v", i+1, err)
		}
	}
	_, err = script.TestAlertChannel(t.Context(), connect.NewRequest(&dockwellv1.TestAlertChannelRequest{Channel: form}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted || reasonOf(err) != "too_many_requests" || !strings.Contains(err.Error(), "you've sent 6 tests in the last minute") {
		t.Errorf("a seventh test in a minute = %v", err)
	}

	// Each on Activity: who, the type, the host, and whether it arrived.
	events, err := dockwellv1connect.NewEventServiceClient(b, b.origin, connect.WithProtoJSON()).ListEvents(t.Context(), connect.NewRequest(&dockwellv1.ListEventsRequest{
		Kinds: []dockwellv1.EventKind{dockwellv1.EventKind_EVENT_KIND_ALERT}, PageSize: 50,
	}))
	if err != nil {
		t.Fatal(err)
	}
	host := strings.TrimPrefix(good.srv.URL, "http://")
	var byToken, byPerson, arrived int
	for _, ev := range events.Msg.GetEvents() {
		if ev.GetAction() != alerts.ActionTest {
			continue
		}
		a := ev.GetAlert()
		if a.GetVariant() != "webhook" || (a.GetWhere() != host && a.GetWhere() != strings.TrimPrefix(gone.srv.URL, "http://")) {
			t.Errorf("a test's event says %v", a)
		}
		if a.GetValue() == 1 {
			arrived++
		}
		switch ev.GetActor().GetKind() {
		case dockwellv1.ActorKind_ACTOR_KIND_TOKEN:
			byToken++
		case dockwellv1.ActorKind_ACTOR_KIND_USER:
			byPerson++
			if a.GetSubjectName() != "ops" {
				t.Errorf("a person's test of ops names %q", a.GetSubjectName())
			}
		default:
			t.Errorf("a test was sent by %v", ev.GetActor())
		}
	}
	if byToken != alerts.TestsPerMinute || byPerson != 3 || arrived != alerts.TestsPerMinute+1 {
		t.Errorf("Activity has %d tests by the token and %d by a person, %d of them arrived", byToken, byPerson, arrived)
	}
}

// A far end that asks Dockwell to slow down is waited for as long as it asks,
// which isn't a failure, and a channel gets one message at a time.
func TestAChannelThatAsksToSlowDownIsWaitedFor(t *testing.T) {
	var calls, inFlight, most atomic.Int32
	var mu sync.Mutex
	limited := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		l := limited
		mu.Unlock()
		if l {
			w.Header().Set("Retry-After", "900")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	f, _, b := evalStand(t, srv.URL+"/hook")
	sites := dockwellv1connect.NewAlertServiceClient(b, b.origin, connect.WithProtoJSON())
	at := &clock{now: time.Now()}
	e := evaluatorOn(f, at)
	// Memory and a disk fill: the disk's rule fires first, and the channel asks
	// Dockwell to wait 15 minutes; the memory's fires within them.
	e.Fleet = evalFleet{func() []*dockwellv1.Server {
		s := connectedAt(at.Now(), 99)
		s[0].Stats.Disks[0].UsedBytes = 99
		return s
	}}
	waited := time.Duration(0)
	for range 60 {
		tickOn(t, e)
		at.add(alerts.Every)
		if calls.Load() > 0 {
			break
		}
	}
	waited += alerts.Every
	if calls.Load() != 1 {
		t.Fatalf("the channel heard %d requests at first", calls.Load())
	}
	// Nothing goes within the 15 minutes it asked for, and no failure is recorded.
	for waited+alerts.Every < 15*time.Minute {
		tickOn(t, e)
		at.add(alerts.Every)
		waited += alerts.Every
	}
	if calls.Load() != 1 {
		t.Errorf("the channel heard %d requests within the 15 minutes it asked for", calls.Load())
	}
	deliveries, err := sites.ListAlertDeliveries(t.Context(), connect.NewRequest(&dockwellv1.ListAlertDeliveriesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(deliveries.Msg.GetDeliveries(), func(d *dockwellv1.AlertDelivery) bool { return !d.GetOk() }) {
		t.Errorf("being asked to slow down is recorded as a failure: %v", deliveries.Msg.GetDeliveries())
	}
	// Then both are due at once, and go one at a time.
	mu.Lock()
	limited = false
	mu.Unlock()
	for range 4 {
		at.add(alerts.Every)
		tickOn(t, e)
	}
	if calls.Load() != 3 || most.Load() != 1 {
		t.Errorf("after the wait the channel heard %d requests, at most %d at once; want both rules' messages, one at a time", calls.Load(), most.Load())
	}
}
