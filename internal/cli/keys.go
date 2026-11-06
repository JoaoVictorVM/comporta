package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/keys"
)

const (
	maxSuggestionDistance = 2
	maxEchoedArgument     = 64
)

func newKeysCommand(out streams) *cobra.Command {
	command := &cobra.Command{
		Use:   "keys",
		Short: "Manage app keys",
		Long: "Create, list, rotate and revoke app keys in the data directory. These commands " +
			"work with or without a running service and need no provider credentials.",
	}
	command.AddCommand(
		newKeysIssueCommand(out, "create", "Create a key for an app and print it once"),
		newKeysIssueCommand(out, "rotate", "Add a key for an app, keeping the others valid until revoked"),
		newKeysListCommand(out),
		newKeysRevokeCommand(out),
	)
	return command
}

// loadKeysConfig validates the configuration structurally: keys commands must work
// on a machine that does not hold provider secrets.
func loadKeysConfig(command *cobra.Command, out streams) (*config.Candidate, error) {
	path, err := command.Flags().GetString("config")
	if err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("configuration path cannot be resolved")
	}
	candidate, err := config.Load(absolute, config.Options{SystemTimezone: config.DetectSystemTimezone()})
	if err != nil {
		printDiagnostics(out.stderr, err)
		return nil, errReported
	}
	for _, warning := range candidate.Warnings() {
		_, _ = fmt.Fprintln(out.stderr, "warning:", warning.String())
	}
	return candidate, nil
}

func newKeysIssueCommand(out streams, verb, short string) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " <app>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			candidate, err := loadKeysConfig(command, out)
			if err != nil {
				return err
			}
			app := args[0]
			if !candidate.HasApp(app) {
				_, _ = fmt.Fprintln(out.stderr, unknownAppMessage(app, candidate.AppNames()))
				return errReported
			}
			created, err := issueKey(command.Context(), keys.NewStore(candidate.DataDir()), verb, app)
			if err != nil {
				return reportKeysError(out, err)
			}
			return printCreated(out, app, created)
		},
	}
}

func issueKey(ctx context.Context, store *keys.Store, verb, app string) (keys.Created, error) {
	if verb == "rotate" {
		return store.Rotate(ctx, app)
	}
	return store.Create(ctx, app)
}

// printCreated writes everything at once, because the value cannot be recovered
// once the commit is done: a failure here can only lead to replacing the key.
func printCreated(out streams, app string, created keys.Created) error {
	text := fmt.Sprintf("Key for %s (shown only once): %s\nStore it in the app's environment. Key ID: %s\n",
		app, created.Key.Reveal(), created.Metadata.ID)
	if _, err := fmt.Fprint(out.stdout, text); err != nil {
		_, _ = fmt.Fprintf(out.stderr,
			"the key was saved but could not be displayed; revoke key ID %s and create another\n", created.Metadata.ID)
		return errReported
	}
	return nil
}

func newKeysListCommand(out streams) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List keys without revealing them",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			candidate, err := loadKeysConfig(command, out)
			if err != nil {
				return err
			}
			entries, err := keys.NewStore(candidate.DataDir()).List()
			if err != nil {
				return reportKeysError(out, err)
			}
			return printKeyTable(out, entries)
		},
	}
}

func printKeyTable(out streams, entries []keys.Metadata) error {
	table := tabwriter.NewWriter(out.stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "KEY ID\tAPP\tSTATUS\tCREATED AT\tLAST USED AT")
	for _, entry := range entries {
		status := "active"
		if !entry.Active() {
			status = "revoked"
		}
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\n", entry.ID, entry.App, status,
			entry.CreatedAt.UTC().Format(time.RFC3339), formatLastUsed(entry.LastUsedAt))
	}
	if err := table.Flush(); err != nil {
		_, _ = fmt.Fprintln(out.stderr, "the key list could not be written")
		return errReported
	}
	return nil
}

func formatLastUsed(at time.Time) string {
	if at.IsZero() {
		return "never"
	}
	return at.UTC().Format(time.RFC3339)
}

func newKeysRevokeCommand(out streams) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <key-id>",
		Short: "Revoke a key by its ID",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			candidate, err := loadKeysConfig(command, out)
			if err != nil {
				return err
			}
			id := args[0]
			changed, err := keys.NewStore(candidate.DataDir()).Revoke(command.Context(), id)
			if err != nil {
				return reportKeysError(out, err)
			}
			message := "Key %s revoked\n"
			if !changed {
				message = "Key %s was already revoked\n"
			}
			_, _ = fmt.Fprintf(out.stdout, message, id)
			return nil
		},
	}
}

func reportKeysError(out streams, err error) error {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		_, _ = fmt.Fprintln(out.stderr, "interrupted before the change was made")
	case errors.Is(err, keys.ErrStoreIO):
		_, _ = fmt.Fprintln(out.stderr, ioErrorMessage(err))
	default:
		_, _ = fmt.Fprintln(out.stderr, err.Error())
	}
	return errReported
}

func ioErrorMessage(err error) string {
	if errors.Is(err, os.ErrPermission) {
		return keys.ErrStoreIO.Error() + ": permission denied"
	}
	return keys.ErrStoreIO.Error()
}

// unknownAppMessage quotes the argument because it came from the terminal and may
// contain control sequences; %q escapes them before they reach the diagnostic.
func unknownAppMessage(app string, known []string) string {
	message := "app " + strconv.Quote(truncateArgument(app)) + " is not defined in comporta.yaml"
	if suggestion := suggestApp(app, known); suggestion != "" {
		message += " (did you mean " + strconv.Quote(suggestion) + "?)"
	}
	return message
}

func truncateArgument(argument string) string {
	if len(argument) <= maxEchoedArgument {
		return argument
	}
	return argument[:maxEchoedArgument] + "..."
}

// suggestApp expects known in sorted order, so the first best match also settles
// ties lexically.
func suggestApp(app string, known []string) string {
	if len(app) > maxEchoedArgument {
		return ""
	}
	best, bestDistance := "", maxSuggestionDistance+1
	for _, name := range known {
		if distance := levenshtein(app, name); distance < bestDistance {
			best, bestDistance = name, distance
		}
	}
	return best
}

func levenshtein(a, b string) int {
	previous := make([]int, len(b)+1)
	current := make([]int, len(b)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := 1; i <= len(a); i++ {
		current[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			current[j] = min(previous[j]+1, current[j-1]+1, previous[j-1]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(b)]
}
