package pricing

import (
	"errors"
	"math/big"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

var ErrOverflow = errors.New("request cost exceeds supported range")
var ErrInvalidUsage = errors.New("invalid usage or price")

type Tokens struct {
	Input, Output, CacheWrite, CacheRead int64
}

// Cost rounds the exact sum once, so fractional dimensions cannot inflate a bill.
func Cost(tokens Tokens, price config.Price) (config.Microusd, error) {
	counts := []int64{tokens.Input, tokens.Output, tokens.CacheWrite, tokens.CacheRead}
	rates := []config.Microusd{price.Input, price.Output, price.CacheWrite, price.CacheRead}
	var sum, product big.Int
	for i, count := range counts {
		if count < 0 || rates[i] < 0 {
			return 0, ErrInvalidUsage
		}
		product.Mul(big.NewInt(count), big.NewInt(int64(rates[i])))
		sum.Add(&sum, &product)
	}
	sum.Add(&sum, big.NewInt(999999))
	sum.Quo(&sum, big.NewInt(1000000))
	if !sum.IsInt64() {
		return 0, ErrOverflow
	}
	return config.Microusd(sum.Int64()), nil
}
