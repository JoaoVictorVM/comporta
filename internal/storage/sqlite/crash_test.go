package sqlite

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func TestCrashHelper(t *testing.T) {
	mode := os.Getenv("COMPORTA_CRASH_TEST_MODE")
	if mode == "" {
		return
	}
	s, err := Open(context.Background(), os.Getenv("COMPORTA_CRASH_TEST_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(mode, "limits-") {
		limitsCrashHelper(t, s, mode)
		fmt.Println("ready")
		select {}
	}
	r := fixture()
	if err = s.Admit(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if mode != "admitted" {
		if err = s.Complete(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "committed" {
		if err = s.Commit(context.Background(), []storage.Record{r}); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "reset" {
		if err = s.Reset(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Println("ready")
	select {}
}

func TestCrashRecoveryDoesNotDoubleCharge(t *testing.T) {
	for _, mode := range []string{"admitted", "completed", "committed", "reset"} {
		t.Run(mode, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "private")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashHelper$")
			child.Env = append(os.Environ(), "COMPORTA_CRASH_TEST_MODE="+mode, "COMPORTA_CRASH_TEST_DIR="+dir)
			stdout, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err = child.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { child.Process.Kill(); child.Wait() })
			scanner := bufio.NewScanner(stdout)
			ready := false
			for scanner.Scan() {
				if scanner.Text() == "ready" {
					ready = true
					break
				}
			}
			if !ready {
				t.Fatal("child did not reach crash barrier")
			}
			if err = child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			child.Wait()
			s, err := Open(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.Recover(context.Background(), time.Now()); err != nil {
				t.Fatal(err)
			}
			if err = s.Recover(context.Background(), time.Now()); err != nil {
				t.Fatal(err)
			}
			rows, err := s.Daily(context.Background(), storage.Filter{AppName: "bot"})
			want := int64(49)
			if mode == "admitted" {
				want = 100
			}
			if err != nil || len(rows) != 1 || int64(rows[0].Cost) != want || rows[0].Requests != 1 {
				t.Fatalf("recovery totals: %+v %v", rows, err)
			}
		})
	}
}

// limitsCrashHelper leaves the database at a crash barrier with reservations of
// 100 each: admitted, completed in the journal only, settled with the result,
// or admitted and covered by a checkpoint.
func limitsCrashHelper(t *testing.T, s *Store, mode string) {
	if mode == "limits-reset" {
		limitHistory(t, s)
		if err := s.Reset(context.Background()); err != nil {
			t.Fatal(err)
		}
		return
	}
	m := limitManager(t, s)
	n := 1
	if mode == "limits-admitted-many" {
		n = 3
	}
	for i := range n {
		r := limitRecord(i + 1)
		h, err := admitLimited(m, s, r)
		if err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "limits-journal-result":
			if err = s.Complete(context.Background(), completed(r)); err != nil {
				t.Fatal(err)
			}
		case "limits-settled":
			if err = settleCompleted(m, s, h, completed(r)); err != nil {
				t.Fatal(err)
			}
		case "limits-checkpointed":
			if err = m.Tick(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func crashAt(t *testing.T, mode string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashHelper$")
	child.Env = append(os.Environ(), "COMPORTA_CRASH_TEST_MODE="+mode, "COMPORTA_CRASH_TEST_DIR="+dir)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(stdout)
	ready := false
	for scanner.Scan() {
		if scanner.Text() == "ready" {
			ready = true
			break
		}
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	if !ready {
		t.Fatal("child did not reach crash barrier")
	}
	return dir
}

// restartLimits recovers as serve does: usage journal first, then limits.
func restartLimits(t *testing.T, dir string, store func(*Store) storage.LimitStore) (*Store, *limits.Manager, error) {
	t.Helper()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Recover(context.Background(), time.Now()); err != nil {
		s.Close()
		t.Fatal(err)
	}
	var target storage.LimitStore = s
	if store != nil {
		target = store(s)
	}
	m := limits.New(limits.Options{Store: target})
	return s, m, m.Recover(context.Background(), nil)
}

func checkRestart(t *testing.T, dir string, wantSpent int64, wantCost config.Microusd) {
	t.Helper()
	for range 2 {
		s, m, err := restartLimits(t, dir, nil)
		if err != nil {
			s.Close()
			t.Fatal(err)
		}
		spent, reserved := budget(t, m)
		rows, err := s.Daily(context.Background(), storage.Filter{AppName: "bot"})
		s.Close()
		if spent != wantSpent || reserved != 0 {
			t.Fatalf("budget after restart: spent=%d reserved=%d", spent, reserved)
		}
		if err != nil || len(rows) != 1 || rows[0].Cost != wantCost {
			t.Fatalf("usage after restart: %+v %v", rows, err)
		}
	}
}

func TestCrashBeforeSnapshotPreservesReservation(t *testing.T) {
	checkRestart(t, crashAt(t, "limits-admitted"), 100, 100)
	checkRestart(t, crashAt(t, "limits-checkpointed"), 100, 100)
}

func TestCrashWithCompletedJournalUsesExactSettlement(t *testing.T) {
	checkRestart(t, crashAt(t, "limits-journal-result"), 49, 49)
}

func TestCrashAfterBudgetSettlementBeforeUsageFlush(t *testing.T) {
	checkRestart(t, crashAt(t, "limits-settled"), 49, 49)
}

func TestCrashDuringRecoveryIsIdempotent(t *testing.T) {
	dir := crashAt(t, "limits-admitted-many")
	s, _, err := restartLimits(t, dir, func(s *Store) storage.LimitStore { return &failingLimits{Store: s, failAfter: 1} })
	s.Close()
	if err == nil {
		t.Fatal("interrupted recovery reported success")
	}
	s, _, err = restartLimits(t, dir, func(s *Store) storage.LimitStore { return &failingLimits{Store: s, saveFailures: 1} })
	s.Close()
	if err == nil {
		t.Fatal("recovery without its checkpoint reported success")
	}
	checkRestart(t, dir, 300, 300)
}
