// SPDX-License-Identifier: AGPL-3.0-only

package alert

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	resourcev1 "github.com/dockwell/dockwell/gen/dockwell/resource/v1alpha1"
)

func TestACheckFillsInItsDefaultsSoTheSameCheckComparesEqual(t *testing.T) {
	c := &resourcev1.AlertCheck{Address: "https://shop.example.com/"}
	if err := (Check{}).Validate(c); err != nil {
		t.Fatal(err)
	}
	want := &resourcev1.AlertCheck{Address: "https://shop.example.com/", Every: "1m", Timeout: "10s", DownAfter: 2, ExpectFrom: 200, ExpectTo: 399}
	if !proto.Equal(c, want) {
		t.Errorf("a check with its defaults reads %v, want %v", c, want)
	}
	again := proto.CloneOf(want)
	if err := (Check{}).Validate(again); err != nil || !proto.Equal(again, want) {
		t.Errorf("a check with its defaults filled in read again as %v: %v", again, err)
	}
}

func TestACheckThatCantRunIsRefused(t *testing.T) {
	for _, tc := range []struct {
		check *resourcev1.AlertCheck
		says  string
	}{
		{&resourcev1.AlertCheck{}, "needs the address"},
		{&resourcev1.AlertCheck{Address: "ftp://files.example.com/"}, "http or https URL"},
		{&resourcev1.AlertCheck{Address: "shop.example.com"}, "http or https URL"},
		{&resourcev1.AlertCheck{Address: "https://admin:hunter2@shop.example.com/"}, "can't hold a user and password"},
		{&resourcev1.AlertCheck{Address: "https://shop.example.com/", Every: "2m"}, "every 30s, 1m, 5m or 15m"},
		{&resourcev1.AlertCheck{Address: "https://shop.example.com/", Every: "30s", Timeout: "30s"}, "less than how often it runs"},
		{&resourcev1.AlertCheck{Address: "https://shop.example.com/", Timeout: "45s"}, "30s at most"},
		{&resourcev1.AlertCheck{Address: "https://shop.example.com/", DownAfter: 11}, "1 to 10 failed runs"},
		{&resourcev1.AlertCheck{Address: "https://shop.example.com/", ExpectFrom: 400, ExpectTo: 200}, "lowest first"},
		{&resourcev1.AlertCheck{Address: "https://shop.example.com/", KeywordAbsent: true}, "needs the keyword"},
	} {
		err := (Check{}).Validate(tc.check)
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("Validate(%v) = %v, want it to say %q", tc.check, err, tc.says)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("a refusal repeats the password: %v", err)
		}
	}
}

func TestAnInstallWatchesItsChecksFromTheStart(t *testing.T) {
	var found *resourcev1.AlertRule
	for _, d := range Defaults() {
		if d.Name == "check-down" {
			found = d.Rule
		}
	}
	if found == nil {
		t.Fatal("no check-down rule among an install's first rules")
	}
	if err := (Rule{}).Validate(found); err != nil {
		t.Fatalf("the check-down rule doesn't validate: %v", err)
	}
	if found.GetSeverity() != resourcev1.AlertRule_SEVERITY_CRITICAL || found.GetRepeat() != "4h" || found.GetDuration() != "2m" || len(found.GetChannels()) != 0 {
		t.Errorf("check-down reads %v, want critical, held 2 minutes, again every 4 hours, to every channel", found)
	}
	// It holds for a duration, both ways, as every rule does, so it doesn't flap:
	// one left empty holds the 2 minutes.
	held := &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_CHECK_DOWN, Duration: "5m"}
	if err := (Rule{}).Validate(held); err != nil || held.GetDuration() != "5m" {
		t.Errorf("a check-down rule held 5 minutes = %v, %v", held, err)
	}
	empty := &resourcev1.AlertRule{Condition: resourcev1.AlertRule_CONDITION_CHECK_DOWN}
	if err := (Rule{}).Validate(empty); err != nil || empty.GetDuration() != "2m" {
		t.Errorf("a check-down rule with no duration = %v, %v", empty, err)
	}
}

// What a check can never reach from the control plane is refused as the plan is
// made, with what to do instead; a private address needs the check to say it's
// inside the network.
func TestACheckOfAnAddressItCantReachIsRefused(t *testing.T) {
	for _, tc := range []struct {
		check *resourcev1.AlertCheck
		says  string
	}{
		{&resourcev1.AlertCheck{Address: "http://192.168.1.10/"}, "only from inside the network"},
		{&resourcev1.AlertCheck{Address: "http://127.0.0.1:8080/", InsideNetwork: true}, "never reaches. Check the address people reach it at"},
		{&resourcev1.AlertCheck{Address: "http://localhost:8080/"}, "never reaches"},
		{&resourcev1.AlertCheck{Address: "http://169.254.169.254/latest/", InsideNetwork: true}, "never reaches"},
	} {
		err := (Check{}).Validate(tc.check)
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("Validate(%v) = %v, want it to say %q", tc.check, err, tc.says)
		}
	}
	inside := &resourcev1.AlertCheck{Address: "http://192.168.1.10/", InsideNetwork: true}
	if err := (Check{}).Validate(inside); err != nil {
		t.Errorf("a check of a private address from inside the network = %v", err)
	}
	if (Check{}).Cap() != 200 || !strings.Contains((Check{}).CapWords(200), "Remove a check") {
		t.Errorf("an install keeps %d checks: %q", (Check{}).Cap(), (Check{}).CapWords(200))
	}
}
