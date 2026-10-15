package nmeadevicediscovery

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/test"
)

func boolPtr(b bool) *bool { return &b }

// newPersisting builds the sensor through NewDevicelist so it loads and
// saves state under VIAM_MODULE_DATA (pointed at a temp dir by the caller).
func newPersisting(t *testing.T, fake sensor.Sensor, cfg *Config) sensor.Sensor {
	t.Helper()
	deps := resource.Dependencies{sensor.Named("all-pgn"): fake}
	if cfg == nil {
		cfg = &Config{}
	}
	cfg.AllPGNComponent = "all-pgn"
	s, err := NewDevicelist(context.Background(), deps, sensor.Named("devicelist"), cfg, logging.NewTestLogger(t))
	test.That(t, err, test.ShouldBeNil)
	return s
}

func device(t *testing.T, got map[string]interface{}, key string) map[string]interface{} {
	t.Helper()
	dev, ok := got[key].(map[string]interface{})
	if !ok {
		keys := make([]string, 0, len(got))
		for k := range got {
			keys = append(keys, k)
		}
		t.Fatalf("no device %q in %v", key, keys)
	}
	return dev
}

// gatewayReadings is an engine gateway whose address claim was logged at
// 07:00 and whose product and configuration info arrived later.
func gatewayReadings() map[string]interface{} {
	return map[string]interface{}{
		"60928-50": entry(60928, 50, "14 Oct 26 07:00 -0400", map[string]interface{}{
			"Unique Number": float64(88213), "Manufacturer Code": "Yamaha",
			"Device Function": "Engine Gateway", "Device Class": "Propulsion",
		}),
		"126996-50": entry(126996, 50, "14 Oct 26 08:52 -0400", map[string]interface{}{"Model ID": "CL7 Gateway"}),
		"126998-50": entry(126998, 50, "14 Oct 26 08:00 -0400", map[string]interface{}{"Installation Description #1": "port engine room"}),
	}
}

func TestFirstSeenIsEarliestBusTimestamp(t *testing.T) {
	s := &nmeaDeviceDiscoveryDevicelist{allPgn: &fakeAllPgn{readings: gatewayReadings()}}
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)

	// Claim at 07:00, product info at 08:52: the window is the whole span.
	dev := device(t, got, "Yamaha CL7 Gateway [88213]")
	test.That(t, dev["first_seen"], test.ShouldEqual, "14 Oct 26 07:00 -0400")
	test.That(t, dev["last_seen"], test.ShouldEqual, "14 Oct 26 08:52 -0400")
}

func TestStateSurvivesRestartAndKeepsOfflineDevices(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIAM_MODULE_DATA", dir)
	statePath := filepath.Join(dir, "devicelist-devices.json")

	// Day 1: the gateway is on the bus.
	s := newPersisting(t, &fakeAllPgn{readings: gatewayReadings()}, nil)
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, device(t, got, "Yamaha CL7 Gateway [88213]")["first_seen"], test.ShouldEqual, "14 Oct 26 07:00 -0400")
	_, err = os.Stat(statePath)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, s.Close(context.Background()), test.ShouldBeNil)

	// Day 2: viamboat restarted, the gateway is unplugged, a new depth sensor
	// claims an address. The all-pgn history knows nothing about the gateway.
	day2 := map[string]interface{}{
		"60928-35": entry(60928, 35, "15 Oct 26 09:00 -0400", map[string]interface{}{
			"Unique Number": float64(780712), "Manufacturer Code": "Airmar", "Device Function": "Depth / Speed / Temp",
		}),
	}
	s = newPersisting(t, &fakeAllPgn{readings: day2}, nil)
	got, err = s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(got), test.ShouldEqual, 2)

	// The gateway is still listed, unchanged.
	gw := device(t, got, "Yamaha CL7 Gateway [88213]")
	test.That(t, gw["first_seen"], test.ShouldEqual, "14 Oct 26 07:00 -0400")
	test.That(t, gw["last_seen"], test.ShouldEqual, "14 Oct 26 08:52 -0400")
	test.That(t, gw["model_id"], test.ShouldEqual, "CL7 Gateway")
	test.That(t, gw["installation_description_1"], test.ShouldEqual, "port engine room")
	test.That(t, gw["src"], test.ShouldEqual, 50)

	// The new device gets its own bus timestamp as first_seen.
	test.That(t, device(t, got, "Airmar [780712]")["first_seen"], test.ShouldEqual, "15 Oct 26 09:00 -0400")
	test.That(t, s.Close(context.Background()), test.ShouldBeNil)

	// Day 3: gateway back at a different address, product info not yet
	// re-broadcast. first_seen holds, last_seen and src move, and the
	// file's product info fills in.
	day3 := map[string]interface{}{
		"60928-51": entry(60928, 51, "16 Oct 26 10:00 -0400", map[string]interface{}{
			"Unique Number": float64(88213), "Manufacturer Code": "Yamaha",
			"Device Function": "Engine Gateway", "Device Class": "Propulsion",
		}),
	}
	s = newPersisting(t, &fakeAllPgn{readings: day3}, nil)
	got, err = s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	gw = device(t, got, "Yamaha CL7 Gateway [88213]")
	test.That(t, gw["first_seen"], test.ShouldEqual, "14 Oct 26 07:00 -0400")
	test.That(t, gw["last_seen"], test.ShouldEqual, "16 Oct 26 10:00 -0400")
	test.That(t, gw["src"], test.ShouldEqual, 51)
	test.That(t, gw["model_id"], test.ShouldEqual, "CL7 Gateway")
}

