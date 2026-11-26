// SPDX-License-Identifier: AGPL-3.0-only

//go:build !agent || fullapi

package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

// PutIfAbsent writes an immutable marker. A caller can read the existing marker
// after a failed precondition without ever overwriting a newer generation.
func (b *Bucket) PutIfAbsent(ctx context.Context, key string, body []byte) error {
	sum := sha256.Sum256(body)
	res, err := b.request(ctx, http.MethodPut, key, nil, bytes.NewReader(body), int64(len(body)), hex.EncodeToString(sum[:]), "*")
	if err != nil {
		return err
	}
	return res.Body.Close()
}

// PutReader writes a body with a precomputed SHA-256 sum.
func (b *Bucket) PutReader(ctx context.Context, key string, body io.Reader, length int64, sum []byte) error {
	res, err := b.do(ctx, http.MethodPut, key, nil, body, length, hex.EncodeToString(sum))
	if err != nil {
		return err
	}
	return res.Body.Close()
}

// ListObjects walks the complete listing so a partial page can never cause pruning.
func (b *Bucket) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	q := url.Values{"list-type": {"2"}, "max-keys": {"1000"}, "prefix": {prefix}}
	var keys []string
	// A complete generation normally holds 31 objects. Refuse an unbounded
	// listing without attempting any retention deletion.
	for range 100 {
		res, err := b.do(ctx, http.MethodGet, "", q, nil, 0, emptyHash)
		if err != nil {
			return nil, err
		}
		var page struct {
			XMLName               xml.Name `xml:"ListBucketResult"`
			IsTruncated           bool
			NextContinuationToken string
			Contents              []struct{ Key string }
		}
		err = xml.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&page)
		_ = res.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, entry := range page.Contents {
			keys = append(keys, entry.Key)
		}
		if !page.IsTruncated {
			return keys, nil
		}
		token := page.NextContinuationToken
		if token == "" || token == q.Get("continuation-token") {
			return nil, errors.New("the storage returned an incomplete backup listing")
		}
		q.Set("continuation-token", token)
	}
	return nil, errors.New("the storage returned too many backup pages")
}

// ObjectRetention reads an object lock's expiry when the bucket key permits it.
func (b *Bucket) ObjectRetention(ctx context.Context, key string) (time.Time, error) {
	res, err := b.do(ctx, http.MethodGet, key, url.Values{"retention": {""}}, nil, 0, emptyHash)
	if err != nil {
		return time.Time{}, err
	}
	defer res.Body.Close()
	var retention struct {
		XMLName         xml.Name `xml:"Retention"`
		RetainUntilDate time.Time
	}
	err = xml.NewDecoder(io.LimitReader(res.Body, 16<<10)).Decode(&retention)
	return retention.RetainUntilDate, err
}
