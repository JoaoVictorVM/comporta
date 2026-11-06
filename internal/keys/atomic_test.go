package keys

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFailedCommitPreservesPreviousDocument(t *testing.T) {
	failure := errors.New("injected")
	steps := map[string]func(*fileOps){
		"create":  func(o *fileOps) { o.create = func(string) (*os.File, error) { return nil, failure } },
		"write":   func(o *fileOps) { o.write = func(*os.File, []byte) error { return failure } },
		"sync":    func(o *fileOps) { o.sync = func(*os.File) error { return failure } },
		"replace": func(o *fileOps) { o.replace = func(string, string) error { return failure } },
	}
	for name, inject := range steps {
		t.Run(name, func(t *testing.T) {
			store := newTestStore(t)
			created := mustCreate(t, store, "bot")
			before := readFile(t, store.documentPath())

			inject(&store.ops)
			if _, err := store.Create(context.Background(), "other"); !errors.Is(err, ErrStoreIO) {
				t.Fatalf("create: %v", err)
			}
			if _, err := store.Revoke(context.Background(), created.Metadata.ID); !errors.Is(err, ErrStoreIO) {
				t.Fatalf("revoke: %v", err)
			}
			if after := readFile(t, store.documentPath()); after != before {
				t.Fatal("the previous document must stay intact")
			}
			temporaries, err := filepath.Glob(filepath.Join(store.dir, ".keys-*"))
			if err != nil || len(temporaries) != 0 {
				t.Fatalf("temporary files left behind: %v %v", temporaries, err)
			}
		})
	}
}

func TestCommitDurabilityFailureIsIndeterminate(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	store.ops.syncDir = func(string) error { return errors.New("injected") }

	if _, err := store.Revoke(context.Background(), created.Metadata.ID); !errors.Is(err, ErrIndeterminate) {
		t.Fatalf("err = %v", err)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].Active() {
		t.Fatal("a published revocation must not be undone")
	}
}

func TestAtomicReplacementReadersSeeCompleteJSON(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")

	var failures atomic.Int64
	done := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				if _, err := store.read(); err != nil {
					failures.Add(1)
					t.Errorf("reader saw %v", err)
					return
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
	used := created.Metadata.CreatedAt
	for range 60 {
		used = used.Add(time.Second)
		if err := store.mergeLastUsed(context.Background(), map[string]time.Time{created.Metadata.ID: used}); err != nil {
			t.Errorf("commit: %v", err)
			break
		}
	}
	close(done)
	readers.Wait()
	if failures.Load() != 0 {
		t.Fatal("readers must always see a complete document")
	}
}

func TestCanaryKeyAppearsOnlyInTheReturnedValue(t *testing.T) {
	store := newTestStore(t)
	var seen []string
	store.ops.write = func(file *os.File, data []byte) error {
		seen = append(seen, string(data))
		_, err := file.Write(data)
		return err
	}
	created := mustCreate(t, store, "bot")
	value := created.Key.Reveal()
	for _, content := range append(seen, readFile(t, store.documentPath())) {
		if strings.Contains(content, value) {
			t.Fatal("the key reached a file")
		}
	}
	for _, id := range []string{created.Metadata.ID, created.Metadata.App} {
		if strings.Contains(id, value) {
			t.Fatal("the key reached the metadata")
		}
	}
}
