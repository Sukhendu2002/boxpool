package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/daemon"
	"boxpool.run/boxpool/internal/store"
	"boxpool.run/boxpool/internal/transport"
)

const tokenUsage = `Usage:
  boxpool token create --join [--ttl 1h] [--transport direct|tailscale] [--addr host:port]
  boxpool token create --scope cli|api|expose [--sandbox name] [--ttl duration]
  boxpool token ls
  boxpool token revoke <join token | access token | access token ID>

create --join prints a token that boxpool join on another machine uses,
once, to join this machine's pool. It expires after --ttl.

create --scope prints an access token for this head's API, which
boxpool login on another machine saves. cli and api tokens run sandbox
commands and boxpool ls; expose tokens only open the URLs boxpool expose
makes. --sandbox limits a token to one sandbox. It never expires unless
--ttl is given. ls lists access tokens and when each was last used.
Restoring a snapshot with --from requires a token without --sandbox.

revoke makes an unused join token, or an access token, invalid at once.

--transport is how nodes reach this machine. direct, the default, names
this host's name on the LAN. tailscale names its tailnet address, so a node
behind another NAT joins with no port forwarding; it works with Headscale
too, and each node must be on the same tailnet. --addr overrides either.
`

func tokenCmd(args []string) int {
	if len(args) == 2 && args[0] == "revoke" {
		return revokeToken(args[1])
	}
	if len(args) == 1 && args[0] == "ls" {
		return lsTokens()
	}
	if len(args) == 0 || args[0] != "create" {
		fmt.Fprint(os.Stderr, tokenUsage)
		return 2
	}
	fs := flag.NewFlagSet("token create", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, tokenUsage, "\nFlags:\n"); fs.PrintDefaults() }
	join := fs.Bool("join", false, "create a join token")
	scope := fs.String("scope", "", "create an access token: cli, api or expose")
	sandbox := fs.String("sandbox", "", "the only sandbox an access token reaches")
	ttl := fs.Duration("ttl", daemon.DefaultTokenTTL, "how long the token is valid (default for access tokens: forever)")
	addr := fs.String("addr", "", "head address the token names, host:port (default: from --transport, port 7443)")
	mode := fs.String("transport", "direct", "how nodes reach this head: direct or tailscale")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if *join == (*scope != "") || fs.NArg() != 0 || *join && set["sandbox"] || !*join && (set["addr"] || set["transport"]) {
		fs.Usage()
		return 2
	}
	if *ttl <= 0 {
		fmt.Fprintf(os.Stderr, "boxpool token create: --ttl must be positive\n")
		return 2
	}
	if *scope != "" {
		if !set["ttl"] {
			*ttl = 0
		}
		return createAccessToken(daemon.AccessTokenRequest{Scope: *scope, Sandbox: *sandbox, TTL: *ttl})
	}
	tr, err := transport.New(*mode)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool token create: unknown --transport %q: use direct or tailscale\n", *mode)
		return 2
	}
	if *addr == "" {
		a, err := tr.Address(context.Background(), auth.JoinPort)
		if err != nil {
			fmt.Fprintf(os.Stderr, "boxpool token create: %v\n", err)
			return 1
		}
		*addr = a
	}
	ctx := context.Background()
	c, ok := connectLocal(ctx, "token create")
	if !ok {
		return 1
	}
	tok, err := c.CreateJoinToken(ctx, daemon.TokenRequest{TTL: *ttl, Addr: *addr})
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool token create: %v\n", err)
		return 1
	}
	fmt.Println(tok.Token)
	fmt.Fprintf(os.Stderr, "Single use; expires %s.\n", tok.ExpiresAt.Local().Format(time.DateTime))
	return 0
}

func createAccessToken(req daemon.AccessTokenRequest) int {
	ctx := context.Background()
	c, ok := connectLocal(ctx, "token create")
	if !ok {
		return 1
	}
	tok, err := c.CreateAccessToken(ctx, req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool token create: %v\n", err)
		return 1
	}
	fmt.Println(tok.Token)
	expires := "never expires"
	if !tok.ExpiresAt.IsZero() {
		expires = "expires " + tok.ExpiresAt.Local().Format(time.DateTime)
	}
	fmt.Fprintf(os.Stderr, "ID %s; %s. Use it with: boxpool login <this head's address> <token>\n", tok.ID, expires)
	return 0
}

// revokeToken revokes a join token, or an access token given whole or by
// its ID.
func revokeToken(arg string) int {
	ctx := context.Background()
	c, ok := connectLocal(ctx, "token revoke")
	if !ok {
		return 1
	}
	var err error
	if strings.HasPrefix(arg, "bpjoin") {
		err = c.RevokeJoinToken(ctx, arg)
	} else {
		id := arg
		if tok, perr := auth.ParseAccessToken(arg); perr == nil {
			id = tok.Hash()[:store.AccessIDLen]
		}
		err = c.RevokeAccessToken(ctx, id)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool token revoke: %v\n", err)
		return 1
	}
	return 0
}

func lsTokens() int {
	ctx := context.Background()
	c, ok := connectLocal(ctx, "token ls")
	if !ok {
		return 1
	}
	list, err := c.ListAccessTokens(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool token ls: %v\n", err)
		return 1
	}
	now := time.Now()
	since := func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return age(now.Sub(t)) + " ago"
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "ID\tSCOPE\tSANDBOX\tCREATED\tEXPIRES\tLAST USED")
	for _, t := range list {
		expires := "never"
		if !t.ExpiresAt.IsZero() {
			expires = t.ExpiresAt.Local().Format(time.DateTime)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Scope, cmp.Or(t.Sandbox, "-"), since(t.CreatedAt), expires, since(t.LastUsed))
	}
	tw.Flush()
	return 0
}
