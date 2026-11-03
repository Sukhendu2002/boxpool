//go:build darwin && cgo

package vz

import (
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"
	"unsafe"

	virt "github.com/Code-Hex/vz/v3"
	"golang.org/x/sys/unix"
)

// packetBridge is the VM's sole path to vmnet. Errors close both packet
// channels; there is no fallback to an unfiltered framework attachment.
type packetBridge struct {
	file, nativeFile *os.File
	guest, host      *net.UnixConn
	native           *nativeInterface
	filter           *packetFilter
	wg               sync.WaitGroup
	failOnce         sync.Once
}

func datagramPair() (*os.File, *net.UnixConn, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, nil, err
	}
	for _, fd := range fds {
		unix.CloseOnExec(fd)
		for _, opt := range []struct{ name, size int }{{unix.SO_SNDBUF, 64 << 10}, {unix.SO_RCVBUF, 256 << 10}} {
			if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, opt.name, opt.size); err != nil {
				unix.Close(fds[0])
				unix.Close(fds[1])
				return nil, nil, err
			}
		}
	}
	file, peer := os.NewFile(uintptr(fds[0]), "vmnet-packets"), os.NewFile(uintptr(fds[1]), "vmnet-peer")
	conn, err := net.FileConn(peer)
	peer.Close()
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	return file, conn.(*net.UnixConn), nil
}

func newPacketBridge(network unsafe.Pointer, mac string) (*packetBridge, *virt.VirtioNetworkDeviceConfiguration, error) {
	config, err := networkConfig(network)
	if err != nil {
		return nil, nil, err
	}
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return nil, nil, err
	}
	b := &packetBridge{}
	ok := false
	defer func() {
		if !ok {
			b.close()
		}
	}()
	if b.file, b.guest, err = datagramPair(); err != nil {
		return nil, nil, err
	}
	if b.nativeFile, b.host, err = datagramPair(); err != nil {
		return nil, nil, err
	}
	if b.native, err = startNativeInterface(network, b.nativeFile.Fd()); err != nil {
		return nil, nil, err
	}
	b.filter = &packetFilter{
		prefix:  netip.PrefixFrom(netip.MustParseAddr(config.Gateway), 24).Masked(),
		gateway: netip.MustParseAddr(config.Gateway), mac: hw,
		addrs: net.InterfaceAddrs, now: time.Now, replies: make(map[packetFlow]replyState),
	}
	attachment, err := virt.NewFileHandleNetworkDeviceAttachment(b.file)
	if err != nil {
		return nil, nil, err
	}
	nic, err := virt.NewVirtioNetworkDeviceConfiguration(attachment)
	if err != nil {
		return nil, nil, err
	}
	addr, err := virt.NewMACAddress(hw)
	if err != nil {
		return nil, nil, err
	}
	nic.SetMACAddress(addr)
	b.wg.Add(2)
	go b.forward(b.guest, func(frame []byte) error {
		if b.filter.allow(frame) {
			return b.native.write(frame)
		}
		return nil
	})
	go b.receive()
	ok = true
	return b, nic, nil
}

func (b *packetBridge) forward(from *net.UnixConn, send func([]byte) error) {
	defer b.wg.Done()
	defer b.fail()
	buf := make([]byte, 65536)
	for {
		n, err := from.Read(buf)
		if err == nil && n == 0 {
			err = errors.New("empty vmnet packet")
		}
		if err == nil {
			err = send(buf[:n])
		}
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Warn("sandbox packet bridge stopped; network is disconnected", "err", err)
			}
			return
		}
	}
}

// Native callbacks only signal availability. Individual synchronous reads
// yield vmnet's queue between packets, including during a sustained flood.
func (b *packetBridge) receive() {
	defer b.wg.Done()
	defer b.fail()
	buf := make([]byte, 65536)
	notification := make([]byte, 128)
	for {
		n, err := b.native.read(buf)
		if err == nil && n > 0 {
			b.filter.receive(buf[:n])
			_, err = b.guest.Write(buf[:n])
		} else if err == nil {
			_, err = b.host.Read(notification)
		}
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				slog.Warn("sandbox packet bridge stopped; network is disconnected", "err", err)
			}
			return
		}
	}
}

func (b *packetBridge) fail() {
	b.failOnce.Do(func() {
		if b.guest != nil {
			b.guest.Close()
		}
		if b.host != nil {
			b.host.Close()
		}
	})
}

// close is called once, after the VM stops or its construction fails. Native
// callbacks and Go loops finish before their descriptors can be reused.
func (b *packetBridge) close() {
	b.fail()
	b.wg.Wait()
	if b.native != nil {
		if err := b.native.close(); err != nil {
			slog.Warn("stop sandbox packet interface", "err", err)
		}
	}
	for _, f := range []*os.File{b.file, b.nativeFile} {
		if f != nil {
			if err := f.Close(); err != nil {
				slog.Warn("close sandbox packet descriptor", "err", err)
			}
		}
	}
}
