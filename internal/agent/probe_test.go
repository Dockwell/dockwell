// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	agentv1 "github.com/dockwell/dockwell/gen/dockwell/agent/v1"
)

func TestAProbeSaysWhatAnswers(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/down":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/moved":
			http.Redirect(w, r, "http://169.254.169.254/", http.StatusFound)
		default:
			_, _ = w.Write([]byte("hello"))
		}
	}))
	defer up.Close()
	_, port, _ := net.SplitHostPort(up.Listener.Addr().String())
	n, _ := strconv.Atoi(port)

	// A port that's closed: one that was listening a moment ago.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	for name, tc := range map[string]struct {
		call     *agentv1.ProbeCall
		answered bool
		says     string
	}{
		"a URL that answers":          {&agentv1.ProbeCall{Target: &agentv1.ProbeCall_Url{Url: up.URL + "/"}}, true, "200 OK"},
		"a URL that answers 503":      {&agentv1.ProbeCall{Target: &agentv1.ProbeCall_Url{Url: up.URL + "/down"}}, false, "503 Service Unavailable"},
		"a redirect isn't followed":   {&agentv1.ProbeCall{Target: &agentv1.ProbeCall_Url{Url: up.URL + "/moved"}}, true, "302 Found"},
		"a port that listens":         {&agentv1.ProbeCall{Target: &agentv1.ProbeCall_Port{Port: uint32(n)}}, true, "listens on port " + port},
		"a port that doesn't":         {&agentv1.ProbeCall{Target: &agentv1.ProbeCall_Port{Port: uint32(closed)}}, false, "nothing listens on port " + strconv.Itoa(closed)},
		"a URL nothing listens at":    {&agentv1.ProbeCall{Target: &agentv1.ProbeCall_Url{Url: "http://127.0.0.1:" + strconv.Itoa(closed)}}, false, "connection refused"},
		"a link-local address":        {&agentv1.ProbeCall{Target: &agentv1.ProbeCall_Url{Url: "http://169.254.169.254/latest/meta-data"}}, false, "link-local"},
		"a mapped link-local address": {&agentv1.ProbeCall{Target: &agentv1.ProbeCall_Url{Url: "http://[::ffff:169.254.169.254]/"}}, false, "link-local"},
	} {
		got := Probe(t.Context(), tc.call)
		if got.GetAnswered() != tc.answered || !strings.Contains(got.GetSaid(), tc.says) {
			t.Errorf("%s: answered %v, said %q; want %v, saying %q", name, got.GetAnswered(), got.GetSaid(), tc.answered, tc.says)
		}
	}
}
