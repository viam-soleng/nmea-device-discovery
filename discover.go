package nmeadevicediscovery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// The "discover" command makes devices that are already on the bus announce
// themselves. Devices send their identity PGNs only at power-up or when
// asked, so a device that claimed its address before viamboat started is
// otherwise never heard from again (except through Heartbeat, which carries
// no identity). discover asks every device at once with an ISO Request
// (PGN 59904) sent to the broadcast address, one request per identity PGN,
// through a viamboat sender component's generic DoCommand:
//
//	{"pgn": 59904, "dst": 255, "priority": 6, "fields": {"PGN": 60928}}
//
// The replies arrive on the bus within a second or two, land in the all-pgn
// history, and show up in the next reading; discover does not wait for them.
//
// The sender's own device cannot be refreshed this way: viamboat drops the
// frames it transmits itself, so the sender's replies never reach all-pgn.

const (
	pgnISORequest = 59904

	// isoRequestPriority is the priority J1939 and NMEA 2000 use for ISO
	// Requests.
	isoRequestPriority = 6
	// broadcastAddress requests a reply from every device on the bus.
	broadcastAddress = 255

	// discoverRequestGap spaces the requests of one discover call. The
	// address claims go first and are given a moment to settle (a claim can
	// move a device to a new address) before product and configuration info,
	// which are multi-frame replies from every device, are requested.
	discoverRequestGap = 500 * time.Millisecond

	// discoverMinInterval is the shortest time between two discover calls.
	// Every call makes every device on the bus reply, so a client polling
	// discover in a loop would flood the network.
	discoverMinInterval = 30 * time.Second
)

// discoverPGNs are the PGNs discover requests, in the order it sends them.
var discoverPGNs = []int{pgnAddressClaim, pgnProductInfo, pgnConfigInfo}

// discover implements the "discover" DoCommand. An optional "pgns" list
// limits the requests to a subset of discoverPGNs; they are always sent in
// discoverPGNs order.
func (s *nmeaDeviceDiscoveryDevicelist) discover(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	if s.sender == nil {
		return nil, errors.New(`discover needs "sender-component" set to a viamboat sender component`)
	}
	pgns, err := discoverRequestPGNs(cmd["pgns"])
	if err != nil {
		return nil, err
	}

	s.discoverMu.Lock()
	defer s.discoverMu.Unlock()
	if !s.lastDiscover.IsZero() {
		if wait := discoverMinInterval - time.Since(s.lastDiscover); wait > 0 {
			return nil, fmt.Errorf("discover was run %s ago; try again in %s",
				time.Since(s.lastDiscover).Round(time.Second), wait.Round(time.Second))
		}
	}

	var requested []interface{}
	for i, pgn := range pgns {
		if i > 0 && s.discoverGap > 0 {
			select {
			case <-ctx.Done():
				return nil, fmt.Errorf("discover interrupted after requesting %v: %w", requested, ctx.Err())
			case <-time.After(s.discoverGap):
			}
		}
		_, err := s.sender.DoCommand(ctx, map[string]interface{}{
			"pgn":      pgnISORequest,
			"dst":      broadcastAddress,
			"priority": isoRequestPriority,
			"fields":   map[string]interface{}{"PGN": pgn},
		})
		if err != nil {
			if len(requested) > 0 {
				return nil, fmt.Errorf("discover requested %v, then the request for PGN %d failed: %w", requested, pgn, err)
			}
			return nil, fmt.Errorf("discover: sending the request for PGN %d through %q: %w", pgn, s.senderName(), err)
		}
		// Rate-limit from the first request that reached the bus.
		s.lastDiscover = time.Now()
		requested = append(requested, pgn)
	}
	s.logger.Infow("discover: requested identity PGNs from every device", "pgns", requested, "sender", s.senderName())
	return map[string]interface{}{"requested": requested}, nil
}

// discoverRequestPGNs resolves the optional "pgns" argument of discover.
// Values arrive as float64 after a gRPC hop.
func discoverRequestPGNs(arg interface{}) ([]int, error) {
	if arg == nil {
		return discoverPGNs, nil
	}
	list, ok := arg.([]interface{})
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf(`discover "pgns" must be a non-empty list drawn from %v`, discoverPGNs)
	}
	want := map[int]bool{}
	for _, v := range list {
		n, ok := asInt(v)
		if !ok || !slices.Contains(discoverPGNs, n) {
			return nil, fmt.Errorf(`discover "pgns" may only contain %v, got %v`, discoverPGNs, v)
		}
		want[n] = true
	}
	var pgns []int
	for _, p := range discoverPGNs {
		if want[p] {
			pgns = append(pgns, p)
		}
	}
	return pgns, nil
}

// senderName is the configured sender-component, "" when there is none.
func (s *nmeaDeviceDiscoveryDevicelist) senderName() string {
	if s.cfg == nil {
		return ""
	}
	return s.cfg.SenderComponent
}
