package cli

import (
	"fmt"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/spf13/cobra"
	"time"
)

func newPricingCommand(out streams) *cobra.Command {
	root := &cobra.Command{Use: "pricing", Short: "Inspect embedded prices"}
	root.AddCommand(&cobra.Command{Use: "check", Args: cobra.NoArgs, RunE: func(*cobra.Command, []string) error {
		catalog, err := pricing.Embedded()
		if err != nil {
			return err
		}
		now := time.Now()
		old, err := catalog.Check(now)
		if err != nil {
			return err
		}
		fmt.Fprintf(out.stdout, "Price catalog: %s (%d days old). Prices are estimated cost.\n", catalog.VerifiedAt().Format("2006-01-02"), int64(now.Sub(catalog.VerifiedAt()).Hours()/24))
		if old {
			fmt.Fprintln(out.stderr, "warning: price catalog is older than 60 days; update the binary or configure YAML price overrides")
		}
		return nil
	}})
	return root
}
