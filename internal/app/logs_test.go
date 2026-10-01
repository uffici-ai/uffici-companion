package app

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/uffici-ai/uffici-companion/internal/contracts"
	"github.com/uffici-ai/uffici-companion/internal/infra/memory"
	wstransport "github.com/uffici-ai/uffici-companion/internal/infra/transport/websocket"
)

func TestLoadConfigReadsTheMachineLogSwitches(t *testing.T) {
	home := shortHome(t)
	config, err := LoadConfig(env(map[string]string{"UFFICI_HOME": home}), Config{})
	if err != nil || !config.LogShipping || config.HarnessLogLevel != "warn" {
		t.Fatalf("defaults: %+v %v", config, err)
	}
	config, err = LoadConfig(env(map[string]string{
		"UFFICI_HOME": home, "UFFICI_LOG_SHIPPING": "OFF", "UFFICI_LOG_LEVEL_HARNESS": "Error",
	}), Config{})
	if err != nil || config.LogShipping || config.HarnessLogLevel != "error" {
		t.Fatalf("off/error: %+v %v", config, err)
	}
	for name, environment := range map[string]map[string]string{
		"an unknown switch":        {"UFFICI_HOME": home, "UFFICI_LOG_SHIPPING": "maybe"},
		"an unknown harness level": {"UFFICI_HOME": home, "UFFICI_LOG_LEVEL_HARNESS": "trace"},
	} {
		if _, err := LoadConfig(env(environment), Config{}); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// 🔴 The machine switch OFF: no shipper, so no logs ticket is ever minted, and
// the tee captures nothing at all.
func TestTheMachineSwitchOffMintsNothingAndCapturesNothing(t *testing.T) {
	host := memory.New()
	logging := newLogging(Config{LogShipping: false, ShipLevel: slog.LevelDebug}, &bytes.Buffer{}, "tok")
	if shipper := newShipper(Config{LogShipping: false}, host, logging, nil); shipper != nil {
		t.Fatal("a shipper was built with the machine switch off")
	}
	logging.logger.Error("would ship")
	if logging.ring.Len() != 0 {
		t.Fatal("the tee captured with the machine switch off")
	}
	if host.Mints() != 0 {
		t.Fatal("a logs ticket was minted")
	}
}

func TestTheMachineSwitchOnShipsWithTheMachinesFloor(t *testing.T) {
	host := memory.New()
	config := Config{LogShipping: true, ShipLevel: slog.LevelInfo, HarnessLogLevel: "error"}
	logging := newLogging(config, &bytes.Buffer{}, "tok")
	shipper := newShipper(config, host, logging, func() contracts.LogClient { return contracts.LogClient{Name: "x"} })
	if shipper == nil {
		t.Fatal("no shipper with the switch on and a host that takes logs")
	}
	logging.logger.Info("kept for shipping")
	if logging.ring.Len() != 1 {
		t.Fatalf("the tee did not capture: %d", logging.ring.Len())
	}
	shipper.Tick(context.Background())
	if harness, plugin := shipper.Floors(); harness != "error" || plugin != "info" {
		t.Fatalf("floors %s/%s", harness, plugin)
	}
	// Switched off in Uffici: capture stops on the next record.
	host.SetShipping(false, "warn")
	shipper.Flush(context.Background())
	logging.logger.Error("not kept")
	if logging.ring.Len() != 0 {
		t.Fatalf("captured after Uffici switched shipping off: %d", logging.ring.Len())
	}
}

// A host that takes no logs gets no shipper, whatever the switch says.
func TestAHostWithoutLogsGetsNoShipper(t *testing.T) {
	logging := newLogging(Config{LogShipping: true}, &bytes.Buffer{}, "tok")
	if newShipper(Config{LogShipping: true}, noLogs{}, logging, nil) != nil {
		t.Fatal("a shipper for a host that takes no logs")
	}
}

type noLogs struct{ contracts.AgentsHost }

func TestTheDoctorDescribesLogShipping(t *testing.T) {
	for _, tc := range []struct {
		health wstransport.HealthLogs
		want   string
	}{
		{wstransport.HealthLogs{Machine: "off"}, "switched off on this machine"},
		{wstransport.HealthLogs{Machine: "on", Uffici: "off"}, "switched off for this agent in Uffici"},
		{wstransport.HealthLogs{Machine: "on", Uffici: "on", HarnessFloor: "warn", Dropped: 3}, "harness lines at warn and above; 3 dropped"},
	} {
		if got := describeLogs(tc.health); !strings.Contains(got, tc.want) {
			t.Errorf("%+v → %q, want %q", tc.health, got, tc.want)
		}
	}
	config := Config{LogShipping: true, HarnessLogLevel: "info"}
	logging := newLogging(config, &bytes.Buffer{}, "tok")
	if got := logsHealth(Config{LogShipping: false, HarnessLogLevel: "warn"}, nil, logging.ring); got.Machine != "off" {
		t.Fatalf("machine off: %+v", got)
	}
	host := memory.New()
	host.SetShipping(true, "error")
	shipper := newShipper(config, host, logging, nil)
	shipper.Tick(context.Background())
	if got := logsHealth(config, shipper, logging.ring); got.Machine != "on" || got.Uffici != "on" || got.HarnessFloor != "error" {
		t.Fatalf("on: %+v", got)
	}
}
