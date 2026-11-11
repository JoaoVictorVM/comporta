package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/JoaoVictorVM/comporta/internal/admin"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
)

const (
	tokenEnvironment = "COMPORTA_ADMIN_TOKEN"
	defaultAdminURL  = "http://127.0.0.1:8081"
	clientTimeout    = 30 * time.Second
)

// errSensitiveArgs replaces the parsing errors of commands whose arguments may
// hold a token or a user ID: Cobra would quote them back.
var errSensitiveArgs = errors.New("invalid arguments; run with --help for usage")

type connection struct {
	token, url string
}

func addConnectionFlags(command *cobra.Command, c *connection) {
	command.Flags().StringVar(&c.token, "token", "", "admin token (default: $"+tokenEnvironment+")")
	command.Flags().StringVar(&c.url, "admin-url", "", "admin API URL (default: the admin listener of --config)")
	command.SetFlagErrorFunc(func(*cobra.Command, error) error { return errSensitiveArgs })
}

// client resolves the token and destination. --token wins over the
// environment; the default destination is the configured admin listener.
func (c *connection) client(command *cobra.Command) (*admin.Client, bool, error) {
	token := c.token
	if token == "" {
		token = os.Getenv(tokenEnvironment)
	}
	if token == "" {
		return nil, false, errors.New("an admin token is required: set " + tokenEnvironment + " or pass --token")
	}
	target, explicit := c.url, c.url != ""
	if !explicit {
		target = configuredAdminURL(command)
	}
	client, err := admin.NewClient(target, token, clientTimeout)
	return client, explicit, err
}

func configuredAdminURL(command *cobra.Command) string {
	path, err := command.Flags().GetString("config")
	if err != nil {
		return defaultAdminURL
	}
	catalog, err := pricing.Embedded()
	if err != nil {
		return defaultAdminURL
	}
	candidate, err := config.Load(path, config.Options{SystemTimezone: config.DetectSystemTimezone(), Catalog: catalog})
	if err != nil {
		return defaultAdminURL
	}
	listener := candidate.Listeners().Admin
	return admin.LocalURL(listener.Bind, listener.Port)
}

// callFailure turns a client error into the message the operator sees. A
// lost answer is never reported as a failure that changed nothing.
func callFailure(client *admin.Client, err error) error {
	var api *admin.APIError
	switch {
	case errors.As(err, &api):
		return errors.New(api.Message)
	case errors.Is(err, admin.ErrUnreachable):
		return fmt.Errorf("cannot reach Comporta admin API at %s; is the gateway running?", client.Target())
	case errors.Is(err, admin.ErrOutcomeUnknown):
		return errors.New("the connection to the Comporta admin API was lost; the outcome is unknown. Repeat the command to confirm: it is safe to repeat")
	}
	return errors.New("admin API call failed")
}

func exactArgs(n int) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return errSensitiveArgs
		}
		return nil
	}
}

type pauseReply struct {
	App             string `json:"app"`
	All             bool   `json:"all"`
	GlobalPaused    bool   `json:"global_paused"`
	AppPaused       *bool  `json:"app_paused"`
	EffectivePaused *bool  `json:"effective_paused"`
	Changed         bool   `json:"changed"`
	UpdatedAt       string `json:"updated_at"`
}

func localClock(value string) string {
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	return at.In(time.Local).Format("15:04:05")
}

func newPauseCommand(out streams, pause bool) *cobra.Command {
	var all bool
	var c connection
	verb, use, short := "resume", "resume <app> | --all", "Resume an app, or every app, paused by an operator"
	if pause {
		verb, use, short = "pause", "pause <app> | --all", "Stop new requests of an app, or of every app, until resumed"
	}
	command := &cobra.Command{
		Use: use, Short: short,
		Long: short + ". Requests already running finish normally. The pause is kept across restarts and " +
			"overrides the YAML; it is applied by the running gateway through the admin API.",
		Args: func(_ *cobra.Command, args []string) error {
			if (len(args) == 1 && !all) || (len(args) == 0 && all) {
				return nil
			}
			return errors.New("name one app or pass --all")
		},
		RunE: func(command *cobra.Command, args []string) error {
			client, _, err := c.client(command)
			if err != nil {
				return err
			}
			body := map[string]any{"all": true}
			if !all {
				body = map[string]any{"app": args[0]}
			}
			ctx, cancel := context.WithTimeout(command.Context(), clientTimeout)
			defer cancel()
			var reply pauseReply
			if err := client.Do(ctx, http.MethodPost, admin.Prefix+verb, body, &reply); err != nil {
				return callFailure(client, err)
			}
			writePauseOutcome(out, verb, reply)
			return nil
		},
	}
	command.Flags().BoolVar(&all, "all", false, "apply to every app")
	addConnectionFlags(command, &c)
	return command
}

