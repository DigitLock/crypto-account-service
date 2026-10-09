package main

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// connectionCmd holds the read-only views of a connection for the operator (X1 D-45).
func (a *app) connectionCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "connection", Short: "Inspect a connection without its account identity or amounts"}
	cmd.AddCommand(&cobra.Command{
		Use: "inspect <connection_id>",
		Short: "Status, permissions, ip_restricted, ciphertext present, stored rows and audit rows of a connection; " +
			"never the account identity, a uid, an asset, an amount or the fingerprint",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := uuid.Parse(args[0])
			if err != nil || len(args[0]) != 36 {
				return fmt.Errorf("the connection_id is not a UUID")
			}
			reg, err := a.registry(cmd.Context())
			if err != nil {
				return err
			}
			in, err := reg.InspectConnection(cmd.Context(), id)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if in.Found {
				fmt.Fprintf(out, "connection %s: found\n", id)
				fmt.Fprintf(out, "status: %s\n", in.Status)
				fmt.Fprintf(out, "permissions: %s\n", strings.Join(in.Permissions, ","))
				ip := in.IPRestricted
				if ip == "" {
					ip = "not recorded"
				}
				fmt.Fprintf(out, "ip_restricted: %s\n", ip)
				fmt.Fprintf(out, "ciphertext: %s\n", yesNo(in.HasCiphertext))
			} else {
				fmt.Fprintf(out, "connection %s: not found\n", id)
			}
			fmt.Fprintf(out, "snapshots: %d\nbalance rows: %d\ncursors: %d\nledger entries: %d\n",
				in.Snapshots, in.BalanceRows, in.Cursors, in.LedgerEntries)
			var audit []string
			for _, c := range in.Audit {
				audit = append(audit, fmt.Sprintf("%s %d", c.Action, c.Rows))
			}
			if len(audit) == 0 {
				audit = []string{"none"}
			}
			fmt.Fprintf(out, "audit rows: %s\n", strings.Join(audit, ", "))
			fmt.Fprintf(out, "uid in audit details: %s\n", yesNo(in.AuditHasUID))
			return nil
		},
	})
	return cmd
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
