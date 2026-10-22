package agent

import (
	"reflect"
	"strings"
	"testing"
)

func TestParsePorts(t *testing.T) {
	text := `  sl  local_address rem_address st tx_queue rx_queue
   0: 00000000:1F40 00000000:0000 0A 00000000:00000000
   1: 0100007F:0050 00000000:0000 0A 00000000:00000000
   2: 0100007F:ABCD 0100007F:0050 01 00000000:00000000
   3: 00000000:0000 00000000:0000 06 00000000:00000000
   4: 00000000000000000000000000000000:01BB 00000000000000000000000000000000:0000 0A 0:0
   5: 00000000000000000000000001000000:1F90 00000000000000000000000000000000:0000 0A 0:0
   6: B80D012078563412F0DEBC9A01000000:FFFF 00000000000000000000000000000000:0000 0A 0:0
   7: 0000000000000000FFFF0000010200C0:1234 00000000000000000000000000000000:0000 0A 0:0
`
	want := []Port{
		{Proto: "tcp", Address: "0.0.0.0", Port: 8000},
		{Proto: "tcp", Address: "127.0.0.1", Port: 80},
		{Proto: "tcp", Address: "::", Port: 443},
		{Proto: "tcp", Address: "::1", Port: 8080},
		{Proto: "tcp", Address: "2001:db8:1234:5678:9abc:def0:0:1", Port: 65535},
		{Proto: "tcp", Address: "::ffff:192.0.2.1", Port: 4660},
	}
	got, err := parsePorts(strings.NewReader(text))
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("parsePorts = %+v, %v; want %+v", got, err, want)
	}
	got, err = parsePorts(strings.NewReader("sl local_address rem_address st\n0: ignored ignored 01\n"))
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("no listeners = %v, %v; want empty slice", got, err)
	}
}

func TestParsePortsErrors(t *testing.T) {
	for _, text := range []string{"0: missing", "0: bad remote 0A", "0: ZZZZZZZZ:0001 remote 0A", "0: 00000000:ZZZZ remote 0A", "0: 00000000:10000 remote 0A", strings.Repeat("x", 65537)} {
		if _, err := parsePorts(strings.NewReader(text)); err == nil {
			t.Errorf("accepted malformed line %.50q", text)
		}
	}
}
