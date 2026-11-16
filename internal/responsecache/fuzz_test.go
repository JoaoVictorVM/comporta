package responsecache

import (
	"testing"
	"time"
)

// FuzzCacheOperations runs arbitrary sequences of captures, lookups, leases,
// purges, reloads and expiry, and checks the accounting stays exact.
func FuzzCacheOperations(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, int64(4000))
	f.Add([]byte{0, 0, 0, 2, 2, 5, 1, 1, 6, 3}, int64(900))
	f.Fuzz(func(t *testing.T, ops []byte, max int64) {
		if max <= 0 || max > 1_000_000 {
			max = 4000
		}
		clk := &clock{t: time.Unix(1_700_000_000, 0)}
		c := New(Options{MaxBytes: max, Now: clk.now})
		generation := int64(1)
		c.Sync(Settings{Generation: generation, MaxBytes: max})
		var leases []*Lease
		var builders []*Builder
		for i, op := range ops {
			k := key([]string{"a", "b"}[i%2], string([]byte{op % 7}))
			switch op % 9 {
			case 0:
				if b := c.Begin(k, generation, Kind(op%2+1), "t"); b != nil {
					builders = append(builders, b)
				}
			case 1:
				for _, b := range builders {
					b.Append(make([]byte, int(op)*3))
				}
			case 2:
				for _, b := range builders {
					b.Publish(time.Duration(op%5) * time.Second)
				}
				builders = nil
			case 3:
				if l, ok := c.Lookup(k, generation); ok {
					leases = append(leases, l)
				}
			case 4:
				for _, l := range leases {
					l.Release()
				}
				leases = nil
			case 5:
				c.PurgeApps([]string{"a"})
			case 6:
				generation++
				c.Sync(Settings{Generation: generation, MaxBytes: max / 2})
			case 7:
				clk.t = clk.t.Add(time.Second)
				c.Sweep()
			case 8:
				for _, b := range builders {
					b.Abort()
				}
				builders = nil
			}
			if s := c.Stats(); s.UsedBytes < 0 || s.Leases < 0 || s.Builders < 0 {
				t.Fatalf("negative counters: %+v", s)
			}
		}
		for _, b := range builders {
			b.Abort()
		}
		for _, l := range leases {
			l.Release()
		}
		c.Close()
		if s := c.Stats(); s.UsedBytes != 0 || s.Leases != 0 || s.Builders != 0 || s.Entries != 0 {
			t.Fatalf("leftover accounting after everything ended: %+v", s)
		}
	})
}
