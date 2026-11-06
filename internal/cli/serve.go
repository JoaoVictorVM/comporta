package cli

import (
	"errors"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

func newServeCommand(out streams) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Load the configuration and keep it active until interrupted",
		Long: "Load the configuration, resolve its secrets and apply saved changes without " +
			"a restart. This build opens no network listeners yet.",
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
			logger.Info("configuration active; network listeners are not available in this build",
				"generation", snapshot.Generation(), "apps", len(snapshot.AppNames()), "providers", len(snapshot.ProviderNames()))
			if err := manager.Run(command.Context()); err != nil {
				logger.Error("configuration watcher stopped unexpectedly")
				return errReported
			}
			logger.Info("shutdown complete")
			return nil
		},
	}
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
