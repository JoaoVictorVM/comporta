package cli

import (
	"context"
	"errors"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/keys"
)

func newServeCommand(out streams) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Load the configuration and app keys and keep them active until interrupted",
		Long: "Load the configuration and app keys, resolve secrets and apply saved changes " +
			"without a restart. Create a key first with \"comporta keys create <app>\". This " +
			"build opens no network listeners yet.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			path, err := command.Flags().GetString("config")
			if err != nil {
				return err
			}
			logger := slog.New(slog.NewJSONHandler(out.stderr, nil))
			manager, err := config.NewManager(config.ManagerOptions{Path: path, Logger: logger})
			if err != nil {
				logStartupFailure(logger, err)
				return errReported
			}
			snapshot := manager.Capture()
			keyManager, err := keys.NewManager(keys.ManagerOptions{Dir: snapshot.DataDir(), Logger: logger})
			if err != nil {
				logger.Error(err.Error())
				return errReported
			}
			logger.Info("configuration active; network listeners are not available in this build",
				"generation", snapshot.Generation(), "apps", len(snapshot.AppNames()), "providers", len(snapshot.ProviderNames()))
			if err := runManagers(command.Context(), logger, manager, keyManager); err != nil {
				return err
			}
			logger.Info("shutdown complete")
			return nil
		},
	}
}

// runManagers stops both when either ends, so the service never keeps running with
// half of its state frozen.
func runManagers(ctx context.Context, logger *slog.Logger, configuration *config.Manager, keyStore *keys.Manager) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan error, 2)
	go func() { results <- configuration.Run(ctx) }()
	go func() { results <- keyStore.Run(ctx) }()

	first := <-results
	unexpected := ctx.Err() == nil
	cancel()
	second := <-results
	if unexpected || first != nil || second != nil {
		logger.Error("a background watcher stopped unexpectedly")
		return errReported
	}
	return nil
}

func logStartupFailure(logger *slog.Logger, err error) {
	var failure *config.Error
	if !errors.As(err, &failure) {
		logger.Error("configuration could not be loaded")
		return
	}
	for _, diagnostic := range failure.Diagnostics {
		logger.Error(diagnostic.String(), "code", diagnostic.Code)
	}
}
