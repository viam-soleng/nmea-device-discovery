package nmeadevicediscovery

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/data"
	"go.viam.com/test"
)

// fakeAllPgn stands in for the viamboat boat-all-pgn dependency, returning a
// canned history map shaped the way readings arrive after a gRPC hop: numbers
// as float64, timestamps as RFC822Z strings, keys "pgn-src".
type fakeAllPgn struct {
	sensor.Sensor
	readings map[string]interface{}
	gotExtra map[string]interface{}
}

func (f *fakeAllPgn) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	f.gotExtra = extra
	return f.readings, nil
}

func entry(pgn, src float64, ts string, fields map[string]interface{}) map[string]interface{} {
	m := map[string]interface{}{"pgn": pgn, "src": src, "timestamp": ts, "description": "x"}
	for k, v := range fields {
		m[k] = v
	}
	return m
}

func TestReadingsMergesIdentityPGNsPerDevice(t *testing.T) {
	fake := &fakeAllPgn{readings: map[string]interface{}{
		"60928-42": entry(60928, 42, "29 Jul 26 14:02 +0000", map[string]interface{}{
			"Unique Number": float64(12345), "Manufacturer Code": float64(999),
			"Device Function": float64(130), "Device Class": "Navigation",
		}),
		"126996-42": entry(126996, 42, "29 Jul 26 14:05 +0000", map[string]interface{}{
			"Model ID": "CR Box", "Product Code": float64(1234), "NMEA 2000 Version": 2.1,
		}),
		"126998-42": entry(126998, 42, "29 Jul 26 14:01 +0000", map[string]interface{}{
			"Installation Description #1": "helm",
		}),
	}}

	s := &nmeaDeviceDiscoveryDevicelist{allPgn: fake}
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)

	// Filter pushed down to the all-pgn sensor.
	test.That(t, fake.gotExtra["pgns"], test.ShouldResemble,
		[]interface{}{pgnAddressClaim, pgnProductInfo, pgnConfigInfo, pgnHeartbeat})

	// One device, labeled "<mfg> <model> [<unique number>]".
	test.That(t, len(got), test.ShouldEqual, 1)
	dev, ok := got["999 CR Box [12345]"].(map[string]interface{})
	test.That(t, ok, test.ShouldBeTrue)

	// Fields merged from all three PGNs, renamed to snake_case.
	test.That(t, dev["src"], test.ShouldEqual, 42)
	test.That(t, dev["unique_number"], test.ShouldEqual, float64(12345))
	test.That(t, dev["device_class"], test.ShouldEqual, "Navigation")
	test.That(t, dev["model_id"], test.ShouldEqual, "CR Box")
	test.That(t, dev["installation_description_1"], test.ShouldEqual, "helm")

	// last_seen is the newest of the three entries.
	test.That(t, dev["last_seen"], test.ShouldEqual, "29 Jul 26 14:05 +0000")
}

func TestReadingsDeduplicatesMovedDevice(t *testing.T) {
	// The same physical device (identity 12345-999) claimed address 42, then
	// moved to 43 after losing contention; the all-pgn history keeps both.
	fake := &fakeAllPgn{readings: map[string]interface{}{
		"60928-42": entry(60928, 42, "29 Jul 26 14:00 +0000", map[string]interface{}{
			"Unique Number": float64(12345), "Manufacturer Code": float64(999),
		}),
		"60928-43": entry(60928, 43, "29 Jul 26 14:10 +0000", map[string]interface{}{
			"Unique Number": float64(12345), "Manufacturer Code": float64(999),
		}),
	}}

	s := &nmeaDeviceDiscoveryDevicelist{allPgn: fake}
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)

	// One device, at its most recent address. No product info seen, so the
	// label is the manufacturer alone.
	test.That(t, len(got), test.ShouldEqual, 1)
	dev := got["999 [12345]"].(map[string]interface{})
	test.That(t, dev["src"], test.ShouldEqual, 43)
	test.That(t, dev["last_seen"], test.ShouldEqual, "29 Jul 26 14:10 +0000")
}

