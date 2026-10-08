package historysync

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// publicationPauseChecker is optional. A pause only avoids repeatedly decoding
// an unchanged pending object; all new objects and Install guards remain intact.
type publicationPauseChecker interface {
	PublicationPauseReason(context.Context) string
}

func (n *nativeAdapter) PublicationPauseReason(ctx context.Context) string {
	n.mu.Lock()
	guard := n.processGuard
	n.mu.Unlock()
	if guard == nil {
		return "native process guard unavailable; publication deferred"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	running, err := guard(ctx)
	if err != nil {
		return "native process guard unavailable; publication deferred: " + err.Error()
	}
	if running {
		return "native Codex processes are running; publication deferred"
	}
	return ""
}

// pendingObjectPresent deliberately checks metadata and existence only. Its
// contents will receive full decode/integrity validation when publication resumes.
func (r *Relay) pendingObjectPresent(head Head, pending map[string]Head) bool {
	old, ok := pending[head.Entry.ID]
	if !ok || old != head || !validDigest(objectDigest(head)) {
		return false
	}
	stat, err := os.Lstat(filepath.Join(r.cfg.Store, "pending", objectDigest(head)+".json.gz"))
	return err == nil && stat.Mode().IsRegular() && stat.Size() > 0 && stat.Size() <= MaxBundleBytes+(1<<20)
}
