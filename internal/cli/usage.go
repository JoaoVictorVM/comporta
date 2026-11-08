package cli

import (
	"errors"
	"fmt"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/spf13/cobra"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func newUsageCommand(out streams) *cobra.Command {
	var app, since string
	c := &cobra.Command{Use: "usage", Short: "Query local estimated usage", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if app == "" || !regexp.MustCompile(`^[1-9][0-9]*d$`).MatchString(since) {
			return errors.New("usage requires --app and a positive --since Nd")
		}
		days, err := strconv.ParseUint(strings.TrimSuffix(since, "d"), 10, 64)
		if err != nil || days < 1 {
			return errors.New("since must be a positive number of days")
		}
		path, err := c.Flags().GetString("config")
		if err != nil {
			return err
		}
		catalog, err := pricing.Embedded()
		if err != nil {
			return err
		}
		candidate, err := config.Load(path, config.Options{SystemTimezone: config.DetectSystemTimezone(), Catalog: catalog})
		if err != nil {
			return err
		}
		reader, err := sqlite.OpenReader(c.Context(), candidate.DataDir())
		if err != nil {
			return err
		}
		defer reader.Close()
		rows, err := reader.Daily(c.Context(), storage.Filter{AppName: app})
		if err != nil {
			return err
		}
		now := time.Now()
		var totalRequests, totalTokens, totalCost big.Int
		fmt.Fprintf(out.stdout, "Reference: %s; pending completions may take 2 seconds.\nday timezone requests tokens estimated_cost_usd\n", now.UTC().Format(time.RFC3339))
		for _, row := range rows {
			zone := row.Timezone
			if active, ok := candidate.App(app); ok {
				zone = active.Timezone
			}
			loc, err := time.LoadLocation(zone)
			if err != nil {
				return errors.New("invalid historical timezone")
			}
			start := ""
			if days < 3650000 {
				start = now.In(loc).AddDate(0, 0, -int(days)+1).Format("2006-01-02")
			}
			end := now.In(loc).Format("2006-01-02")
			if row.Day < start || row.Day > end {
				continue
			}
			var tokens big.Int
			for _, n := range []int64{row.Input, row.Output, row.CacheWrite, row.CacheRead} {
				tokens.Add(&tokens, big.NewInt(n))
			}
			fmt.Fprintf(out.stdout, "%s %s %d %s %s\n", row.Day, row.Timezone, row.Requests, tokens.String(), row.Cost.String())
			totalRequests.Add(&totalRequests, big.NewInt(row.Requests))
			totalTokens.Add(&totalTokens, &tokens)
			totalCost.Add(&totalCost, big.NewInt(int64(row.Cost)))
		}
		var dollars, fraction big.Int
		dollars.QuoRem(&totalCost, big.NewInt(1000000), &fraction)
		fmt.Fprintf(out.stdout, "total - %s %s %s.%06d\n", totalRequests.String(), totalTokens.String(), dollars.String(), fraction.Int64())
		return nil
	}}
	c.Flags().StringVar(&app, "app", "", "application name")
	c.Flags().StringVar(&since, "since", "7d", "calendar days including today")
	return c
}
