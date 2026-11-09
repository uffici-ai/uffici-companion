package app

import "testing"

// 🔴 The wire's only authentication is a local token, carried in the plugin's
// hello: `--tcp` on a non-loopback address would serve it, and an
// unauthenticated /health, on every interface of the machine.
func TestLoadConfigRefusesANonLoopbackTCPAddress(t *testing.T) {
	home := shortHome(t)
	for _, addr := range []string{"0.0.0.0:7411", "[::]:7411", ":7411", "192.0.2.10:7411", "example-host:7411", "7411"} {
		t.Run(addr, func(t *testing.T) {
			_, err := LoadConfig(env(map[string]string{"UFFICI_HOME": home}), Config{TCPAddr: addr})
			if err == nil {
				t.Fatalf("--tcp %s was accepted", addr)
			}
		})
	}
	for _, addr := range []string{"127.0.0.1:7411", "[::1]:7411", "localhost:7411", "LOCALHOST:7411"} {
		t.Run(addr, func(t *testing.T) {
			if _, err := LoadConfig(env(map[string]string{"UFFICI_HOME": home}), Config{TCPAddr: addr}); err != nil {
				t.Fatalf("--tcp %s was refused: %v", addr, err)
			}
		})
	}
}
