package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"boxpool.run/boxpool/internal/auth"
	"boxpool.run/boxpool/internal/client"
	"boxpool.run/boxpool/internal/preflight"
)

const loginUsage = `Usage:
  boxpool login <head>[:port] <token>
  boxpool logout

login saves a head's address and an access token from boxpool token create
--scope cli on that head. From then on, sandbox commands and ls on this
machine go to that head instead of the local daemon. logout forgets them.
The port defaults to 7443.
`

// remoteLogin is what boxpool login saves.
type remoteLogin struct {
	Head  string `json:"head"`
	Token string `json:"token"`
}

// loginPath is the file boxpool login writes: in BOXPOOL_HOME when it is
// set, as for every other boxpool file, else in the user's config
// directory, as the daemon's state directory may not be writable.
func loginPath() (string, error) {
	if dir := os.Getenv("BOXPOOL_HOME"); dir != "" {
		return filepath.Join(dir, "login.json"), nil
	}
	dir, err := os.UserConfigDir()
	return filepath.Join(dir, "boxpool", "login.json"), err
}

// readLogin returns the saved login, or a zero one if there is none.
func readLogin() (remoteLogin, error) {
	var l remoteLogin
	path, err := loginPath()
	if err != nil {
		return l, err
	}
	b, err := os.ReadFile(path)
	// One this user cannot read, as in a root daemon's BOXPOOL_HOME, is not theirs.
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return l, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &l)
	}
	if err != nil {
		return l, fmt.Errorf("%s: %w", path, err)
	}
	return l, nil
}

func login(args []string) int {
	if len(args) != 2 {
		fmt.Fprint(os.Stderr, loginUsage)
		return 2
	}
	head := args[0]
	if _, _, err := net.SplitHostPort(head); err != nil {
		head = net.JoinHostPort(head, auth.JoinPort)
	}
	if err := auth.ValidateJoinAddr(head); err != nil {
		fmt.Fprintf(os.Stderr, "boxpool login: %v\n", err)
		return 2
	}
	c, err := client.Remote(head, args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool login: %v\n", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Health(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "boxpool login: %s: %v\n", head, err)
		return 1
	}
	path, err := loginPath()
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0700)
	}
	if err == nil {
		b, _ := json.Marshal(remoteLogin{Head: head, Token: args[1]})
		err = os.WriteFile(path, b, 0600)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "boxpool login: %v\n", err)
		return 1
	}
	fmt.Printf("logged in to %s\n", head)
	return 0
}

func logout(args []string) int {
	if len(args) != 0 {
		fmt.Fprint(os.Stderr, loginUsage)
		return 2
	}
	path, err := loginPath()
	if err == nil {
		err = os.Remove(path)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "boxpool logout: %v\n", err)
		return 1
	}
	return 0
}

// newClient returns a client for the head boxpool login saved, or else
// for the local daemon.
func newClient() (*client.Client, error) {
	l, err := readLogin()
	if err != nil {
		return nil, err
	}
	if l.Head != "" {
		return client.Remote(l.Head, l.Token)
	}
	dir, err := preflight.StateDir()
	if err != nil {
		return nil, err
	}
	return client.New(dir), nil
}
