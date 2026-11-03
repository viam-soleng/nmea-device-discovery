package nmeadevicediscovery

import (
	"context"
	"math"
	"testing"

	"go.viam.com/rdk/data"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/test"
)

// capture calls Readings the way data capture does and reports whether the
// reading would be stored.
func capture(t *testing.T, s *nmeaDeviceDiscoveryDevicelist) bool {
	t.Helper()
	got, err := s.Readings(context.Background(), map[string]interface{}{data.FromDMString: true})
	if data.IsNoCaptureToStoreError(err) {
		return false
	}
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got, test.ShouldNotBeEmpty)
	return true
}

func garminClaim(src float64, ts string) map[string]interface{} {
	return entry(60928, src, ts, map[string]interface{}{
		"Unique Number": float64(1037921), "Manufacturer Code": "Garmin", "Device Function": "Display",
	})
}

func garminHeartbeat(src float64, ts string) map[string]interface{} {
	return entry(126993, src, ts, map[string]interface{}{"Sequence Counter": float64(1)})
}

func TestCaptureStoresEveryReadingByDefault(t *testing.T) {
	fake := &fakeAllPgn{readings: map[string]interface{}{"60928-3": garminClaim(3, "15 Oct 26 12:00 -0400")}}
	s := &nmeaDeviceDiscoveryDevicelist{allPgn: fake, cfg: &Config{AllPGNComponent: "all-pgn"}}
	test.That(t, capture(t, s), test.ShouldBeTrue)
	test.That(t, capture(t, s), test.ShouldBeTrue)

	st, err := s.DoCommand(context.Background(), map[string]interface{}{"command": "status"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, st["capture_changes_only"], test.ShouldBeFalse)
	test.That(t, st, test.ShouldNotContainKey, "last_capture_stored")
}

func TestCaptureChangesOnly(t *testing.T) {
	fake := &fakeAllPgn{readings: map[string]interface{}{
		"60928-3":  garminClaim(3, "15 Oct 26 12:00 -0400"),
		"126993-3": garminHeartbeat(3, "15 Oct 26 12:00 -0400"),
	}}
	s := &nmeaDeviceDiscoveryDevicelist{allPgn: fake, cfg: &Config{AllPGNComponent: "all-pgn", CaptureChangesOnly: true}}
	test.That(t, capture(t, s), test.ShouldBeTrue) // first capture is always stored
	test.That(t, capture(t, s), test.ShouldBeFalse)

	// Heartbeat moves last_seen only.
	fake.readings["126993-3"] = garminHeartbeat(3, "15 Oct 26 12:05 -0400")
	test.That(t, capture(t, s), test.ShouldBeFalse)

	// Callers other than data capture always get the reading.
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, device(t, got, "Garmin [1037921]")["last_seen"], test.ShouldEqual, "15 Oct 26 12:05 -0400")

	// Product info arrives: the label and fields change, once.
	fake.readings["126996-3"] = entry(126996, 3, "15 Oct 26 12:06 -0400", map[string]interface{}{"Model ID": "GPSMAP 8612"})
	test.That(t, capture(t, s), test.ShouldBeTrue)
	test.That(t, capture(t, s), test.ShouldBeFalse)

	// The device moves address: src changes.
	fake.readings = map[string]interface{}{
		"60928-4":  garminClaim(4, "15 Oct 26 12:10 -0400"),
		"126996-4": entry(126996, 4, "15 Oct 26 12:10 -0400", map[string]interface{}{"Model ID": "GPSMAP 8612"}),
	}
	test.That(t, capture(t, s), test.ShouldBeTrue)

	// viamboat restarts with an empty history: the remembered device is
	// still reported, unchanged.
	fake.readings = map[string]interface{}{}
	test.That(t, capture(t, s), test.ShouldBeFalse)

	// A second device appears.
	fake.readings["60928-42"] = entry(60928, 42, "15 Oct 26 12:20 -0400", map[string]interface{}{
		"Unique Number": float64(12345), "Manufacturer Code": float64(999),
	})
	test.That(t, capture(t, s), test.ShouldBeTrue)
	test.That(t, capture(t, s), test.ShouldBeFalse)

	st, err := s.DoCommand(context.Background(), map[string]interface{}{"command": "status"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, st["capture_changes_only"], test.ShouldBeTrue)
	test.That(t, st["last_capture_stored"], test.ShouldNotEqual, "")
}

func TestCaptureKeyIgnoresNumericShape(t *testing.T) {
	// A field read live (int in-process) and from the state file (float64)
	// is the same reading.
	a := map[string]interface{}{"x [1]": map[string]interface{}{"src": 3, "unique_number": 1, "last_seen": "a"}}
	b := map[string]interface{}{"x [1]": map[string]interface{}{"src": float64(3), "unique_number": float64(1), "first_seen": "b"}}
	keyA, errA := captureKey(a)
	keyB, errB := captureKey(b)
	test.That(t, errA, test.ShouldBeNil)
	test.That(t, errB, test.ShouldBeNil)
	test.That(t, keyA, test.ShouldEqual, keyB)
	test.That(t, keyA, test.ShouldNotEqual, "")
}

func TestCaptureKeyFailureStoresEveryCaptureAndSaysSo(t *testing.T) {
	// One device reports a value encoding/json rejects.
	nanInfo := func() map[string]interface{} {
		return entry(126996, 3, "15 Oct 26 12:00 -0400", map[string]interface{}{"Model ID": "GPSMAP 8612", "Load Equivalency": math.NaN()})
	}
	fake := &fakeAllPgn{readings: map[string]interface{}{
		"60928-3":  garminClaim(3, "15 Oct 26 12:00 -0400"),
		"126996-3": nanInfo(),
	}}
	logger, logs := logging.NewObservedTestLogger(t)
	s := &nmeaDeviceDiscoveryDevicelist{allPgn: fake, logger: logger, cfg: &Config{AllPGNComponent: "all-pgn", CaptureChangesOnly: true}}

	// Every capture is stored, but the warning is logged once.
	for i := 0; i < 3; i++ {
		test.That(t, capture(t, s), test.ShouldBeTrue)
	}
	test.That(t, logs.FilterMessageSnippet("cannot compute inventory key").Len(), test.ShouldEqual, 1)
	st, err := s.DoCommand(context.Background(), map[string]interface{}{"command": "status"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, st["capture_key_error"], test.ShouldContainSubstring, "NaN")

	// The value goes away: the change is stored once, then duplicates are
	// skipped again, and the recovery is logged.
	fake.readings["126996-3"] = entry(126996, 3, "15 Oct 26 12:01 -0400", map[string]interface{}{"Model ID": "GPSMAP 8612", "Load Equivalency": float64(1)})
	test.That(t, capture(t, s), test.ShouldBeTrue)
	test.That(t, capture(t, s), test.ShouldBeFalse)
	test.That(t, logs.FilterMessageSnippet("inventory key computed again").Len(), test.ShouldEqual, 1)
	st, err = s.DoCommand(context.Background(), map[string]interface{}{"command": "status"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, st, test.ShouldNotContainKey, "capture_key_error")

	// A recurrence warns again.
	fake.readings["126996-3"] = nanInfo()
	test.That(t, capture(t, s), test.ShouldBeTrue)
	test.That(t, logs.FilterMessageSnippet("cannot compute inventory key").Len(), test.ShouldEqual, 2)
}

func TestCaptureChangesOnlyAttributeName(t *testing.T) {
	// The attribute follows this module's hyphenated naming.
	cfg, err := resource.TransformAttributeMap[*Config](map[string]interface{}{
		"all-pgn-component": "all-pgn", "capture-changes-only": true,
	})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, cfg.CaptureChangesOnly, test.ShouldBeTrue)
}