func writePauseOutcome(out streams, verb string, reply pauseReply) {
	at := localClock(reply.UpdatedAt)
	switch {
	case reply.All && verb == "pause" && reply.Changed:
		fmt.Fprintf(out.stdout, "all apps paused at %s; resume with: comporta resume --all\n", at)
	case reply.All && verb == "pause":
		fmt.Fprintf(out.stdout, "all apps were already paused since %s; resume with: comporta resume --all\n", at)
	case reply.All && reply.Changed:
		fmt.Fprintf(out.stdout, "all apps resumed at %s\n", at)
	case reply.All:
		fmt.Fprintln(out.stdout, "no app was paused")
	case verb == "pause" && reply.Changed:
		fmt.Fprintf(out.stdout, "%s paused at %s; resume with: comporta resume %s\n", reply.App, at, reply.App)
	case verb == "pause":
		fmt.Fprintf(out.stdout, "%s was already paused since %s; resume with: comporta resume %s\n", reply.App, at, reply.App)
	case reply.Changed:
		fmt.Fprintf(out.stdout, "%s resumed at %s\n", reply.App, at)
	default:
		fmt.Fprintf(out.stdout, "%s was not paused by its own override\n", reply.App)
	}
	if verb == "resume" && !reply.All && reply.GlobalPaused {
		fmt.Fprintf(out.stdout, "%s remains paused by the global pause; resume every app with: comporta resume --all\n", reply.App)
	}
}

func newUsersCommand(out streams) *cobra.Command {
	users := &cobra.Command{Use: "users", Short: "Operate on end users"}
	var c connection
	forget := &cobra.Command{
		Use:   "forget <user-id>",
		Short: "Delete every record of an end user in every app",
		Long: "Delete the request records, daily usage and limit counters of an end user, in every app " +
			"including removed ones, in one transaction. App totals are kept. The gateway must be running. " +
			"The ID stays in your shell history; the gateway never logs it.",
		Args: exactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			client, _, err := c.client(command)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(command.Context(), clientTimeout)
			defer cancel()
			var reply admin.ForgetResponse
			if err := client.Do(ctx, http.MethodPost, admin.Prefix+"users/forget", map[string]string{"user_id": args[0]}, &reply); err != nil {
				return callFailure(client, err)
			}
			if reply.DeletedRows == 0 && reply.UnlinkedRows == 0 && reply.ClearedMemorySubjects == 0 {
				fmt.Fprintln(out.stdout, "no records found for this user ID")
				return nil
			}
			fmt.Fprintf(out.stdout, "deleted %d rows; unlinked %d rows kept for app totals; cleared %d in-memory counters\n",
				reply.DeletedRows, reply.UnlinkedRows, reply.ClearedMemorySubjects)
			return nil
		},
	}
	addConnectionFlags(forget, &c)
	users.AddCommand(forget)
	return users
}

func newAdminCommand(out streams) *cobra.Command {
	group := &cobra.Command{Use: "admin", Short: "Manage the admin credential"}
	reset := &cobra.Command{
		Use:   "reset-token",
		Short: "Replace the admin token of a stopped gateway",
		Long: "Generate a new admin token and print it once. The gateway must be stopped; pauses, usage " +
			"and the user identity key are kept. The previous token stops working.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			path, err := command.Flags().GetString("config")
			if err != nil {
				return err
			}
			catalog, err := pricing.Embedded()
			if err != nil {
				return err
			}
			candidate, err := config.Load(path, config.Options{SystemTimezone: config.DetectSystemTimezone(), Catalog: catalog})
			if err != nil {
				return errors.New("configuration could not be loaded")
			}
			// The instance lock first, then the token lock: the order serve uses.
			store, err := sqlite.Prepare(command.Context(), candidate.DataDir())
			if err != nil {
				return errors.New("the gateway is running or the data directory is unavailable; stop the gateway before resetting the token")
			}
			defer store.Close()
			if err := store.Initialize(command.Context()); err != nil {
				return errors.New("usage storage unavailable; the token was not changed")
			}
			token, err := admin.ResetToken(command.Context(), candidate.DataDir(), store)
			if errors.Is(err, admin.ErrTokenInUse) {
				return err
			}
			if err != nil {
				return errors.New("the admin token reset did not complete; run the command again before starting the gateway")
			}
			fmt.Fprintf(out.stdout, "New admin token (shown only once; store it now): %s\n", token)
			return nil
		},
	}
	group.AddCommand(reset)
	return group
}
