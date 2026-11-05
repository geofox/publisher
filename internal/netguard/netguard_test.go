package netguard

import (
	"context"
	"net"
	"testing"
)

type lookup map[string][]string

func (l lookup) LookupIPAddr(_ context.Context, host string) ([]net.IPAddr, error) {
	var out []net.IPAddr
	for _, a := range l[host] {
		out = append(out, net.IPAddr{IP: net.ParseIP(a)})
	}
	return out, nil
}

func TestCheckPublicURL(t *testing.T) {
	r := lookup{"pub.example": {"93.184.216.34"}, "mixed.example": {"93.184.216.34", "fd00::1"}}
	for raw, ok := range map[string]bool{
		"wss://pub.example/x": true, "wss://93.184.216.34": true,
		"ws://pub.example": false, "wss://mixed.example": false, "wss://empty.example": false,
		"wss://10.1.2.3": false, "wss://100.64.0.1": false, "wss://[fe80::1]": false, "wss://224.0.0.1": false,
		"wss://localhost": false, "wss://db.internal": false, "wss://printer.local": false, "wss://redis": false,
		"wss://": false, "::bad": false,
	} {
		if err := CheckPublicURL(context.Background(), r, raw, "wss"); (err == nil) != ok {
			t.Errorf("%s: err = %v, want ok=%v", raw, err, ok)
		}
	}
}
