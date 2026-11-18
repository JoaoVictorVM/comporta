package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/JoaoVictorVM/comporta/internal/distributed"
	"github.com/JoaoVictorVM/comporta/internal/storage/postgres"
)

func newStorageRecoverCommand(out streams) *cobra.Command {
	return &cobra.Command{
		Use:   "recover",
		Short: "Rebuild the shared limit state in Redis from the Postgres ledger",
		Long: "Hold the cluster's exclusive gate, rebuild budgets, reservations, rate windows, slots and pauses in a new " +
			"epoch and publish it. Admissions wait or get 503 meanwhile. Nothing is deleted from Postgres, no balance is " +
			"reset, and loop guard history, which only Redis had, is replaced by a quarantine.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			candidate, err := loadResolved(command, out)
			if err != nil {
				return err
			}
			store, err := requireShared(command.Context(), candidate)
			if err != nil {
				return reportShared(out, err)
			}
			defer store.Close()
			if err := store.RequireCurrent(command.Context()); err != nil {
				return reportShared(out, errors.New("shared database is not migrated; run \"comporta storage migrate\""))
			}
			redis, err := openRedis(command.Context(), candidate)
			if err != nil {
				return reportShared(out, err)
			}
			defer redis.Close()
			cluster := candidate.Deployment().ClusterID
			state, err := postgres.LoadCluster(command.Context(), store.DB(), cluster)
			if err != nil {
				return reportShared(out, errors.New("cluster not bootstrapped; start a replica first"))
			}
			coordinator := distributed.New(distributed.Options{Cluster: cluster, SQL: store, Redis: redis, Publisher: store})
			coordinator.UseEpoch(state.Epoch, state.Mode)
			if err := coordinator.Rebuild(command.Context(), maxLoopWindow(candidate)); err != nil {
				return reportShared(out, errors.New("rebuild did not complete; the previous state is kept, run the command again"))
			}
			_, _ = fmt.Fprintln(out.stdout, "Shared limit state rebuilt in a new epoch.")
			return nil
		},
	}
}
