// Command casctl manages tenants, service tokens and processor credentials of CAS, sets values of EVM sources and
// reconciles a source on demand (SRS — Core UC-105).
// It connects with the owner role through CASCTL_DATABASE_URL only. The group sim plays the processor against the
// API of card-auth (SRS — Card Spend §2.1.1) and never opens the database.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
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
	pool, err := a.db(ctx)
	if err != nil {
		return nil, err
	}
	return registry.New(pool), nil
}

// db opens the pool of the owner role on first use.
func (a *app) db(ctx context.Context) (*pgxpool.Pool, error) {
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
	return a.pool, nil
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
		Short:         "Manage tenants, service tokens and processor credentials of CAS; simulate the processor",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(a.tenantCmd(), a.tokenCmd(), a.processorCmd(), a.sourceCmd(), a.reconcileCmd(), a.simCmd())
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

func (a *app) processorCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "processor", Short: "Issue, list and revoke Basic credentials of the processor API of card-auth"}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "issue <tenant>",
			Short: "Issue a processor credential; the pair is printed once as username:password",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				reg, err := a.registry(cmd.Context())
				if err != nil {
					return err
				}
				pair, err := reg.IssueProcessorCredential(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				// The pair alone on stdout, so that it can be captured; the note on stderr.
				fmt.Fprintln(cmd.OutOrStdout(), pair.Username+":"+pair.Password)
				fmt.Fprintf(cmd.ErrOrStderr(),
					"Processor credential %s of tenant %s. It is shown only once and cannot be recovered: store it now.\n",
					pair.Username, pair.Tenant)
				return nil
			},
		},
		&cobra.Command{
			Use:   "list [<tenant>]",
			Short: "List processor credentials, of all tenants or of one",
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
				pairs, err := reg.ListProcessorCredentials(cmd.Context(), tenant)
				if err != nil {
					return err
				}
				w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
				fmt.Fprintln(w, "USERNAME\tTENANT\tCREATED\tREVOKED")
				for _, p := range pairs {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.KeyID, p.Tenant, formatTime(&p.CreatedAt), formatTime(p.RevokedAt))
				}
				return w.Flush()
			},
		},
		&cobra.Command{
			Use:   "revoke <username>",
			Short: "Revoke a processor credential",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				reg, err := a.registry(cmd.Context())
				if err != nil {
					return err
				}
				changed, err := reg.RevokeProcessorCredential(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				if changed {
					fmt.Fprintf(cmd.OutOrStdout(), "Processor credential %s revoked\n", args[0])
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "Processor credential %s is already revoked: nothing changed\n", args[0])
				}
				return nil
			},
		},
	)
	return cmd
}

func (a *app) sourceCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "source", Short: "Set values and the treasury connection of EVM sources; development seeds of sources"}
	cmd.AddCommand(&cobra.Command{
		Use: "set <source> <key>=<value>",
		Short: "Set one value of an EVM source: controller_address, backfill_floor (sources.config) or " +
			"token_address (alias of the tracked token, USDC with 6 decimals)",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value, ok := strings.Cut(args[1], "=")
			if !ok {
				return errors.New("the second argument must be <key>=<value>")
			}
			reg, err := a.registry(cmd.Context())
			if err != nil {
				return err
			}
			previous, stored, err := setSourceValue(cmd.Context(), reg, args[0], key, value)
			if err != nil {
				return err
			}
			if previous == "" {
				previous = "unset"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Source %s, %s: previous %s, new %s\n", args[0], key, previous, stored)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "set-treasury <source> <connection_id>",
		Short: "Name the treasury connection of an EVM source: its ID to treasury_connection and its wallet address " +
			"to treasury_address",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuid.Parse(args[1])
			if err != nil || len(args[1]) != 36 {
				return errors.New("the connection ID must be a UUID in its 36-character form")
			}
			reg, err := a.registry(cmd.Context())
			if err != nil {
				return err
			}
			previous, current, err := reg.SetTreasury(cmd.Context(), args[0], id)
			if err != nil {
				return err
			}
			unset := func(s string) string {
				if s == "" {
					return "unset"
				}
				return s
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Source %s, %s: previous %s, new %s\n", args[0], registry.KeyTreasuryConnection,
				unset(previous.Connection), current.Connection)
			fmt.Fprintf(out, "Source %s, %s: previous %s, new %s\n", args[0], registry.KeyTreasuryAddress,
				unset(previous.Address), current.Address)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "add-fake",
		Short: "Add the source fake of the fake connector and the aliases of its assets. Development and demo only",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			reg, err := a.registry(cmd.Context())
			if err != nil {
				return err
			}
			seed, err := reg.AddFakeSource(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			switch {
			case !seed.SourceAdded && len(seed.AliasesAdded) == 0:
				fmt.Fprintln(out, "Source fake and its aliases exist: nothing changed")
				return nil
			case seed.SourceAdded:
				fmt.Fprintln(out, "Source fake added")
			default:
				fmt.Fprintln(out, "Source fake exists")
			}
			if len(seed.AliasesAdded) > 0 {
				fmt.Fprintln(out, "Aliases added: "+strings.Join(seed.AliasesAdded, ", "))
			}
			return nil
		},
	})
	return cmd
}

func formatTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.UTC().Format(time.RFC3339)
}
