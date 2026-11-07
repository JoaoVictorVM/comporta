package pricing

import "math"

func EstimateInput(originalBytes, effectiveBytes, images int64) (int64, error) {
	if originalBytes < 0 || effectiveBytes < 0 || images < 0 {
		return 0, ErrInvalidUsage
	}
	bytes := max(originalBytes, effectiveBytes)
	base := bytes / 3
	if bytes%3 != 0 {
		base++
	}
	if images > (math.MaxInt64-base)/1600 {
		return 0, ErrOverflow
	}
	return base + images*1600, nil
}
