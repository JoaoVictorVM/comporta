package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/JoaoVictorVM/comporta/internal/admin"
)

var (
	sinceFlag = regexp.MustCompile(`^[1-9][0-9]*d$`)
	dayFlag   = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

type usageFlags struct {
	app, user, since, from, through string
	anonymous, json                 bool
}

// validate checks the period before any call, so an operator mistake never
// reaches the gateway.
func (f usageFlags) validate(sinceSet bool) error {
	if f.user != "" && f.anonymous {
		return errors.New("--user and --anonymous are mutually exclusive")
	}
	if (f.from == "") != (f.through == "") {
		return errors.New("--from and --through go together")
	}
	if f.from != "" {
		if sinceSet {
			return errors.New("--since cannot be combined with --from and --through")
		}
		from, err1 := time.Parse(time.DateOnly, f.from)
		through, err2 := time.Parse(time.DateOnly, f.through)
		if !dayFlag.MatchString(f.from) || !dayFlag.MatchString(f.through) || err1 != nil || err2 != nil {
			return errors.New("--from and --through must be dates as YYYY-MM-DD")
		}
		if through.Before(from) || int(through.Sub(from).Hours()/24)+1 > admin.MaxUsageDays {
			return errors.New("the period must run forward and cover at most 366 days")
		}
		return nil
	}
	if !sinceFlag.MatchString(f.since) {
		return errors.New("--since must be a positive number of days, such as 7d")
	}
	days, err := strconv.Atoi(strings.TrimSuffix(f.since, "d"))
	if err != nil || days < 1 || days > admin.MaxUsageDays {
		return errors.New("--since covers at most 366 days")
	}
	return nil
}

func (f usageFlags) request() map[string]any {
	body := map[string]any{}
	if f.app != "" {
		body["app"] = f.app
	}
	if f.user != "" {
		body["user_id"] = f.user
	}
	if f.anonymous {
		body["anonymous"] = true
	}
	if f.from != "" {
		body["from"], body["through"] = f.from, f.through
	} else {
		body["since"] = f.since
	}
	body["limit"] = 1000
	return body
}

func newUsageCommand(out streams) *cobra.Command {
	var f usageFlags
	var c connection
	command := &cobra.Command{
		Use:   "usage",
		Short: "Query estimated usage by app, user and period from the running gateway",
		Long: "Query aggregated usage through the admin API of the running gateway, which needs a token. " +
			"Without --user or --anonymous it shows app totals; with them, only that user's usage. " +
			"Days are the civil days usage was recorded under; the period covers at most 366 days.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if err := f.validate(command.Flags().Changed("since")); err != nil {
				return err
			}
			client, _, err := c.client(command)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(command.Context(), clientTimeout)
			defer cancel()
			body := f.request()
			var combined admin.UsagePage
			for first := true; ; first = false {
				var page admin.UsagePage
				if err := client.Do(ctx, http.MethodPost, admin.Prefix+"usage/query", body, &page); err != nil {
					return callFailure(client, err)
				}
				if first {
					combined.ReferenceAt, combined.MaterializationDelaySeconds = page.ReferenceAt, page.MaterializationDelaySeconds
					combined.Rows = []admin.UsageRow{}
				}
				combined.Rows = append(combined.Rows, page.Rows...)
				combined.Totals = page.Totals
				if page.NextCursor == nil {
					break
				}
				body["cursor"] = *page.NextCursor
			}
			if f.json {
				data, _ := json.MarshalIndent(combined, "", "  ")
				fmt.Fprintln(out.stdout, string(data))
				return nil
			}
			writeUsage(out, combined, f.app == "")
			return nil
		},
	}
	command.Flags().StringVar(&f.app, "app", "", "application name (default: every app)")
	command.Flags().StringVar(&f.since, "since", "7d", "calendar days including today, at most 366d")
	command.Flags().StringVar(&f.user, "user", "", "raw user ID, pseudonymized per app and never stored")
	command.Flags().BoolVar(&f.anonymous, "anonymous", false, "show the anonymous bucket")
	command.Flags().StringVar(&f.from, "from", "", "first day, YYYY-MM-DD, with --through")
	command.Flags().StringVar(&f.through, "through", "", "last day, YYYY-MM-DD, inclusive")
	command.Flags().BoolVar(&f.json, "json", false, "print the aggregated JSON")
	addConnectionFlags(command, &c)
	return command
}

func usd(micros string) string {
	var value, dollars, fraction big.Int
	if _, ok := value.SetString(micros, 10); !ok {
		return micros
	}
	dollars.QuoRem(&value, big.NewInt(1_000_000), &fraction)
	return fmt.Sprintf("%s.%06d", dollars.String(), fraction.Int64())
}

func writeUsage(out streams, page admin.UsagePage, withApp bool) {
	fmt.Fprintf(out.stdout, "Reference: %s; pending completions may take %d seconds.\n", page.ReferenceAt, page.MaterializationDelaySeconds)
	prefix := ""
	if withApp {
		prefix = "app "
	}
	fmt.Fprintf(out.stdout, "%sday timezone requests tokens estimated_cost_usd\n", prefix)
	for _, row := range page.Rows {
		app := ""
		if withApp {
			app = row.App + " "
		}
		fmt.Fprintf(out.stdout, "%s%s %s %s %s %s\n", app, row.Day, row.Timezone, row.Requests, row.Tokens, usd(row.CostMicrousd))
	}
	dash := "-"
	if withApp {
		dash = "- -"
	}
	fmt.Fprintf(out.stdout, "total %s %s %s %s\n", dash, page.Totals.Requests, page.Totals.Tokens, usd(page.Totals.CostMicrousd))
}
