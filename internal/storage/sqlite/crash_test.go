package sqlite

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

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
