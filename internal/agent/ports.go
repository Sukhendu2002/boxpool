package agent

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
)

func getPorts(w http.ResponseWriter, r *http.Request) {
	ports, err := listeningPorts()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ports)
}

func parsePorts(r io.Reader) ([]Port, error) {
	ports := []Port{}
	s := bufio.NewScanner(r)
	for line := 1; s.Scan(); line++ {
		fields := strings.Fields(s.Text())
		if len(fields) == 0 || fields[0] == "sl" {
			continue
		}
		if len(fields) < 4 {
			return nil, fmt.Errorf("line %d: missing TCP fields", line)
		}
		if fields[3] != "0A" {
			continue
		}
		hexIP, hexPort, ok := strings.Cut(fields[1], ":")
		if !ok || (len(hexIP) != 8 && len(hexIP) != 32) || len(hexPort) != 4 {
			return nil, fmt.Errorf("line %d: bad local address %q", line, fields[1])
		}
		ip := make([]byte, len(hexIP)/2)
		for i := 0; i < len(hexIP); i += 8 {
			word, err := strconv.ParseUint(hexIP[i:i+8], 16, 32)
			if err != nil {
				return nil, fmt.Errorf("line %d: bad address: %w", line, err)
			}
			// /proc prints each address word in little-endian byte order.
			binary.LittleEndian.PutUint32(ip[i/2:i/2+4], uint32(word))
		}
		port, err := strconv.ParseUint(hexPort, 16, 16)
		if err != nil {
			return nil, fmt.Errorf("line %d: bad port: %w", line, err)
		}
		addr, _ := netip.AddrFromSlice(ip)
		ports = append(ports, Port{Proto: "tcp", Address: addr.String(), Port: int(port)})
	}
	return ports, s.Err()
}
