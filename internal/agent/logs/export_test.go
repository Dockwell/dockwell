// SPDX-License-Identifier: AGPL-3.0-only

package logs

// WaitPruned waits for what Finish left pruning on its own time.
func (a *Archive) WaitPruned() { a.pruning.Wait() }
