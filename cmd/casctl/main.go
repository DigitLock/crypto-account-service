// Command casctl manages tenants and service tokens of CAS (SRS — Core UC-105).
// It connects with the owner role through CASCTL_DATABASE_URL only.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"

	"github.com/DigitLock/crypto-account-service/internal/registry"
)

const dbURLVar = "CASCTL_DATABASE_URL"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.Getenv)
	stop()
	os.Exit(code)
}

// run executes one casctl command and returns the exit code. Errors go to stderr,
// never with the connection string.
func run(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) int {
	app := &app{getenv: getenv}
	defer app.close()

	root := app.rootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(stderr, "casctl:", safeError(err))
		return 1
	}
	return 0
}

// app opens the registry on first use, so that help and usage need no database.
type app struct {
	getenv func(string) string
	pool   *pgxpool.Pool
}

func (a *app) registry(ctx context.Context) (*registry.Registry, error) {
	if a.pool == nil {
		url := a.getenv(dbURLVar)
		if url == "" {
			return nil, errors.New(dbURLVar + " is not set: the owner role connection string")
		}
		pool, err := pgxpool.New(ctx, url)
		if err != nil {
			// The parse error would quote the connection string.
			return nil, errors.New(dbURLVar + " is not a valid PostgreSQL connection string")
		}
		a.pool = pool
	}
	return registry.New(a.pool), nil
}

func (a *app) close() {
	if a.pool != nil {
		a.pool.Close()
	}
}

// safeError replaces connection failures, whose text names host and user, with a fixed message.
func safeError(err error) error {
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return errors.New("cannot connect to the database of " + dbURLVar)
	}
	return err
}

func (a *app) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "casctl",
		Short:         "Manage tenants and service tokens of CAS",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(a.tenantCmd(), a.tokenCmd())
	return root
}

func (a *app) tenantCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "tenant", Short: "Create, list, disable and enable tenants"}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "create <name>",
			Short: "Create an ACTIVE tenant",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				reg, err := a.registry(cmd.Context())
				if err != nil {
					return err
				}
				t, err := reg.CreateTenant(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Tenant %s created: %s\n", t.Name, t.ID)
				return nil
			},
		},
		&cobra.Command{
			Use:   "list",
			Short: "List tenants",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				reg, err := a.registry(cmd.Context())
				if err != nil {
					return err
				}
				tenants, err := reg.ListTenants(cmd.Context())
				if err != nil {
					return err
				}
				w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "NAME\tSTATUS\tCREATED")
				for _, t := range tenants {
					fmt.Fprintf(w, "%s\t%s\t%s\n", t.Name, t.Status, formatTime(&t.CreatedAt))
				}
				return w.Flush()
			},
		},
		a.statusCmd("disable", "Disable a tenant: its tokens are refused, its data stays", registry.TenantDisabled,
			(*registry.Registry).DisableTenant),
		a.statusCmd("enable", "Enable a disabled tenant", registry.TenantActive, (*registry.Registry).EnableTenant),
	)
	return cmd
}

func (a *app) statusCmd(
	verb, short, status string, change func(*registry.Registry, context.Context, string) (bool, error),
) *cobra.Command {
	return &cobra.Command{
		Use:   verb + " <name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := a.registry(cmd.Context())
			if err != nil {
				return err
			}
			changed, err := change(reg, cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if changed {
				fmt.Fprintf(cmd.OutOrStdout(), "Tenant %s is %s\n", args[0], status)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Tenant %s is already %s: nothing changed\n", args[0], status)
			}
			return nil
		},
	}
}

func (a *app) tokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Issue, list and revoke service tokens"}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "issue <tenant>",
			Short: "Issue a service token; it is printed once",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				reg, err := a.registry(cmd.Context())
				if err != nil {
					return err
				}
				tok, err := reg.IssueToken(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				// The token alone on stdout, so that it can be captured; the note on stderr.
				fmt.Fprintln(cmd.OutOrStdout(), tok.Value)
				fmt.Fprintf(cmd.ErrOrStderr(),
					"Service token %s of tenant %s. It is shown only once and cannot be recovered: store it now.\n",
					tok.KeyID, tok.Tenant)
				return nil
			},
		},
		&cobra.Command{
			Use:   "list [<tenant>]",
			Short: "List service tokens, of all tenants or of one",
			Args:  cobra.MaximumNArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				reg, err := a.registry(cmd.Context())
				if err != nil {
					return err
				}
				var tenant string
				if len(args) == 1 {
					tenant = args[0]
				}
				tokens, err := reg.ListTokens(cmd.Context(), tenant)
				if err != nil {
					return err
				}
				w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "KEY_ID\tTENANT\tCREATED\tREVOKED")
				for _, t := range tokens {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.KeyID, t.Tenant, formatTime(&t.CreatedAt), formatTime(t.RevokedAt))
				}
				return w.Flush()
			},
		},
		&cobra.Command{
			Use:   "revoke <key_id>",
			Short: "Revoke a service token",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				reg, err := a.registry(cmd.Context())
				if err != nil {
					return err
				}
				changed, err := reg.RevokeToken(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if changed {
					fmt.Fprintf(cmd.OutOrStdout(), "Token %s revoked\n", args[0])
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "Token %s is already revoked: nothing changed\n", args[0])
				}
				return nil
			},
		},
	)
	return cmd
}

func formatTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
