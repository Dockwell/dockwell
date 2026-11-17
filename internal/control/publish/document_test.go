// SPDX-License-Identifier: AGPL-3.0-only

package publish

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// full is a tunnel's document with every key Cloudflare documents, at the top and in
// its rules, keys it doesn't have yet at each level, a number past what a float
// holds exactly, and Cloudflare's own spacing (ADR 0104, H1).
const full = `{
 "ingress": [
  {
   "service": "http://127.0.0.1:7600",
   "hostname": "panel.example.com",
   "originRequest": {
    "noTLSVerify": true,
    "httpHostHeader": "panel.example.com",
    "access": {"required": true, "teamName": "team", "audTag": ["aud"]},
    "dockwellFutureRuleOR": {"x": 1}
   },
   "dockwellFutureRule": "y"
  },
  {
   "path": "^/api",
   "service": "http://127.0.0.1:18786",
   "hostname": "api.example.com"
  },
  {
   "service": "http_status:404"
  }
 ],
 "warp-routing": {
  "enabled": false
 },
 "originRequest": {
  "connectTimeout": 10,
  "tlsTimeout": 10,
  "tcpKeepAlive": 30,
  "noHappyEyeballs": false,
  "keepAliveConnections": 100,
  "keepAliveTimeout": 90,
  "httpHostHeader": "",
  "originServerName": "",
  "caPool": "",
  "noTLSVerify": false,
  "disableChunkedEncoding": false,
  "bastionMode": false,
  "proxyAddress": "127.0.0.1",
  "proxyPort": 0,
  "proxyType": "",
  "ipRules": [{"prefix": "10.0.0.0/8", "ports": [80, 443], "allow": false}],
  "http2Origin": false,
  "matchSNItoHost": false,
  "dockwellFutureOR": 12345678901234567890
 },
 "dockwellFuture": {"nested": [1, 2, 3]}
}`

func TestInsertSplicesOneRuleAndKeepsEveryOtherByte(t *testing.T) {
	doc, err := Parse(json.RawMessage(full))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Rules) != 3 || !doc.HasCatchAll() || doc.Rules[0].Hostname != "panel.example.com" || doc.Rules[1].Path != "^/api" {
		t.Fatalf("read %+v", doc.Rules)
	}
	rule := Rule{Hostname: "*.example.com", Service: "http://127.0.0.1:80"}
	after, at, err := doc.Insert(rule)
	if err != nil || at != 2 {
		t.Fatalf("insert: %d %v", at, err)
	}
	// Byte for byte, everything but the one rule and its comma is as it was read.
	elem, _ := json.Marshal(rule)
	spliced := after.Raw()
	cut := bytes.Index(spliced, elem)
	if cut < 0 || !bytes.Equal(append(bytes.Clone(spliced[:cut]), spliced[cut+len(elem)+1:]...), []byte(full)) {
		t.Fatalf("the document changed beyond the rule:\n%s", spliced)
	}
	if err := CheckSplice(json.RawMessage(full), spliced, at, true); err != nil {
		t.Fatalf("the check refused a clean splice: %v", err)
	}
	// Taking it out again gives back exactly what was read.
	back, err := after.Remove(at)
	if err != nil || !bytes.Equal(back.Raw(), []byte(full)) {
		t.Fatalf("remove: %v\n%s", err, back.Raw())
	}
	if err := CheckSplice(spliced, back.Raw(), at, false); err != nil {
		t.Fatalf("the check refused a clean removal: %v", err)
	}
}

func TestCheckSpliceRefusesMoreThanOneRule(t *testing.T) {
	doc, _ := Parse(json.RawMessage(full))
	after, at, _ := doc.Insert(Rule{Hostname: "*.example.com", Service: "http://127.0.0.1:80"})
	good := string(after.Raw())
	for name, bad := range map[string]string{
		"a top-level setting changed": strings.Replace(good, `"connectTimeout": 10`, `"connectTimeout": 11`, 1),
		"an unknown key dropped": strings.Replace(good, `,
 "dockwellFuture": {"nested": [1, 2, 3]}`, "", 1),
		"a large number changed":  strings.Replace(good, "12345678901234567890", "12345678901234567891", 1),
		"a rule's setting gone":   strings.Replace(good, `"dockwellFutureRule": "y"`, `"dockwellFutureRule": "z"`, 1),
		"warp routing turned on":  strings.Replace(good, `"enabled": false`, `"enabled": true`, 1),
		"a new top-level setting": strings.Replace(good, `"dockwellFuture"`, `"added": 1, "dockwellFuture"`, 1),
	} {
		if bad == good {
			t.Fatalf("%s: the fixture didn't change", name)
		}
		if err := CheckSplice(json.RawMessage(full), json.RawMessage(bad), at, true); !errors.Is(err, ErrMoreChanged) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Two rules swapped is a change of routing, which a set would miss.
	swapped := `{"ingress":[{"hostname":"api.example.com","path":"^/api","service":"http://127.0.0.1:18786"},{"hostname":"*.example.com","service":"http://127.0.0.1:80"},{"hostname":"panel.example.com","service":"http://127.0.0.1:7600"},{"service":"http_status:404"}]}`
	before := `{"ingress":[{"hostname":"panel.example.com","service":"http://127.0.0.1:7600"},{"hostname":"api.example.com","path":"^/api","service":"http://127.0.0.1:18786"},{"service":"http_status:404"}]}`
	if err := CheckSplice(json.RawMessage(before), json.RawMessage(swapped), 2, true); !errors.Is(err, ErrMoreChanged) {
		t.Errorf("a reordered list: %v", err)
	}
	// Equal numbers written alike still compare equal.
	if err := CheckSplice(json.RawMessage(`{"a":1,"ingress":[{"service":"http_status:404"}]}`), json.RawMessage(`{"ingress":[{"hostname":"x.example.com","service":"http://127.0.0.1:80"},{"service":"http_status:404"}],"a":1}`), 0, true); err != nil {
		t.Errorf("key order alone: %v", err)
	}
}

func TestADocumentFromNothing(t *testing.T) {
	doc, err := Parse(json.RawMessage("null"))
	if err != nil || !doc.Empty() {
		t.Fatalf("a new tunnel: %v", err)
	}
	after, at, err := doc.Insert(Rule{Hostname: "*.example.com", Service: "http://127.0.0.1:80"})
	if err != nil || at != 0 || len(after.Rules) != 2 || !after.HasCatchAll() {
		t.Fatalf("from nothing: %v %+v", err, after)
	}
	if err := CheckSplice(json.RawMessage("null"), after.Raw(), 0, true); err != nil {
		t.Fatalf("from nothing, checked: %v", err)
	}
	// Settings and no ingress are someone else's to finish, and no catch-all isn't
	// a document Cloudflare takes.
	for _, raw := range []string{`{"originRequest":{"connectTimeout":5}}`, `{"ingress":[{"hostname":"a.example.com","service":"http://127.0.0.1:1"}]}`} {
		d, err := Parse(json.RawMessage(raw))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := d.Insert(Rule{Hostname: "*.example.com", Service: "http://127.0.0.1:80"}); err == nil {
			t.Errorf("%s: should refuse", raw)
		}
	}
	if _, err := Parse(json.RawMessage(`[1]`)); !errors.Is(err, ErrNoDocument) {
		t.Errorf("not an object: %v", err)
	}
}
