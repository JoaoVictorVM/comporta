package limits

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// FormatUSD renders microdollars with cents, keeping extra digits only when
// they carry value, so integer amounts never pass through a float.
func FormatUSD(micros int64) string {
	sign := ""
	if micros < 0 {
		sign, micros = "-", -micros
	}
	whole, fraction := micros/1_000_000, micros%1_000_000
	if fraction%10_000 == 0 {
		return fmt.Sprintf("%s$%d.%02d", sign, whole, fraction/10_000)
	}
	return sign + "$" + strconv.FormatInt(whole, 10) + "." + strings.TrimRight(fmt.Sprintf("%06d", fraction), "0")
}

// FormatAmount renders a value in the unit of a dimension.
func FormatAmount(d Dimension, value int64) string {
	if d.money() {
		return FormatUSD(value)
	}
	return strconv.FormatInt(value, 10)
}

func formatWait(d time.Duration) string {
	minutes := int64((d + time.Minute - 1) / time.Minute)
	if minutes < 1 {
		minutes = 1
	}
	days, hours := minutes/(24*60), (minutes/60)%24
	minutes %= 60
	if days > 0 {
		return fmt.Sprintf("%dd%dh%dm", days, hours, minutes)
	}
	return fmt.Sprintf("%dh%dm", hours, minutes)
}

func whose(v Violation, app string) string {
	if v.Scope == ScopeUser {
		return "this user"
	}
	return "app " + app
}

// Message is the client-facing text of a violation. It names the app, never
// the user, and states the renewal in the effective timezone of the app.
func Message(v Violation, app string, now time.Time) string {
	switch v.Kind {
	case KindConcurrency:
		return fmt.Sprintf("too many concurrent requests for %s (limit %d)", whose(v, app), v.Limit)
	case KindRate:
		what := "request rate limit of %d per minute"
		if v.Dimension == TokensPerMinute {
			what = "token rate limit of %d tokens per minute"
		}
		message := fmt.Sprintf(what+" for %s exceeded", v.Limit, whose(v, app))
		if v.Dimension == TokensPerMinute && v.Reason == ReasonReservation {
			message += " by tokens of requests still in flight"
		}
		if v.RetryAfter > 0 {
			message += fmt.Sprintf("; retry in %ds", v.RetryAfter)
		}
		return message
	}
	period := "daily"
	if v.Dimension.monthly() {
		period = "monthly"
	}
	noun := "budget"
	if !v.Dimension.money() {
		noun = "token budget"
	}
	if v.hardCap {
		noun = "hard cap"
		if !v.Dimension.money() {
			noun = "token hard cap"
		}
	}
	unit := ""
	if !v.Dimension.money() {
		unit = " tokens"
	}
	message := fmt.Sprintf("%s %s of %s%s for %s would be exceeded", period, noun, FormatAmount(v.Dimension, v.Limit), unit, whose(v, app))
	if v.Reason == ReasonReservation {
		message += " by pending reservations"
	}
	message += fmt.Sprintf(" (spent %s, reserved %s)", FormatAmount(v.Dimension, v.Used), FormatAmount(v.Dimension, v.Reserved))
	if v.TooLarge {
		message += "; the request alone exceeds this limit"
	}
	if v.RenewsAt != nil {
		location, err := loadLocation(v.Timezone)
		if err != nil {
			location = time.UTC
		}
		local := v.RenewsAt.In(location)
		when := local.Format("15:04")
		if v.Dimension.monthly() {
			when = local.Format("2006-01-02 15:04")
		}
		message += fmt.Sprintf("; renews at %s %s (in %s)", when, v.Timezone, formatWait(v.RenewsAt.Sub(now)))
	}
	return message
}
