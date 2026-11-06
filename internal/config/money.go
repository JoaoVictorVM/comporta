package config

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// Microusd is an amount of US dollars in millionths. Values come straight from
// the decimal lexeme so no binary float ever touches money.
type Microusd int64

const microsPerUSD = 1_000_000

var moneyPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)(\.[0-9]{1,6})?$`)

var (
	errMoneyFormat   = errors.New("must be a plain decimal with at most 6 decimal places")
	errMoneyOverflow = errors.New("exceeds the supported monetary range")
)

// ParseMoney converts a decimal lexeme such as "2.40" into microdollars.
func ParseMoney(lexeme string) (Microusd, error) {
	if !moneyPattern.MatchString(lexeme) {
		return 0, errMoneyFormat
	}
	whole, fraction, _ := strings.Cut(lexeme, ".")
	dollars, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, errMoneyOverflow
	}
	micros, err := strconv.ParseInt(fraction+strings.Repeat("0", 6-len(fraction)), 10, 64)
	if err != nil {
		return 0, errMoneyFormat
	}
	if dollars > (math.MaxInt64-micros)/microsPerUSD {
		return 0, errMoneyOverflow
	}
	return Microusd(dollars*microsPerUSD + micros), nil
}

func (m Microusd) String() string {
	return fmt.Sprintf("%d.%06d", int64(m)/microsPerUSD, int64(m)%microsPerUSD)
}

// downgradeHardCap derives the default ceiling of 120% of a budget, rounding up
// so the ceiling never ends below the documented percentage.
func downgradeHardCap(budget int64) (int64, bool) {
	if budget > (math.MaxInt64-99)/120 {
		return 0, false
	}
	return (budget*120 + 99) / 100, true
}
