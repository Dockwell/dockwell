// SPDX-License-Identifier: AGPL-3.0-only

package gitfetch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ErrCommitNotOnRef reports that the requested commit is not reachable from the
// branch or tag tip contained in the verified pack.
var ErrCommitNotOnRef = errors.New("commit is not on the requested branch or tag")

// maxAncestrySteps bounds the parent walk when verifying commit ancestry in a
// shallow pack.
const maxAncestrySteps = 1024

// VerifyPack checks a raw Git pack file on disk using gitfetch's SHA-1DC pack
// indexer and bounds (object count, single-object size, delta depth, ofs-delta
// only, and total inflation budget), verifies that commit is a valid commit
// object in the pack, and when refTip is non-empty and differs from commit,
// verifies that commit is an ancestor of refTip within the pack.
func VerifyPack(ctx context.Context, f *os.File, size, budget int64, commit, refTip string) error {
	if !Commit.MatchString(commit) {
		return fmt.Errorf("%q isn't a commit. Name one in full, as 40 hex digits", commit)
	}
	if refTip != "" && !Commit.MatchString(refTip) {
		return fmt.Errorf("%q isn't a commit. Name one in full, as 40 hex digits", refTip)
	}
	p, err := indexPack(ctx, f, size, budget)
	if err != nil {
		return err
	}
	c, err := p.object(commit, "commit", maxObject)
	if err != nil {
		return missing(err, "commit "+commit)
	}
	if _, _, err := readCommit(c); err != nil {
		return fmt.Errorf("the commit %s %w", commit, err)
	}
	if refTip == "" || refTip == commit {
		return nil
	}
	if ok, err := isAncestor(ctx, p, refTip, commit); err != nil {
		return err
	} else if !ok {
		return ErrCommitNotOnRef
	}
	return nil
}

// isAncestor walks parent pointers in p from tip looking for want.
func isAncestor(ctx context.Context, p *pack, tip, want string) (bool, error) {
	seen := map[string]bool{tip: true}
	queue := []string{tip}
	for steps := 0; len(queue) > 0 && steps < maxAncestrySteps; steps++ {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		cur := queue[0]
		queue = queue[1:]
		if cur == want {
			return true, nil
		}
		raw, err := p.object(cur, "commit", maxObject)
		if err != nil {
			if errors.Is(err, errMissing) {
				// Shallow boundary: parent isn't in the pack.
				if cur == tip {
					return false, missing(err, "commit "+tip)
				}
				continue
			}
			return false, err
		}
		for _, parent := range commitParents(raw) {
			if parent == want {
				return true, nil
			}
			if !seen[parent] {
				seen[parent] = true
				queue = append(queue, parent)
			}
		}
	}
	return false, nil
}

func commitParents(c []byte) []string {
	var out []string
	for line := range strings.Lines(string(c)) {
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			break
		}
		if p, ok := strings.CutPrefix(line, "parent "); ok && Commit.MatchString(p) {
			out = append(out, p)
		}
	}
	return out
}
