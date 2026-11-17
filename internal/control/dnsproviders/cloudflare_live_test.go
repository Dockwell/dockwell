// SPDX-License-Identifier: AGPL-3.0-only

package dnsproviders_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dockwell/dockwell/internal/agent/tunnels"
	"github.com/dockwell/dockwell/internal/control/dnsproviders"
)

// The checks ADR 0104 rests on, made against Cloudflare itself. They run only when
// DOCKWELL_TEST_CLOUDFLARE names a command that sends one call to a scratch zone and
// prints Cloudflare's answer with its HTTP status as "status": a guard that holds
// every call to a test zone and to tunnels named dockwell-test-*, fills in {zone}
// and {account}, and hides secrets. The token never reaches this test. With
// DOCKWELL_TEST_CLOUDFLARED naming cloudflared, a scratch tunnel is also run, to read
// its connector ID as the agent does. Everything made is removed after.

// guardDoer sends each request through the guard command.
type guardDoer struct{ command string }

func (g guardDoer) Do(req *http.Request) (*http.Response, error) {
	path := strings.TrimPrefix(req.URL.Path, "/client/v4")
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}
	args := []string{"-c", g.command + ` "$@"`, "guard", req.Method, path}
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		f, err := os.CreateTemp("", "cf-body-*.json")
		if err != nil {
			return nil, err
		}
		defer os.Remove(f.Name())
		if _, err := f.Write(body); err != nil {
			return nil, err
		}
		f.Close()
		args = append(args, f.Name())
	}
	out, err := exec.CommandContext(req.Context(), "sh", args...).Output()
	var said struct {
		Status  int    `json:"status"`
		Refused string `json:"refused"`
	}
	if jerr := json.Unmarshal(out, &said); jerr != nil {
		return nil, fmt.Errorf("the guard answered with no JSON: %w", errors.Join(err, jerr))
	}
	if said.Refused != "" {
		return nil, fmt.Errorf("the guard refused %s %s: %s", req.Method, path, said.Refused)
	}
	return &http.Response{StatusCode: said.Status, Body: io.NopCloser(bytes.NewReader(out)), Header: http.Header{}, Request: req}, nil
}

func liveCloudflare(t *testing.T) *dnsproviders.Cloudflare {
	t.Helper()
	command := os.Getenv("DOCKWELL_TEST_CLOUDFLARE")
	if command == "" {
		t.Skip("DOCKWELL_TEST_CLOUDFLARE names no guard to a scratch Cloudflare zone")
	}
	return &dnsproviders.Cloudflare{Base: "https://guard.invalid/client/v4", Client: guardDoer{command: command}}
}

