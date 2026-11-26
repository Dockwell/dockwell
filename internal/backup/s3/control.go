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
	return b.putWhen(ctx, key, body, "If-None-Match", "*")
}

// PutIfMatch updates a marker only if its current ETag is the one just read.
func (b *Bucket) PutIfMatch(ctx context.Context, key string, body []byte, etag string) error {
	if etag == "" || etag == "*" {
		return errors.New("the storage did not return a usable marker ETag")
	}
	return b.putWhen(ctx, key, body, "If-Match", etag)
}

func (b *Bucket) putWhen(ctx context.Context, key string, body []byte, condition, etag string) error {
	sum := sha256.Sum256(body)
	res, err := b.request(ctx, http.MethodPut, key, nil, bytes.NewReader(body), int64(len(body)), hex.EncodeToString(sum[:]), condition, etag)
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

// GetWithETag reads a marker and its version validator.
func (b *Bucket) GetWithETag(ctx context.Context, key string) (io.ReadCloser, string, error) {
	res, err := b.do(ctx, http.MethodGet, key, nil, nil, 0, emptyHash)
	if err != nil {
		return nil, "", err
	}
	return res.Body, res.Header.Get("ETag"), nil
}

// Versioned reports whether a bucket has retained versions, including suspended buckets.
func (b *Bucket) Versioned(ctx context.Context) (bool, error) {
	res, err := b.do(ctx, http.MethodGet, "", url.Values{"versioning": {""}}, nil, 0, emptyHash)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	var config struct {
		XMLName xml.Name `xml:"VersioningConfiguration"`
		Status  string
	}
	if err := xml.NewDecoder(io.LimitReader(res.Body, 16<<10)).Decode(&config); err != nil {
		return false, err
	}
	switch config.Status {
	case "":
		return false, nil
	case "Enabled", "Suspended":
		return true, nil
	default:
		return false, errors.New("the storage returned an unknown bucket versioning state")
	}
}

// ObjectVersion is an individual stored version or delete marker.
type ObjectVersion struct {
	Key          string
	VersionID    string `xml:"VersionId"`
	DeleteMarker bool   `xml:"-"`
}

// ListObjectVersions returns the complete listing, including hidden versions.
func (b *Bucket) ListObjectVersions(ctx context.Context, prefix string) ([]ObjectVersion, error) {
	q := url.Values{"versions": {""}, "max-keys": {"1000"}, "prefix": {prefix}}
	var versions []ObjectVersion
	for range 100 {
		res, err := b.do(ctx, http.MethodGet, "", q, nil, 0, emptyHash)
		if err != nil {
			return nil, err
		}
		var page struct {
			XMLName             xml.Name `xml:"ListVersionsResult"`
			IsTruncated         bool
			NextKeyMarker       string
			NextVersionIDMarker string          `xml:"NextVersionIdMarker"`
			Versions            []ObjectVersion `xml:"Version"`
			DeleteMarkers       []ObjectVersion `xml:"DeleteMarker"`
		}
		err = xml.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&page)
		_ = res.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, v := range page.DeleteMarkers {
			v.DeleteMarker = true
			page.Versions = append(page.Versions, v)
		}
		for _, v := range page.Versions {
			if v.VersionID == "" {
				return nil, errors.New("the storage returned a version without an ID")
			}
			versions = append(versions, v)
		}
		if !page.IsTruncated {
			return versions, nil
		}
		if page.NextKeyMarker == "" || page.NextKeyMarker == q.Get("key-marker") && page.NextVersionIDMarker == q.Get("version-id-marker") {
			return nil, errors.New("the storage returned an incomplete backup version listing")
		}
		q.Set("key-marker", page.NextKeyMarker)
		q.Set("version-id-marker", page.NextVersionIDMarker)
	}
	return nil, errors.New("the storage returned too many backup version pages")
}

// DeleteVersion removes a version without creating a delete marker.
func (b *Bucket) DeleteVersion(ctx context.Context, key, version string) error {
	res, err := b.do(ctx, http.MethodDelete, key, url.Values{"versionId": {version}}, nil, 0, emptyHash)
	if err != nil {
		if IsNotFound(err) {
			return nil
		}
		return err
	}
	return res.Body.Close()
}

// ObjectRetention reads an object lock's expiry when the bucket key permits it.
func (b *Bucket) ObjectRetention(ctx context.Context, key, version string) (time.Time, error) {
	q := url.Values{"retention": {""}}
	if version != "" {
		q.Set("versionId", version)
	}
	res, err := b.do(ctx, http.MethodGet, key, q, nil, 0, emptyHash)
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
