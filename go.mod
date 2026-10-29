module boxpool.run/boxpool

go 1.24.0

require (
	github.com/Code-Hex/vz/v3 v3.7.1
	github.com/coder/websocket v1.8.15
	github.com/creack/pty v1.1.24
	github.com/hashicorp/go-hclog v1.6.2
	github.com/hashicorp/raft v1.7.3
	github.com/klauspost/compress v1.19.2
	golang.org/x/sys v0.37.0
	golang.org/x/term v0.36.0
	modernc.org/sqlite v1.46.1
)

require (
	github.com/Code-Hex/go-infinity-channel v1.0.0 // indirect
	github.com/armon/go-metrics v0.4.1 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/fatih/color v1.13.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/hashicorp/go-immutable-radix v1.0.0 // indirect
	github.com/hashicorp/go-metrics v0.5.4 // indirect
	github.com/hashicorp/go-msgpack/v2 v2.1.2 // indirect
	github.com/hashicorp/golang-lru v0.5.0 // indirect
	github.com/mattn/go-colorable v0.1.12 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/mod v0.29.0 // indirect
	modernc.org/libc v1.67.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)

// shortcut: patched fork of vz v3 for the observer goroutine leak (upstream fix is v4 only, needs Go 1.25); drop this replace when boxpool moves to vz v4
replace github.com/Code-Hex/vz/v3 => github.com/Sukhendu2002/vz/v3 v3.7.1-boxpool.1
