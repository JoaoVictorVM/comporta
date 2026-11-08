package pricing

import (
	_ "embed"
	"encoding/json"
	"errors"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"math"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

//go:embed catalog.json
var catalogData []byte

var ErrUnknownPrice = errors.New("no price defined for model")

type entry struct {
	Format           config.APIFormat `json:"format"`
	Price            config.Price     `json:"price"`
	CacheWrite1h     config.Microusd  `json:"cache_write_1h"`
	Threshold        int64            `json:"threshold"`
	HighPrice        config.Price     `json:"high_price"`
	HighCacheWrite1h config.Microusd  `json:"high_cache_write_1h"`
}

type Catalog struct {
	verified time.Time
	models   map[string]entry
}

func Embedded() (*Catalog, error) {
	var raw struct {
		Verified time.Time        `json:"verified_at"`
		Models   map[string]entry `json:"models"`
	}
	if err := json.Unmarshal(catalogData, &raw); err != nil {
		return nil, errors.New("invalid price catalog")
	}
	if raw.Verified.IsZero() || len(raw.Models) == 0 {
		return nil, errors.New("invalid price catalog")
	}
	for _, e := range raw.Models {
		if _, err := Cost(Tokens{}, e.Price); err != nil {
			return nil, err
		}
		if _, err := Cost(Tokens{}, e.HighPrice); err != nil {
			return nil, err
		}
		if e.Threshold < 0 || e.CacheWrite1h < 0 || e.HighCacheWrite1h < 0 {
			return nil, ErrInvalidUsage
		}
	}
	return &Catalog{verified: raw.Verified, models: raw.Models}, nil
}

func (c *Catalog) HasPrice(model string) bool { _, ok := c.models[model]; return ok }

func (c *Catalog) HasProviderPrice(model string, provider config.Provider) bool {
	if e, ok := c.models[model]; ok && e.Format == provider.APIFormat {
		return true
	}
	return provider.Local && provider.APIFormat == config.FormatOpenAI
}
func (c *Catalog) VerifiedAt() time.Time { return c.verified }
func (c *Catalog) Check(now time.Time) (bool, error) {
	if now.Before(c.verified) {
		return false, errors.New("price catalog date is in the future")
	}
	return now.Sub(c.verified) > 60*24*time.Hour, nil
}

type Quote struct {
	price       config.Price
	write1h     config.Microusd
	high        config.Price
	highWrite1h config.Microusd
	threshold   int64
	source      string
	version     time.Time
}

func (q Quote) Frozen() storage.Quote {
	return storage.Quote{EstimateMethod: "bytes/3 rounded up plus 1600 per image; fixture-validated heuristic", Price: q.price, HighPrice: q.high, Write1h: q.write1h, HighWrite1h: q.highWrite1h, Threshold: q.threshold, Source: q.source, Version: q.version}
}

func (q Quote) CacheWriteEstimated(total int64, five, hour *int64) bool {
	return total > 0 && (five == nil || hour == nil) && (q.write1h != q.price.CacheWrite || (q.threshold > 0 && q.highWrite1h != q.high.CacheWrite))
}

func (c *Catalog) Resolve(snapshot *config.Snapshot, provider config.Provider, model string) (Quote, error) {
	if p, ok := snapshot.Price(model); ok {
		return Quote{price: p, write1h: p.CacheWrite, source: "override"}, nil
	}
	if e, ok := c.models[model]; ok && e.Format == provider.APIFormat {
		return Quote{price: e.Price, write1h: e.CacheWrite1h, high: e.HighPrice, highWrite1h: e.HighCacheWrite1h, threshold: e.Threshold, source: "embedded", version: c.verified}, nil
	}
	if provider.Local && provider.APIFormat == config.FormatOpenAI {
		return Quote{source: "local"}, nil
	}
	return Quote{}, ErrUnknownPrice
}

func (q Quote) Price(inputTotal int64) config.Price {
	if q.threshold > 0 && inputTotal > q.threshold {
		return q.high
	}
	return q.price
}

func (q Quote) Cost(t Tokens, write5m, write1h *int64) (config.Microusd, error) {
	if t.Input < 0 || t.CacheWrite < 0 || t.CacheRead < 0 || t.Input > math.MaxInt64-t.CacheWrite || t.Input+t.CacheWrite > math.MaxInt64-t.CacheRead {
		return 0, ErrOverflow
	}
	total := t.Input + t.CacheWrite + t.CacheRead
	p := q.Price(total)
	hour := q.write1h
	if q.threshold > 0 && total > q.threshold {
		hour = q.highWrite1h
	}
	if write5m == nil || write1h == nil {
		p.CacheWrite = max(p.CacheWrite, hour)
		return Cost(t, p)
	}
	if *write5m < 0 || *write1h < 0 || *write5m > t.CacheWrite || *write1h != t.CacheWrite-*write5m {
		return 0, ErrInvalidUsage
	}
	return costTerms([]int64{t.Input, t.Output, *write5m, *write1h, t.CacheRead}, []config.Microusd{p.Input, p.Output, p.CacheWrite, hour, p.CacheRead})
}

func (q Quote) ConservativePrice() config.Price {
	p := q.price
	input := max(p.Input, p.CacheWrite, p.CacheRead, q.write1h)
	output := p.Output
	if q.threshold > 0 {
		input = max(input, q.high.Input, q.high.CacheWrite, q.high.CacheRead, q.highWrite1h)
		output = max(output, q.high.Output)
	}
	return config.Price{Input: input, Output: output, CacheWrite: input, CacheRead: input}
}

func (q Quote) Estimate(original, effective, images, output int64) (config.Microusd, error) {
	input, err := EstimateInput(original, effective, images)
	if err != nil {
		return 0, err
	}
	return Cost(Tokens{Input: input, Output: output}, q.ConservativePrice())
}
