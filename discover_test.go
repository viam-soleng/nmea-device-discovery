package nmeadevicediscovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	"go.viam.com/test"
)

// fakeSender stands in for the viamboat sender generic component, recording
// every DoCommand it is asked to send. failOn makes the request for that PGN
// fail.
type fakeSender struct {
	resource.Resource
	sent   []map[string]interface{}
	failOn int
}

func (f *fakeSender) Name() resource.Name { return generic.Named("sender") }

func (f *fakeSender) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if fields, ok := cmd["fields"].(map[string]interface{}); ok && fields["PGN"] == f.failOn {
		return nil, errors.New("address not yet claimed")
	}
	f.sent = append(f.sent, cmd)
	return map[string]interface{}{"sent": true}, nil
}

// requestedPGNs lists the PGN each recorded ISO Request asked for.
func (f *fakeSender) requestedPGNs() []interface{} {
	var out []interface{}
	for _, c := range f.sent {
		out = append(out, c["fields"].(map[string]interface{})["PGN"])
	}
	return out
}

func newDiscovering(t *testing.T, sender *fakeSender) *nmeaDeviceDiscoveryDevicelist {
	t.Helper()
	return &nmeaDeviceDiscoveryDevicelist{
		allPgn: &fakeAllPgn{readings: map[string]interface{}{}},
		cfg:    &Config{AllPGNComponent: "all-pgn", SenderComponent: "sender"},
		sender: sender,
		logger: logging.NewTestLogger(t),
	}
}

func TestDiscoverBroadcastsISORequests(t *testing.T) {
	sender := &fakeSender{}
	s := newDiscovering(t, sender)

	got, err := s.DoCommand(context.Background(), map[string]interface{}{"command": "discover"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got["requested"], test.ShouldResemble, []interface{}{60928, 126996, 126998})

	// One ISO Request per identity PGN, address claims first, each broadcast
	// in the shape the sender's generic DoCommand path turns into a frame.
	test.That(t, sender.requestedPGNs(), test.ShouldResemble, []interface{}{60928, 126996, 126998})
	test.That(t, sender.sent[0], test.ShouldResemble, map[string]interface{}{
		"pgn": 59904, "dst": 255, "priority": 6, "fields": map[string]interface{}{"PGN": 60928},
	})
}

func TestDiscoverSubsetIsSentInOrder(t *testing.T) {
	sender := &fakeSender{}
	s := newDiscovering(t, sender)

	// Numbers arrive as float64 after a gRPC hop.
	got, err := s.DoCommand(context.Background(), map[string]interface{}{
		"command": "discover", "pgns": []interface{}{float64(126996), float64(60928)},
	})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, got["requested"], test.ShouldResemble, []interface{}{60928, 126996})
	test.That(t, sender.requestedPGNs(), test.ShouldResemble, []interface{}{60928, 126996})
}

func TestDiscoverRejectsOtherPGNs(t *testing.T) {
	sender := &fakeSender{}
	s := newDiscovering(t, sender)

	for _, pgns := range []interface{}{
		[]interface{}{float64(129025)},
		[]interface{}{},
		"60928",
	} {
		_, err := s.DoCommand(context.Background(), map[string]interface{}{"command": "discover", "pgns": pgns})
		test.That(t, err, test.ShouldNotBeNil)
	}
	test.That(t, sender.sent, test.ShouldBeEmpty)
}

func TestDiscoverNeedsSender(t *testing.T) {
	s := newDiscovering(t, nil)
	s.sender = nil

	_, err := s.DoCommand(context.Background(), map[string]interface{}{"command": "discover"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "sender-component")
}

func TestDiscoverIsRateLimited(t *testing.T) {
	sender := &fakeSender{}
	s := newDiscovering(t, sender)
	ctx := context.Background()

	_, err := s.DoCommand(ctx, map[string]interface{}{"command": "discover"})
	test.That(t, err, test.ShouldBeNil)

	// Every call makes every device reply; a second call right away is refused
	// without touching the bus.
	_, err = s.DoCommand(ctx, map[string]interface{}{"command": "discover"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "try again")
	test.That(t, len(sender.sent), test.ShouldEqual, 3)

	s.lastDiscover = time.Now().Add(-discoverMinInterval - time.Second)
	_, err = s.DoCommand(ctx, map[string]interface{}{"command": "discover"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, len(sender.sent), test.ShouldEqual, 6)
}

func TestDiscoverReportsPartialFailure(t *testing.T) {
	sender := &fakeSender{failOn: 126996}
	s := newDiscovering(t, sender)

	_, err := s.DoCommand(context.Background(), map[string]interface{}{"command": "discover"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, err.Error(), test.ShouldContainSubstring, "[60928]")
	test.That(t, err.Error(), test.ShouldContainSubstring, "126996")
	// The address-claim request reached the bus, so the rate limit applies.
	test.That(t, s.lastDiscover.IsZero(), test.ShouldBeFalse)

	// Failing before anything is sent leaves the rate limit untouched, so the
	// caller can retry once the sender is ready.
	sender = &fakeSender{failOn: 60928}
	s = newDiscovering(t, sender)
	_, err = s.DoCommand(context.Background(), map[string]interface{}{"command": "discover"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, s.lastDiscover.IsZero(), test.ShouldBeTrue)
}

func TestDiscoverStopsWhenContextEnds(t *testing.T) {
	sender := &fakeSender{}
	s := newDiscovering(t, sender)
	s.discoverGap = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.DoCommand(ctx, map[string]interface{}{"command": "discover"})
	test.That(t, err, test.ShouldNotBeNil)
	test.That(t, errors.Is(err, context.DeadlineExceeded), test.ShouldBeTrue)
	test.That(t, sender.requestedPGNs(), test.ShouldResemble, []interface{}{60928})
}

func TestSenderComponentIsADependency(t *testing.T) {
	deps, _, err := (&Config{AllPGNComponent: "all-pgn"}).Validate("path")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"all-pgn"})

	deps, _, err = (&Config{AllPGNComponent: "all-pgn", SenderComponent: "sender"}).Validate("path")
	test.That(t, err, test.ShouldBeNil)
	test.That(t, deps, test.ShouldResemble, []string{"all-pgn", "sender"})

	// Constructed through NewDevicelist, the sender is resolved from the
	// dependencies and reported by status.
	sender := &fakeSender{}
	resDeps := resource.Dependencies{
		sensor.Named("all-pgn"): &fakeAllPgn{readings: map[string]interface{}{}},
		generic.Named("sender"): sender,
	}
	cfg := &Config{AllPGNComponent: "all-pgn", SenderComponent: "sender", Persist: boolPtr(false)}
	s, err := NewDevicelist(context.Background(), resDeps, sensor.Named("devicelist"), cfg, logging.NewTestLogger(t))
	test.That(t, err, test.ShouldBeNil)
	status, err := s.DoCommand(context.Background(), map[string]interface{}{"command": "status"})
	test.That(t, err, test.ShouldBeNil)
	test.That(t, status["sender_component"], test.ShouldEqual, "sender")

	// A sender-component that is not among the dependencies fails construction.
	_, err = NewDevicelist(context.Background(), resource.Dependencies{
		sensor.Named("all-pgn"): &fakeAllPgn{readings: map[string]interface{}{}},
	}, sensor.Named("devicelist"), cfg, logging.NewTestLogger(t))
	test.That(t, err, test.ShouldNotBeNil)
}