func TestReadingsKeyFormatsLargeUniqueNumber(t *testing.T) {
	// Unique numbers arrive as float64 over gRPC; values >= 1e6 render in
	// scientific notation under %v ("1.037921e+06-Garmin" seen on a real
	// Garmin device). Keys must normalize to integer form. Known manufacturer
	// codes decode to their lookup name and stay strings.
	fake := &fakeAllPgn{readings: map[string]interface{}{
		"60928-3": entry(60928, 3, "07 Aug 26 09:00 +0000", map[string]interface{}{
			"Unique Number": float64(1037921), "Manufacturer Code": "Garmin",
		}),
	}}

	s := &nmeaDeviceDiscoveryDevicelist{allPgn: fake}
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)

	for k := range got {
		test.That(t, strings.Contains(k, "e+"), test.ShouldBeFalse)
	}
	dev, ok := got["Garmin [1037921]"].(map[string]interface{})
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, dev["src"], test.ShouldEqual, 3)
}

func TestReadingsKeySanitizesModelStrings(t *testing.T) {
	// Real-network values: a Furuno MFD reports a NUL-terminated fixed-length
	// model_id with junk after the terminator, and Simrad pads with doubled
	// spaces. Keys must stay clean; the model_id FIELD keeps the raw value.
	fake := &fakeAllPgn{readings: map[string]interface{}{
		"60928-0": entry(60928, 0, "07 Aug 26 16:55 -0400", map[string]interface{}{
			"Unique Number": float64(51359), "Manufacturer Code": "Furuno",
		}),
		"126996-0": entry(126996, 0, "07 Aug 26 16:55 -0400", map[string]interface{}{
			"Model ID": "TZT14\x004 TZT",
		}),
		"60928-2": entry(60928, 2, "07 Aug 26 16:55 -0400", map[string]interface{}{
			"Unique Number": float64(1589637), "Manufacturer Code": "Simrad",
		}),
		"126996-2": entry(126996, 2, "07 Aug 26 16:55 -0400", map[string]interface{}{
			"Model ID": "NSS4-12  iGPS",
		}),
	}}

	s := &nmeaDeviceDiscoveryDevicelist{allPgn: fake}
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)

	furuno, ok := got["Furuno TZT14 [51359]"].(map[string]interface{})
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, furuno["model_id"], test.ShouldEqual, "TZT14\x004 TZT")

	_, ok = got["Simrad NSS4-12 iGPS [1589637]"]
	test.That(t, ok, test.ShouldBeTrue)
}

func TestReadingsDeviceWithoutClaimKeyedByAddress(t *testing.T) {
	// Product info overheard from a device whose address claim we never saw.
	fake := &fakeAllPgn{readings: map[string]interface{}{
		"126996-7": entry(126996, 7, "29 Jul 26 14:00 +0000", map[string]interface{}{
			"Model ID": "Mystery Box",
		}),
	}}

	s := &nmeaDeviceDiscoveryDevicelist{allPgn: fake}
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)

	// No address claim seen: the source address stands in for identity.
	dev, ok := got["Mystery Box [src-7]"].(map[string]interface{})
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, dev["model_id"], test.ShouldEqual, "Mystery Box")
}

func TestVirtualNodesWithSharedUniqueNumberAreDistinct(t *testing.T) {
	// A Garmin plotter exposes a Display node and a Button Interface node
	// under the same unique number, both live at once. Neither may hide the
	// other: they differ only by device function.
	fake := &fakeAllPgn{readings: map[string]interface{}{
		"60928-17": entry(60928, 17, "14 Oct 26 08:52 -0400", map[string]interface{}{
			"Unique Number": float64(474370), "Manufacturer Code": "Garmin", "Device Function": "Display",
		}),
		"126996-17": entry(126996, 17, "14 Oct 26 08:52 -0400", map[string]interface{}{"Model ID": "GPSMAP 9500"}),
		"60928-9": entry(60928, 9, "14 Oct 26 08:52 -0400", map[string]interface{}{
			"Unique Number": float64(474370), "Manufacturer Code": "Garmin", "Device Function": "Button Interface",
		}),
		"126996-9": entry(126996, 9, "14 Oct 26 08:52 -0400", map[string]interface{}{"Model ID": "Virtual N2K Input Handler"}),
	}}
	s := &nmeaDeviceDiscoveryDevicelist{allPgn: fake}
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)

	test.That(t, len(got), test.ShouldEqual, 2)
	display, ok := got["Garmin GPSMAP 9500 [474370]"].(map[string]interface{})
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, display["src"], test.ShouldEqual, 17)
	handler, ok := got["Garmin Virtual N2K Input Handler [474370]"].(map[string]interface{})
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, handler["src"], test.ShouldEqual, 9)
}

