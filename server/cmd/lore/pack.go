package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/lore-gpt/lore/core/httpapi"
)

// packCmd retrieves a context pack from a running server's POST /v1/pack and prints it. It is a thin HTTP
// client — it needs only the server URL and an API key (from --api-key or LORE_API_KEY), never the database —
// so it works against any reachable Lore server. It reuses httpapi's request/response types so the CLI can
// never drift from the wire contract.
func packCmd() *cobra.Command {
	var (
		url, runID, query, apiKey string
		minSeq                    int64
		scopes                    []string
		limit, budget             int
		asJSON                    bool
	)
	cmd := &cobra.Command{
		Use:   "pack",
		Short: "Retrieve a context pack for a run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			key := apiKey
			if key == "" {
				key = os.Getenv("LORE_API_KEY")
			}
			if key == "" {
				return fmt.Errorf("no API key: pass --api-key or set LORE_API_KEY (e.g. source the credentials file)")
			}

			body, err := json.Marshal(httpapi.PackRequest{
				RunID:       runID,
				Query:       query,
				MinSeq:      minSeq,
				Scopes:      scopes,
				Limit:       limit,
				TokenBudget: budget,
			})
			if err != nil {
				return err
			}

			ctx, stop := signalContext()
			defer stop()
			reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url+"/v1/pack", bytes.NewReader(body))
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+key)
			req.Header.Set("Content-Type", "application/json")

			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return fmt.Errorf("request %s: %w", url, err)
			}
			defer func() { _ = resp.Body.Close() }()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				return err
			}
			if resp.StatusCode != http.StatusOK {
				// The server's error body is already a structured {code,message}; surface it verbatim.
				return fmt.Errorf("pack failed (%s): %s", resp.Status, bytes.TrimSpace(raw))
			}
			if asJSON {
				_, err = cmd.OutOrStdout().Write(append(bytes.TrimSpace(raw), '\n'))
				return err
			}
			var pack httpapi.PackResponse
			if err := json.Unmarshal(raw, &pack); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			return renderPack(cmd.OutOrStdout(), pack)
		},
	}
	cmd.Flags().StringVar(&url, "url", "http://localhost:8080", "base URL of the Lore server")
	cmd.Flags().StringVar(&runID, "run-id", "", "run to pack context for (required)")
	cmd.Flags().StringVar(&query, "query", "", "retrieval query (required)")
	cmd.Flags().StringVar(&apiKey, "api-key", "", "bearer token (default: LORE_API_KEY)")
	cmd.Flags().Int64Var(&minSeq, "min-seq", 0, "read-your-writes barrier: the run seq the pack must reflect")
	cmd.Flags().StringArrayVar(&scopes, "scope", nil, "restrict recall to a scope (repeatable; default: project-wide)")
	cmd.Flags().IntVar(&limit, "limit", 0, "max distilled memories (0 = server default)")
	cmd.Flags().IntVar(&budget, "budget", 0, "token budget for distilled recall (0 = unbounded)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the raw JSON response instead of a table")
	_ = cmd.MarkFlagRequired("run-id")
	_ = cmd.MarkFlagRequired("query")
	return cmd
}

// renderPack prints the pack text, a one-line provenance footer, and a table of the sources that composed it.
func renderPack(w io.Writer, pack httpapi.PackResponse) error {
	if _, err := fmt.Fprintln(w, pack.Text); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w,
		"\ncovered_seq=%d  freshness_lag_ms=%d  saved_tokens=%d  working=%s  truncated=%t\n",
		pack.CoveredSeq, pack.FreshnessLagMs, pack.SavedTokens, pack.WorkingSource, pack.Truncated); err != nil {
		return err
	}
	if len(pack.Sources) == 0 {
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "\nKIND\tSCORE\tSECTION\tID"); err != nil {
		return err
	}
	for _, s := range pack.Sources {
		if _, err := fmt.Fprintf(tw, "%s\t%.4f\t%s\t%s\n", s.Kind, s.Score, s.Section, s.ID); err != nil {
			return err
		}
	}
	return tw.Flush()
}
