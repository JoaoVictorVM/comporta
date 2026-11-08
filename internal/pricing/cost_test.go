package pricing

import (
	"errors"
	"math"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

func TestExactFourDimensionCost(t *testing.T) {
	got, err := Cost(Tokens{10, 3, 4, 5}, config.Price{Input: 2000000, Output: 6000000, CacheWrite: 2500000, CacheRead: 200000})
	if err != nil || got != 49 {
		t.Fatalf("cost=%v error=%v", got, err)
	}
}

func TestRoundUpOnceAcrossDimensions(t *testing.T) {
	got, err := Cost(Tokens{1, 1, 1, 1}, config.Price{Input: 1, Output: 1, CacheWrite: 1, CacheRead: 1})
	if err != nil || got != 1 {
		t.Fatalf("cost=%v error=%v", got, err)
	}
}

func TestCostOverflow(t *testing.T) {
	_, err := Cost(Tokens{Input: math.MaxInt64}, config.Price{Input: math.MaxInt64})
	if !errors.Is(err, ErrOverflow) {
		t.Fatal(err)
	}
	_, err = Cost(Tokens{Input: -1}, config.Price{})
	if !errors.Is(err, ErrInvalidUsage) {
		t.Fatal(err)
	}
}

func TestZeroPriceKnownZero(t *testing.T) {
	got, err := Cost(Tokens{Input: math.MaxInt64}, config.Price{})
	if got != 0 || err != nil {
		t.Fatalf("cost=%v error=%v", got, err)
	}
}

func TestInputEstimateCeiling(t *testing.T) {
	for _, tc := range []struct{ original, effective, images, want int64 }{{0, 0, 0, 0}, {1, 0, 0, 1}, {3, 4, 0, 2}, {3, 3, 2, 3201}, {math.MaxInt64, 0, 0, 3074457345618258603}} {
		got, err := EstimateInput(tc.original, tc.effective, tc.images)
		if got != tc.want || err != nil {
			t.Fatalf("%+v: %d %v", tc, got, err)
		}
	}
	if _, err := EstimateInput(1, 1, math.MaxInt64); !errors.Is(err, ErrOverflow) {
		t.Fatal(err)
	}
}