func TestCollidingLabelsGetFunctionQualifier(t *testing.T) {
	// Same product, same unique number, no product info yet: the two halves
	// of a dual gateway differ by device instance (always in the name), and
	// the two nodes of one half by function (added only on collision).
	claim := func(src, lower, upper float64, fn string) map[string]interface{} {
		return entry(60928, src, "14 Oct 26 08:52 -0400", map[string]interface{}{
			"Unique Number": float64(777), "Manufacturer Code": "Actisense", "Device Function": fn,
			"Device Instance Lower": lower, "Device Instance Upper": upper,
		})
	}
	fake := &fakeAllPgn{readings: map[string]interface{}{
		"60928-0": claim(0, 0, 0, "PC Gateway"),
		"60928-2": claim(2, 1, 0, "PC Gateway"),
		"60928-4": claim(4, 1, 0, "Display"),
		"60928-6": claim(6, 1, 0, "Display"),
	}}
	s := &nmeaDeviceDiscoveryDevicelist{allPgn: fake}
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)

	// Four claims, three distinct identities (4 and 6 are one device that
	// moved). Instance 0 needs no qualifier; the two instance-1 nodes are
	// told apart by function.
	test.That(t, len(got), test.ShouldEqual, 3)
	_, ok := got["Actisense [777]"]
	test.That(t, ok, test.ShouldBeTrue)
	_, ok = got["Actisense [777] #1 (PC Gateway)"]
	test.That(t, ok, test.ShouldBeTrue)
	display, ok := got["Actisense [777] #1 (Display)"].(map[string]interface{})
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, display["src"], test.ShouldEqual, 4) // tie on last_seen: lower address wins
}

func TestNameDoesNotDependOnOtherDevices(t *testing.T) {
	// After a viamboat restart devices reappear one at a time. A dual
	// gateway's second half must get the same name alone as it does once
	// its twin is back, or data capture sees one device under two names.
	half := func(src, instance float64) map[string]interface{} {
		return entry(60928, src, "14 Oct 26 08:52 -0400", map[string]interface{}{
			"Unique Number": float64(340545), "Manufacturer Code": "Actisense",
			"Device Function": "PC Gateway", "Device Instance Lower": instance,
		})
	}
	product := func(src float64) map[string]interface{} {
		return entry(126996, src, "14 Oct 26 08:52 -0400", map[string]interface{}{"Model ID": "NGX-1: Dual Gateway"})
	}
	const second = "Actisense NGX-1: Dual Gateway [340545] #1"

	alone := &fakeAllPgn{readings: map[string]interface{}{"60928-2": half(2, 1), "126996-2": product(2)}}
	got, err := (&nmeaDeviceDiscoveryDevicelist{allPgn: alone}).Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(got), test.ShouldEqual, 1)
	_, ok := got[second]
	test.That(t, ok, test.ShouldBeTrue)

	both := &fakeAllPgn{readings: map[string]interface{}{
		"60928-0": half(0, 0), "126996-0": product(0),
		"60928-2": half(2, 1), "126996-2": product(2),
	}}
	got, err = (&nmeaDeviceDiscoveryDevicelist{allPgn: both}).Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(got), test.ShouldEqual, 2)
	_, ok = got[second]
	test.That(t, ok, test.ShouldBeTrue)
	_, ok = got["Actisense NGX-1: Dual Gateway [340545]"]
	test.That(t, ok, test.ShouldBeTrue)
}

