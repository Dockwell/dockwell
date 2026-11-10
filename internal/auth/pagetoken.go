// SPDX-License-Identifier: AGPL-3.0-only

package auth

import (
	"encoding/base64"
	"errors"
	"strings"
)

// pageTokenPrefix marks a page token sealed for one person.
const pageTokenPrefix = "dwp1."

// ErrPageToken is a page token this install didn't give the caller.
var ErrPageToken = errors.New("that page token isn't one this install gave you. Start again from the first page")

// SealPageToken makes a page token opaque to whoever reads a list narrowed to
// their places, and good only for them: it says where the next page starts,
// which may be an object out of their reach (ADR 0049).
func (s *Service) SealPageToken(userID, token string) string {
	if token == "" {
		return ""
	}
	return pageTokenPrefix + base64.RawURLEncoding.EncodeToString(s.key.Seal(pagePurpose(userID), []byte(token)))
}

// OpenPageToken reads a page token SealPageToken gave userID.
func (s *Service) OpenPageToken(userID, sealed string) (string, error) {
	raw, ok := strings.CutPrefix(sealed, pageTokenPrefix)
	if !ok {
		return "", ErrPageToken
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", ErrPageToken
	}
	plain, err := s.key.Open(pagePurpose(userID), b)
	if err != nil {
		return "", ErrPageToken
	}
	return string(plain), nil
}

func pagePurpose(userID string) string { return "page token for " + userID }
