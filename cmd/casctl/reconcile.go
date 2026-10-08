package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/DigitLock/crypto-account-service/internal/reconcile"
)

// reconcileCmd is casctl reconcile <source> (SRS — Core UC-105 row 10, EC-123; S3 D-8): SRS — Card Spend UC-4 in
// the process of casctl with the owner role; the runs are stored as the worker of server stores them.
func (a *app) reconcileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reconcile <source>",
		Short: "Reconcile an EVM source: one stored run per tenant, printed with its mismatches by type",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			pool, err := a.db(cmd.Context())
			if err != nil {
				return err
			}
			runs, err := reconcile.Source(cmd.Context(), pool, args[0])
			if err != nil {
				return fmt.Errorf("source %s not reconciled, no run stored: %w", args[0], err)
			}
			for _, r := range runs {
				fmt.Fprintf(cmd.OutOrStdout(), "Tenant %s, run %s: %s\n", r.Tenant, r.ID, mismatchCounts(r.Mismatches))
			}
			return nil
		},
	}
}

// mismatchCounts is "no mismatches" or the counts by type: "MISSING_DEBIT 1, UNKNOWN_DEBIT 2".
func mismatchCounts(ms []reconcile.Mismatch) string {
	counts := reconcile.Counts(ms)
	if len(counts) == 0 {
		return "no mismatches"
	}
	parts := make([]string, len(counts))
	for i, c := range counts {
		parts[i] = fmt.Sprintf("%s %d", c.Type, c.Count)
	}
	return strings.Join(parts, ", ")
}