func TestMissingFunctionQualifierReadsUnknown(t *testing.T) {
	// Two nodes that collide on label and instance, one with a function the
	// decoder dropped: the qualifier must not render as "()".
	fake := &fakeAllPgn{readings: map[string]interface{}{
		"60928-5": entry(60928, 5, "14 Oct 26 08:52 -0400", map[string]interface{}{
			"Unique Number": float64(99), "Manufacturer Code": "Garmin", "Device Function": "Display",
		}),
		"60928-6": entry(60928, 6, "14 Oct 26 08:52 -0400", map[string]interface{}{
			"Unique Number": float64(99), "Manufacturer Code": "Garmin",
		}),
	}}
	got, err := (&nmeaDeviceDiscoveryDevicelist{allPgn: fake}).Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(got), test.ShouldEqual, 2)
	_, ok := got["Garmin [99] (Display)"]
	test.That(t, ok, test.ShouldBeTrue)
	_, ok = got["Garmin [99] (unknown)"]
	test.That(t, ok, test.ShouldBeTrue)
}

func TestTieOnLastSeenPrefersMoreCompleteAddress(t *testing.T) {
	// all-pgn timestamps have one-minute resolution, so a device that moved
	// can show the same last_seen at both addresses. The choice must not
	// depend on map order: the address with more identity data wins.
	fake := &fakeAllPgn{readings: map[string]interface{}{
		"60928-4": entry(60928, 4, "14 Oct 26 08:52 -0400", map[string]interface{}{
			"Unique Number": float64(555), "Manufacturer Code": "Furuno",
		}),
		"60928-6": entry(60928, 6, "14 Oct 26 08:52 -0400", map[string]interface{}{
			"Unique Number": float64(555), "Manufacturer Code": "Furuno",
		}),
		"126996-6": entry(126996, 6, "14 Oct 26 08:52 -0400", map[string]interface{}{"Model ID": "TZT14"}),
	}}
	s := &nmeaDeviceDiscoveryDevicelist{allPgn: fake}
	for i := 0; i < 20; i++ {
		got, err := s.Readings(context.Background(), nil)
		test.That(t, err, test.ShouldBeNil)
		test.That(t, len(got), test.ShouldEqual, 1)
		dev, ok := got["Furuno TZT14 [555]"].(map[string]interface{})
		test.That(t, ok, test.ShouldBeTrue)
		test.That(t, dev["src"], test.ShouldEqual, 6)
	}
}

func TestUnregisteredManufacturerGetsUnknownInIdentity(t *testing.T) {
	// A box claiming an unregistered manufacturer code (e.g. the NMEA2000
	// library's 2046 placeholder): the decoder drops the field entirely. Two
	// such boxes must stay distinct, and the key must not read "<nil>".
	claim := func(src, unique float64, fn string) map[string]interface{} {
		return entry(60928, src, "12 Oct 26 13:45 -0400", map[string]interface{}{
			"Unique Number": unique, "Device Function": fn, "Device Class": "Internetwork device",
		})
	}
	fake := &fakeAllPgn{readings: map[string]interface{}{
		"60928-80":  claim(80, 74565, "PC Gateway"),
		"126996-80": entry(126996, 80, "12 Oct 26 13:44 -0400", map[string]interface{}{"Model ID": "CR Alert Test", "Model Serial Code": "CR0001"}),
		"60928-81":  claim(81, 74566, "Ownship Position (Loran C)"),
	}}
	s := &nmeaDeviceDiscoveryDevicelist{allPgn: fake}
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)

	test.That(t, len(got), test.ShouldEqual, 2)
	_, ok := got["CR Alert Test [74565]"]
	test.That(t, ok, test.ShouldBeTrue)
	_, ok = got["74566"]
	test.That(t, ok, test.ShouldBeTrue)

	dev := map[string]interface{}{"unique_number": float64(74565), "device_function": "PC Gateway"}
	test.That(t, stableIdentity(dev), test.ShouldEqual, "74565-unknown-0-PC Gateway")
	test.That(t, stableIdentity(map[string]interface{}{"unique_number": float64(7)}), test.ShouldEqual, "7-unknown-0-unknown")
}

