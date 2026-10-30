//go:build linux || darwin

package transport_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"boxpool.run/boxpool/internal/transport"
)

func TestTailscaleServe(t *testing.T) {
	bin := t.TempDir()
	log := filepath.Join(bin, "log")
	script := `#!/bin/sh
echo "$*" >> ` + log + `
case "$1" in
status) echo '{"Self":{"DNSName":"head.tail1.ts.net."}}' ;;
serve) if [ -n "$FAIL" ]; then echo 'Serve is not enabled on your tailnet.' >&2; exit 1; fi ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "tailscale"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx := context.Background()
	url, err := transport.Tailscale{}.Serve(ctx, 41000)
	if err != nil || url != "https://head.tail1.ts.net:41000/" {
		t.Errorf("Serve = %q, %v", url, err)
	}
	if err := (transport.Tailscale{}).Unserve(ctx, 41000); err != nil {
		t.Error(err)
	}
	b, _ := os.ReadFile(log)
	want := "status --json\nserve --bg --https=41000 http://127.0.0.1:41000\nserve --https=41000 off\n"
	if string(b) != want {
		t.Errorf("tailscale ran:\n%s\nwant:\n%s", b, want)
	}
	t.Setenv("FAIL", "1")
	if _, err := (transport.Tailscale{}).Serve(ctx, 41001); err == nil || !strings.Contains(err.Error(), "Serve is not enabled") {
		t.Errorf("Serve without HTTPS enabled = %v; want tailscale's message", err)
	}
}