func TestStateSavesOnlyWhenChanged(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIAM_MODULE_DATA", dir)
	statePath := filepath.Join(dir, "devicelist-devices.json")

	s := newPersisting(t, &fakeAllPgn{readings: gatewayReadings()}, nil)
	_, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	before, err := os.Stat(statePath)
	test.That(t, err, test.ShouldBeNil)

	// Same history again: nothing changed, the file is not rewritten.
	_, err = s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	after, err := os.Stat(statePath)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, after.ModTime(), test.ShouldEqual, before.ModTime())

	// And the file is keyed by identity, not label.
	var st persistedState
	b, err := os.ReadFile(statePath)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, json.Unmarshal(b, &st), test.ShouldBeNil)
	_, ok := st.Devices["88213-Yamaha-0-Engine Gateway"]
	test.That(t, ok, test.ShouldBeTrue)
}

func TestLabelChangeKeepsFirstSeen(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIAM_MODULE_DATA", dir)

	// First sight: address claim only, labeled by manufacturer.
	claimOnly := map[string]interface{}{
		"60928-3": entry(60928, 3, "14 Oct 26 09:00 -0400", map[string]interface{}{
			"Unique Number": float64(1037921), "Manufacturer Code": "Garmin", "Device Function": "Display",
		}),
	}
	s := newPersisting(t, &fakeAllPgn{readings: claimOnly}, nil)
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, device(t, got, "Garmin [1037921]")["first_seen"], test.ShouldEqual, "14 Oct 26 09:00 -0400")

	// Product info arrives: the label changes, the record does not restart.
	withProduct := map[string]interface{}{
		"60928-3":  claimOnly["60928-3"],
		"126996-3": entry(126996, 3, "14 Oct 26 09:30 -0400", map[string]interface{}{"Model ID": "GPSMAP 8612"}),
	}
	s = newPersisting(t, &fakeAllPgn{readings: withProduct}, nil)
	got, err = s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(got), test.ShouldEqual, 1)
	dev := device(t, got, "Garmin GPSMAP 8612 [1037921]")
	test.That(t, dev["first_seen"], test.ShouldEqual, "14 Oct 26 09:00 -0400")
	test.That(t, dev["last_seen"], test.ShouldEqual, "14 Oct 26 09:30 -0400")
}

func TestClaimlessSourceIsNotPersisted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIAM_MODULE_DATA", dir)

	s := newPersisting(t, &fakeAllPgn{readings: map[string]interface{}{
		"126996-7": entry(126996, 7, "14 Oct 26 08:52 -0400", map[string]interface{}{"Model ID": "Mystery Box"}),
	}}, nil)
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, device(t, got, "Mystery Box [src-7]")["first_seen"], test.ShouldEqual, "14 Oct 26 08:52 -0400")

	// Reported, but a bus address is not an identity: nothing written.
	_, err = os.Stat(filepath.Join(dir, "devicelist-devices.json"))
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
}

func TestPersistOffWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIAM_MODULE_DATA", dir)

	s := newPersisting(t, &fakeAllPgn{readings: gatewayReadings()}, &Config{Persist: boolPtr(false)})
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, device(t, got, "Yamaha CL7 Gateway [88213]")["first_seen"], test.ShouldEqual, "14 Oct 26 07:00 -0400")
	entries, err := os.ReadDir(dir)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(entries), test.ShouldEqual, 0)
}

func TestForgetAndReset(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VIAM_MODULE_DATA", dir)
	statePath := filepath.Join(dir, "devicelist-devices.json")

	s := newPersisting(t, &fakeAllPgn{readings: gatewayReadings()}, nil)
	_, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)

	// Unplugged: the bus is empty, the gateway is still remembered.
	s = newPersisting(t, &fakeAllPgn{readings: map[string]interface{}{}}, nil)
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(got), test.ShouldEqual, 1)

	status, err := s.DoCommand(context.Background(), map[string]interface{}{"command": "status"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, status["remembered_devices"], test.ShouldEqual, 1)
	test.That(t, status["persist_path"], test.ShouldEqual, statePath)

	// Forget by reading key.
	res, err := s.DoCommand(context.Background(), map[string]interface{}{"command": "forget", "device": "Yamaha CL7 Gateway [88213]"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, res["forgotten"], test.ShouldEqual, "88213-Yamaha-0-Engine Gateway")
	_, err = s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldEqual, errNoDevices)

	// Unknown key is an error, not a silent no-op.
	_, err = s.DoCommand(context.Background(), map[string]interface{}{"command": "forget", "device": "nope"})
	test.That(t, err, test.ShouldNotBeNil)

	// Reset removes the file; a restart starts empty.
	s = newPersisting(t, &fakeAllPgn{readings: gatewayReadings()}, nil)
	_, err = s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	res, err = s.DoCommand(context.Background(), map[string]interface{}{"command": "reset"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, res["reset"], test.ShouldEqual, true)
	_, err = os.Stat(statePath)
	test.That(t, os.IsNotExist(err), test.ShouldBeTrue)
	s = newPersisting(t, &fakeAllPgn{readings: map[string]interface{}{}}, nil)
	_, err = s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldEqual, errNoDevices)
}

func TestStatusMatchesStatusCommand(t *testing.T) {
	cfg := &Config{AllPGNComponent: "all-pgn", SenderComponent: "sender", Persist: boolPtr(false)}
	s := &nmeaDeviceDiscoveryDevicelist{allPgn: &fakeAllPgn{readings: map[string]interface{}{}}, cfg: cfg, state: newState()}

	got, err := s.Status(context.Background())
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got, test.ShouldResemble, map[string]interface{}{
		"all_pgn_component":  "all-pgn",
		"persist":            false,
		"persist_path":       "",
		"remembered_devices": 0,
		"sender_component":   "sender",
	})

	cmd, err := s.DoCommand(context.Background(), map[string]interface{}{"command": "status"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got, test.ShouldResemble, cmd)
}