func TestHeartbeatKeepsLastSeenCurrent(t *testing.T) {
	// A plotter claimed its address when viamboat started three weeks ago
	// and has sent position every second since, but no identity PGN. Its
	// heartbeat, sent every minute, is what shows it is still on the bus.
	fake := &fakeAllPgn{readings: map[string]interface{}{
		"60928-3": entry(60928, 3, "24 Sep 26 16:13 -0400", map[string]interface{}{
			"Unique Number": float64(1037921), "Manufacturer Code": "Garmin", "Device Function": "Display",
		}),
		"126993-3": entry(126993, 3, "15 Oct 26 12:24 -0400", map[string]interface{}{
			"Data transmit offset": float64(60000), "Sequence Counter": float64(17),
		}),
		// Something at address 9 is alive but has never been identified:
		// not a device until its claim is seen.
		"126993-9": entry(126993, 9, "15 Oct 26 12:24 -0400", map[string]interface{}{
			"Data transmit offset": float64(60000), "Sequence Counter": float64(3),
		}),
	}}
	s := &nmeaDeviceDiscoveryDevicelist{allPgn: fake}
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)

	test.That(t, len(got), test.ShouldEqual, 1)
	dev, ok := got["Garmin [1037921]"].(map[string]interface{})
	test.That(t, ok, test.ShouldBeTrue)
	test.That(t, dev["first_seen"], test.ShouldEqual, "24 Sep 26 16:13 -0400")
	test.That(t, dev["last_seen"], test.ShouldEqual, "15 Oct 26 12:24 -0400")
	// The heartbeat's own fields are not part of the device.
	_, leaked := dev["sequence_counter"]
	test.That(t, leaked, test.ShouldBeFalse)
}

func TestEmptyInventory(t *testing.T) {
	// Only heartbeats, as right after a viamboat restart: no device yet.
	fake := &fakeAllPgn{readings: map[string]interface{}{
		"126993-3": entry(126993, 3, "15 Oct 26 17:21 -0400", map[string]interface{}{"Sequence Counter": float64(1)}),
	}}
	s := &nmeaDeviceDiscoveryDevicelist{allPgn: fake}

	// An empty map would reach viam-server as nil and be rejected, so a
	// direct caller gets an error that points at discover.
	_, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldEqual, errNoDevices)

	// Data capture is told there is nothing to store, which it skips quietly.
	_, err = s.Readings(context.Background(), map[string]interface{}{data.FromDMString: true})
	test.That(t, data.IsNoCaptureToStoreError(err), test.ShouldBeTrue)
}

// emptyAllPgn fails the way all-pgn does when nothing matches the filter: its
// empty map reaches viam-server as nil and is rejected there.
type emptyAllPgn struct{ sensor.Sensor }

func (emptyAllPgn) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	return nil, sensor.ErrReadingsNil("sensor", "all-pgn")
}

func TestAllPgnWithNothingMatchingIsAnEmptyHistory(t *testing.T) {
	// The first minute after a viamboat restart: no identity PGN and no
	// heartbeat yet.
	s := &nmeaDeviceDiscoveryDevicelist{allPgn: emptyAllPgn{}}
	_, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldEqual, errNoDevices)
	_, err = s.Readings(context.Background(), map[string]interface{}{data.FromDMString: true})
	test.That(t, data.IsNoCaptureToStoreError(err), test.ShouldBeTrue)

	// Remembered devices are still listed while the history is empty.
	s.state = newState()
	s.state.Devices["88213-Yamaha-0-Engine Gateway"] = persistedDevice{
		Fields:   map[string]interface{}{"unique_number": float64(88213), "manufacturer_code": "Yamaha", "model_id": "CL7 Gateway", "src": float64(50)},
		LastSeen: "14 Oct 26 08:52 -0400",
	}
	got, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldBeNil)
	_, ok := got["Yamaha CL7 Gateway [88213]"]
	test.That(t, ok, test.ShouldBeTrue)
}

func TestOtherAllPgnErrorsStillFail(t *testing.T) {
	s := &nmeaDeviceDiscoveryDevicelist{allPgn: failingAllPgn{}}
	_, err := s.Readings(context.Background(), nil)
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "Reading from all-pgn-component")
}

type failingAllPgn struct{ sensor.Sensor }

func (failingAllPgn) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	return nil, errors.New("reader not connected")
}
