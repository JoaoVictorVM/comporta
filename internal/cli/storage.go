package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/JoaoVictorVM/comporta/internal/storage/postgres"
)

var errSharedUnavailable = errors.New("shared database unavailable; check storage.postgres and its secret")

// loadResolved loads the configuration with its secrets, which commands that
// reach the shared database need. Diagnostics never print secret values.
func loadResolved(command *cobra.Command, out streams) (*config.Candidate, error) {
	path, err := command.Flags().GetString("config")
	if err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("configuration path cannot be resolved")
	}
	env, err := config.CaptureEnvironment(filepath.Dir(absolute), os.Environ(), nil)
	if err != nil {
		printDiagnostics(out.stderr, err)
		return nil, errReported
	}
	catalog, err := pricing.Embedded()
	if err != nil {
		return nil, err
	}
	candidate, err := config.Load(absolute, config.Options{SystemTimezone: config.DetectSystemTimezone(), Catalog: catalog, ResolveSecrets: true, Environment: env})
	if err != nil {
		printDiagnostics(out.stderr, err)
		return nil, errReported
	}
	return candidate, nil
}

// openShared connects to the shared database of a distributed configuration.
func openShared(ctx context.Context, candidate *config.Candidate) (*postgres.Store, error) {
	dsn, ok := candidate.StorageSecret("storage.postgres.dsn")
	if !ok {
		return nil, errSharedUnavailable
	}
	store, err := postgres.Open(ctx, postgres.Options{DSN: dsn.Reveal(), MaxConnections: int(min(candidate.Storage().Postgres.MaxConnections, 4))})
	if err != nil {
		return nil, errSharedUnavailable
	}
	return store, nil
}

func newStorageCommand(out streams) *cobra.Command {
	group := &cobra.Command{Use: "storage", Short: "Operate the shared storage of a distributed deployment"}
	migrate := &cobra.Command{
		Use:   "migrate",
		Short: "Apply the pending migrations of the shared database",
		Long: "Apply the Postgres migrations once, under a lock shared by every replica. Run it as a separate job " +
			"to keep schema privileges away from the runtime user; serve also applies them at start.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			store, err := sharedFromCommand(command, out)
			if err != nil {
				return err
			}
			defer store.Close()
			if err := store.Migrate(command.Context(), 0); err != nil {
				_, _ = fmt.Fprintln(out.stderr, err.Error())
				return errReported
			}
			version, _ := store.SchemaVersion(command.Context())
			_, _ = fmt.Fprintf(out.stdout, "Shared database at schema version %d.\n", version)
			return nil
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show the schema version and the cluster state, without secrets",
		Args:  cobra.NoArgs,
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
			version, err := store.SchemaVersion(command.Context())
			if err != nil {
				_, _ = fmt.Fprintln(out.stderr, err.Error())
				return errReported
			}
			_, _ = fmt.Fprintf(out.stdout, "schema_version: %d\n", version)
			state, err := store.ClusterStatus(command.Context(), candidate.Deployment().ClusterID)
			if err != nil {
				_, _ = fmt.Fprintln(out.stdout, "cluster: not bootstrapped")
				return nil
			}
			_, _ = fmt.Fprintf(out.stdout, "cluster: %s\nmode: %s\nconfig_revision: %d\nactive_replicas: %d\npending_outbox: %d\n",
				candidate.Deployment().ClusterID, state.Mode, state.ConfigRevision, state.ActiveReplicas, state.PendingOutbox)
			return nil
		},
	}
	group.AddCommand(migrate, status)
	return group
}

func sharedFromCommand(command *cobra.Command, out streams) (*postgres.Store, error) {
	candidate, err := loadResolved(command, out)
	if err != nil {
		return nil, err
	}
	store, err := requireShared(command.Context(), candidate)
	if err != nil {
		return nil, reportShared(out, err)
	}
	return store, nil
}

var errNotDistributed = errors.New("this command applies to deployment.mode distributed only")

func requireShared(ctx context.Context, candidate *config.Candidate) (*postgres.Store, error) {
	if !candidate.Deployment().Distributed() {
		return nil, errNotDistributed
	}
	return openShared(ctx, candidate)
}

func reportShared(out streams, err error) error {
	_, _ = fmt.Fprintln(out.stderr, err.Error())
	return errReported
}
