package responsecache

import "sync"

// EpochTracker turns the invalidation revisions shared by the replicas into
// the apps this process must purge. Entries never leave the process; only the
// revision of each app is shared, so a deletion on one replica empties the
// cache of every other before it reports success.
type EpochTracker struct {
	mu   sync.Mutex
	seen map[string]int64
}

func NewEpochTracker() *EpochTracker { return &EpochTracker{seen: map[string]int64{}} }

// Changed records the revisions read and returns the apps whose revision
// moved since the previous call. The first call purges every app it sees
// with a revision, since this process cannot know what it cached before.
func (t *EpochTracker) Changed(revisions map[string]int64) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for app, revision := range revisions {
		if previous, ok := t.seen[app]; !ok || revision > previous {
			out = append(out, app)
		}
		t.seen[app] = revision
	}
	return out
}
