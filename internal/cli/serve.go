package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/JoaoVictorVM/comporta/internal/admin"
	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/keys"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/metering"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/runtimecontrol"
	"github.com/JoaoVictorVM/comporta/internal/server"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

func newServeCommand(out streams) *cobra.Command {
	var resetHistory bool
	command := &cobra.Command{
		Use:   "serve",
		Short: "Run the gateway: the proxy and admin listeners, until interrupted",
		Long: "Load the configuration and app keys, open the proxy and admin listeners and apply " +
			"saved changes without a restart. Create a key first with \"comporta keys create <app>\".",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			path, err := command.Flags().GetString("config")
			if err != nil {
				return err
			}
			logger := slog.New(slog.NewJSONHandler(out.stderr, nil))
			catalog, err := pricing.Embedded()
			if err != nil {
				return err
			}
			configEvents := &metering.ConfigEvents{Logger: logger}
			manager, err := config.NewManager(config.ManagerOptions{Path: path, Logger: logger, Options: config.Options{Catalog: catalog}, Sink: configEvents})
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
			// Before the listeners: without a key no request can be pseudonymized, and
			// this failure does not depend on the financial fail mode.
			store, err := sqlite.Prepare(command.Context(), snapshot.DataDir())
			if err != nil {
				logger.Error(err.Error())
				return errReported
			}
			defer store.Close()
			identity, err := useridentity.Open(command.Context(), useridentity.Options{
				Dir: snapshot.DataDir(), History: store, ResetHistory: resetHistory, Logger: logger,
			})
			if err != nil {
				logger.Error(err.Error())
				return errReported
			}
			if err := store.Initialize(command.Context()); err != nil {
				return err
			}
			if err := store.Recover(command.Context(), time.Now()); err != nil {
				return err
			}
			configEvents.Publisher = store
			// The token lock comes after the instance lock, the same order a reset
			// uses, so neither can wait on the other.
			credential, err := admin.OpenCredential(command.Context(), snapshot.DataDir(), store, func(token string) {
				_, _ = fmt.Fprintf(out.stdout, "Admin token (shown only once; store it now): %s\n", token)
			})
			if err != nil {
				logger.Error(adminStartupMessage(err))
				return errReported
			}
			defer credential.Close()
			accounting := metering.New(store, catalog, logger, func() int64 { return manager.Capture().Storage().RetentionDays })
			// Limits recover after the usage journal and before the listeners, from
			// the checkpoint, the ledger and the reservations a crash left open.
			coordinator := limits.New(limits.Options{Store: store, Logger: logger, Publisher: store})
			if err := coordinator.Recover(command.Context(), manager.Capture()); err != nil {
				logger.Error("limit state recovery failed; listeners not opened")
				return errReported
			}
			if err := accounting.UseLimits(coordinator, store, manager); err != nil {
				return err
			}
			// Pauses load before the listeners: a paused app never takes a request
			// after a restart, and an unreadable state keeps the service down.
			control := runtimecontrol.New(store)
			if err := control.Load(command.Context()); err != nil {
				logger.Error("pause state could not be loaded; listeners not opened")
				return errReported
			}
			adminAPI := admin.NewHandler(admin.HandlerOptions{
				Credential: credential, Logger: logger,
				Service: admin.NewService(admin.ServiceOptions{
					Control: control, Source: manager, Identity: identity, History: store,
					Metering: accounting, Limits: coordinator, Logger: logger,
				}),
			})
			if err := runGateway(command.Context(), logger, manager, keyManager, identity, accounting, control, adminAPI, configEvents.Run, coordinator.Run); err != nil {
				return err
			}
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = store.Checkpoint(shutdown)
			cancel()
			if closeErr := store.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				logger.Error("usage shutdown incomplete; journal retained for recovery")
				return errReported
			}
			logger.Info("shutdown complete")
			return nil
		},
	}
	command.Flags().BoolVar(&resetHistory, "reset-user-history", false,
		"Recover from a lost hmac.key by unlinking user history while preserving app totals; never replaces an existing key")
	return command
}

func runGateway(ctx context.Context, logger *slog.Logger, manager *config.Manager, keyManager *keys.Manager, identity *useridentity.Service, accounting *metering.Service, gate proxy.Gate, adminAPI http.Handler, configWorker, limitsWorker server.Worker) error {
	guard := auth.NewGuard(nil, logger)
	authHTTP := auth.NewHTTP(keyManager, auth.NewAuthenticator(keyManager, nil), guard)
	handler := proxy.NewHandler(proxy.Options{Auth: authHTTP, Identity: identity, Logger: logger, Accounting: accounting, Gate: gate})
	defer handler.Close()
	gateway, err := server.Open(ctx, server.Options{
		Source:  manager,
		Proxy:   server.ProxySurface(manager, handler),
		Admin:   server.AdminSurfaceWith(manager, adminAPI),
		Workers: []server.Worker{manager.Run, keyManager.Run, accounting.Run, configWorker, limitsWorker},
		Logger:  logger,
	})
	if err != nil {
		logger.Error(err.Error())
		return errReported
	}
	snapshot := manager.Capture()
	logger.Info("configuration active",
		"generation", snapshot.Generation(), "apps", len(snapshot.AppNames()), "providers", len(snapshot.ProviderNames()))
	if err := gateway.Run(ctx); err != nil {
		logger.Error(err.Error())
		return errReported
	}
	return nil
}

// adminStartupMessage keeps the operator instructions of credential failures and
// hides everything else behind a fixed message.
func adminStartupMessage(err error) string {
	switch {
	case errors.Is(err, admin.ErrTokenMissing), errors.Is(err, admin.ErrTokenInvalid), errors.Is(err, admin.ErrTokenInUse):
		return err.Error()
	}
	return "admin credential unavailable; listeners not opened"
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
