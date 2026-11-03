package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"boxpool.run/boxpool/internal/store"
)

func auditCmd(args []string) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: boxpool audit [--json] [--limit 100] [--before ID]\n\nShows this head's audit log, newest first. Events are retained for 90 days.\nRun on the head; this command uses the local daemon even after login.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	asJSON := fs.Bool("json", false, "print a JSON array instead of a table")
	limit := fs.Int("limit", 100, "maximum events to show (1–1000)")
	before := fs.Int64("before", 0, "show events older than this event ID")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 || *limit < 1 || *limit > 1000 || *before < 0 {
		fs.Usage()
		return 2
	}
	ctx := context.Background()
	c, ok := connectLocal(ctx, "audit")
	if !ok {
		return 1
	}
	list, err := c.Audit(ctx, *before, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool audit: %v\n", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		err = enc.Encode(list)
	} else {
		err = writeAuditTable(os.Stdout, list)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool audit: %v\n", err)
		return 1
	}
	return 0
}

func writeAuditTable(w io.Writer, list []store.AuditEvent) error {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME\tTOKEN\tACTION\tTARGET")
	for _, e := range list {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", e.ID, e.Time.UTC().Format(time.RFC3339), e.TokenID, e.Action, e.Target)
	}
	return tw.Flush()
}
