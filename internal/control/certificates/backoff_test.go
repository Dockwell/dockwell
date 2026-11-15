// SPDX-License-Identifier: AGPL-3.0-only

package certificates

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A request the CA turns away is tried again after its Retry-After, or 1, 2, 4
// seconds and on, a few times at most. None is started that can't end before the
// call's deadline, or that waits past caMaxWait, so a CA that's down is said in its
// own words, not as a deadline that ran out mid-request (#716).
func TestARetryOfTheCAEndsBeforeTheCallsDeadline(t *testing.T) {
	answer := func(retryAfter string) *http.Response {
		res := &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{}, Body: http.NoBody}
		if retryAfter != "" {
			res.Header.Set("Retry-After", retryAfter)
		}
		return res
	}
	open := httptest.NewRequest(http.MethodGet, "https://ca.example.com/dir", http.NoBody)
	soon, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tight := open.WithContext(soon)
	for _, tc := range []struct {
		what       string
		n          int
		r          *http.Request
		retryAfter string
		want       time.Duration
	}{
		{"the first retry", 1, open, "", time.Second},
		{"the third", 3, open, "", 4 * time.Second},
		{"one past caRetries", caRetries + 1, open, "", 0},
		{"one the CA asks to wait for", 1, open, "5", 5 * time.Second},
		{"one the CA asks to wait past caMaxWait for", 1, open, "3600", 0},
		{"one that fits before the deadline", 1, tight, "", time.Second},
		{"one that can't end before the deadline", 2, tight, "", 0},
	} {
		res := answer(tc.retryAfter)
		got := caBackoff(tc.n, tc.r, res)
		_ = res.Body.Close()
		if got != tc.want {
			t.Errorf("%s waits %s, want %s", tc.what, got, tc.want)
		}
	}
}