func scratchName(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func decoded(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	return v
}

func apiCode(err error) int {
	var api *dnsproviders.APIError
	if errors.As(err, &api) {
		return api.Code
	}
	return 0
}

func TestCloudflareLive(t *testing.T) {
	cf := liveCloudflare(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	const account = "{account}"

	// A token reads its own rights (Verified, 11).
	id, status, err := cf.VerifyAccountToken(ctx, account)
	if err != nil || id == "" || status != "active" {
		t.Fatalf("verify at the account: %q %q %v", id, status, err)
	}
	rights, err := cf.AccountTokenRights(ctx, account, id)
	if err != nil {
		t.Fatalf("the token's own record: %v", err)
	}
	if len(rights.Policies) == 0 || len(rights.Policies[0].Groups) == 0 || rights.Policies[0].Groups[0].ID == "" || len(rights.Policies[0].Resources) == 0 {
		t.Fatalf("a token's record should list rights by ID with their scope: %+v", rights)
	}

	// A zone's name, account and SSL mode (Verified, 7). Its name comes from the
	// API, so nothing here names it.
	zinfo, err := cf.ZoneByID(ctx, "{zone}")
	if err != nil || zinfo.Name == "" || zinfo.Account == "" {
		t.Fatalf("the zone: %+v %v", zinfo, err)
	}
	zone := dnsproviders.Zone{ID: "{zone}", Name: zinfo.Name}
	if mode, _, err := cf.SSLMode(ctx, zone); err != nil || mode == "" {
		t.Fatalf("the zone's SSL mode: %q %v", mode, err)
	}

	// A remotely managed tunnel, made with a secret generated here (Verified, 5).
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		t.Fatal(err)
	}
	secret := base64.StdEncoding.EncodeToString(secretBytes)
	tunnel, err := cf.CreateTunnel(ctx, account, "dockwell-test-"+scratchName(t), secret)
	if err != nil || tunnel.ID == "" {
		t.Fatalf("making a scratch tunnel: %v", err)
	}
	t.Cleanup(func() {
		// A tunnel whose connector just stopped is refused for a minute or so, while
		// its connections close (1022), so this waits and tries again.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		for {
			err := cf.DeleteTunnel(ctx, account, tunnel.ID)
			if err == nil {
				return
			}
			if apiCode(err) != 1022 || ctx.Err() != nil {
				t.Errorf("the scratch tunnel %s is left: %v", tunnel.ID, err)
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Second):
			}
		}
	})

	t.Run("a tunnel's document", func(t *testing.T) {
		got, err := cf.ReadTunnelConfig(ctx, account, tunnel.ID)
		if err != nil || got.Version != 0 || !got.Empty() || got.Source != "cloudflare" {
			t.Fatalf("a new tunnel reads version 0 with no document: %+v %v", got, err)
		}
		host := "dwtest-a." + zone.Name
		full := json.RawMessage(`{"originRequest":{"connectTimeout":10,"noTLSVerify":true,"keepAliveTimeout":60},` +
			`"dockwellFuture":{"x":1},"ingress":[` +
			`{"hostname":"` + host + `","service":"http://127.0.0.1:18785","originRequest":{"httpHostHeader":"rule.example","dockwellFutureRuleOR":true},"dockwellFutureRule":"y"},` +
			`{"hostname":"` + zone.Name + `","service":"http://127.0.0.1:18785"},` +
			`{"hostname":"*.` + zone.Name + `","service":"http://127.0.0.1:18785"},` +
			`{"hostname":"` + host + `","path":"^/api","service":"http://127.0.0.1:18786"},` +
			`{"service":"http_status:404"}]}`)
		wrote, err := cf.WriteTunnelConfig(ctx, account, tunnel.ID, full)
		if err != nil || wrote.Version != 1 {
			t.Fatalf("a write that changes the document adds one, and says so: %+v %v", wrote, err)
		}
		read, err := cf.ReadTunnelConfig(ctx, account, tunnel.ID)
		if err != nil || read.Version != 1 {
			t.Fatalf("read back: %+v %v", read, err)
		}
		// Every key, unknown ones too, comes back equal, though not in its order.
		want := decoded(t, full).(map[string]any)
		back := decoded(t, read.Config).(map[string]any)
		delete(back, "warp-routing") // Cloudflare adds it, and it can't be set here
		if !reflect.DeepEqual(want, back) {
			t.Fatalf("what was written didn't come back:\n%s\n%s", full, read.Config)
		}
		// Writing back what was read, exactly, changes nothing, and moves no version.
		same, err := cf.WriteTunnelConfig(ctx, account, tunnel.ID, read.Config)
		if err != nil || same.Version != 1 {
			t.Fatalf("an unchanged write keeps the version: %+v %v", same, err)
		}
		// Cloudflare refuses a catch-all that isn't last, and that moves nothing.
		bad := json.RawMessage(`{"ingress":[{"service":"http_status:404"},{"hostname":"` + host + `","service":"http://127.0.0.1:18785"}]}`)
		if _, err := cf.WriteTunnelConfig(ctx, account, tunnel.ID, bad); err == nil {
			t.Fatal("a catch-all before a rule should be refused")
		}
		// A document that leaves the top-level originRequest out drops it.
		bare := json.RawMessage(`{"ingress":[{"hostname":"` + host + `","service":"http://127.0.0.1:18785"},{"service":"http_status:404"}]}`)
		if w, err := cf.WriteTunnelConfig(ctx, account, tunnel.ID, bare); err != nil || w.Version != 2 {
			t.Fatalf("a bare document: %+v %v", w, err)
		}
		read, err = cf.ReadTunnelConfig(ctx, account, tunnel.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, kept := decoded(t, read.Config).(map[string]any)["originRequest"]; kept {
			t.Fatal("a PUT replaces the whole document, so what it leaves out goes")
		}
	})

	t.Run("records", func(t *testing.T) {
		comment := "dockwell test " + scratchName(t)
		made := []string{}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			for _, id := range made {
				if err := cf.Delete(ctx, zone, id); err != nil && apiCode(err) != 81044 {
					t.Errorf("a scratch record %s is left: %v", id, err)
				}
			}
		})
		create := func(r dnsproviders.Record) (dnsproviders.Record, error) {
			got, err := cf.CreateRecord(ctx, zone, r)
			if err == nil {
				made = append(made, got.ID)
			}
			return got, err
		}
		name := "dwtest-" + scratchName(t) + "." + zone.Name
		cname := dnsproviders.Record{Type: "CNAME", Name: name, Content: tunnel.ID + ".cfargotunnel.com", Proxied: true, TTL: 1, Comment: comment}
		got, err := create(cname)
		if err != nil || got.Comment != comment || !got.Proxied {
			t.Fatalf("a proxied CNAME to the tunnel: %+v %v", got, err)
		}
		if back, err := cf.RecordByID(ctx, zone, got.ID); err != nil || back.Content != cname.Content {
			t.Fatalf("read back by ID: %+v %v", back, err)
		}
		if listed, err := cf.RecordsCommented(ctx, zone, comment); err != nil || len(listed) != 1 || listed[0].ID != got.ID {
			t.Fatalf("listed by its comment's prefix: %+v %v", listed, err)
		}
		if err := cf.Delete(ctx, zone, got.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := cf.RecordByID(ctx, zone, got.ID); apiCode(err) != 81044 {
			t.Fatalf("a deleted record reads as gone (81044): %v", err)
		}

		// A CNAME sits beside TXT and MX, and not beside an A (81053) (Verified, 4).
		beside := "dwtest-" + scratchName(t) + "." + zone.Name
		ten := 10
		for _, r := range []dnsproviders.Record{
			{Type: "TXT", Name: beside, Content: `"dockwell test"`, TTL: 1, Comment: comment},
			{Type: "MX", Name: beside, Content: "mx.invalid", Priority: &ten, TTL: 1, Comment: comment},
			{Type: "CNAME", Name: beside, Content: tunnel.ID + ".cfargotunnel.com", Proxied: true, TTL: 1, Comment: comment},
		} {
			if _, err := create(r); err != nil {
				t.Fatalf("%s at %s: %v", r.Type, beside, err)
			}
		}
		if all, err := cf.RecordsAt(ctx, zone, beside); err != nil || len(all) != 3 {
			t.Fatalf("every type at the name: %+v %v", all, err)
		}
		held := "dwtest-" + scratchName(t) + "." + zone.Name
		if _, err := create(dnsproviders.Record{Type: "A", Name: held, Content: "192.0.2.1", Proxied: true, TTL: 1, Comment: comment}); err != nil {
			t.Fatal(err)
		}
		onA := cname
		onA.Name = held
		if _, err := create(onA); apiCode(err) != 81053 {
			t.Fatalf("a CNAME beside an A should be refused with 81053: %v", err)
		}
	})

	t.Run("a connector proves itself by its ID", func(t *testing.T) {
		binary := os.Getenv("DOCKWELL_TEST_CLOUDFLARED")
		if binary == "" {
			t.Skip("DOCKWELL_TEST_CLOUDFLARED names no cloudflared")
		}
		token, err := json.Marshal(map[string]string{"a": tunnel.AccountTag, "t": tunnel.ID, "s": secret})
		if err != nil {
			t.Fatal(err)
		}
		tokenFile := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(tokenFile, []byte(base64.StdEncoding.EncodeToString(token)), 0o600); err != nil {
			t.Fatal(err)
		}
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		metrics := l.Addr().String()
		l.Close()
		var log bytes.Buffer
		run := exec.Command(binary, "tunnel", "--no-autoupdate", "--metrics", metrics, "run", "--token-file", tokenFile)
		run.Stdout, run.Stderr = &log, &log
		if err := run.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_ = run.Process.Signal(os.Interrupt)
			done := make(chan struct{})
			go func() { _ = run.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				_ = run.Process.Kill()
				<-done
			}
			if strings.Contains(log.String(), secret) {
				t.Error("cloudflared's log holds the tunnel's secret")
			}
		}()
		var ready tunnels.Ready
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			ready, err = tunnels.ReadReady(ctx, metrics, nil)
			if err == nil && ready.Connections > 0 {
				break
			}
			time.Sleep(time.Second)
		}
		if ready.ConnectorID == "" {
			t.Fatalf("cloudflared's /ready gave no connector ID: %v", err)
		}
		for time.Now().Before(deadline) {
			connectors, err := cf.TunnelConnectors(ctx, account, tunnel.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range connectors {
				if c.ID == ready.ConnectorID && len(c.Connections) > 0 && c.Connections[0].OriginIP != "" {
					return
				}
			}
			time.Sleep(3 * time.Second)
		}
		t.Fatalf("the connections list never carried the connector %s", ready.ConnectorID)
	})
}
