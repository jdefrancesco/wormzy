package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/jdefrancesco/wormzy/internal/rendezvous"
	"github.com/quic-go/quic-go"
)

type directTarget struct {
	cand rendezvous.Candidate
	addr *net.UDPAddr
}

type directRaceResult struct {
	conn      *quic.Conn
	initiated bool
	candidate *rendezvous.Candidate
	attempt   int
	err       error
	discard   func()
}

type relayRaceResult struct {
	conn      *quic.Conn
	transport *quic.Transport
	err       error
	discard   func()
}

type progressivePathSelection struct {
	peer             rendezvous.SelfInfo
	directCandidates []rendezvous.Candidate
	relayCandidate   *rendezvous.Candidate
	iceSession       *iceQUICSession
	upnpMapping      *upnpMapping
	cleanupOnce      sync.Once
}

// cleanup releases the UPnP mapping retained for a legacy direct path.
func (selection *progressivePathSelection) cleanup(rep Reporter) {
	if selection == nil {
		return
	}
	selection.cleanupOnce.Do(func() {
		cleanupUPnPMapping(selection.upnpMapping, rep)
		selection.upnpMapping = nil
	})
}

// prepareProgressivePath tries direct ICE first, then prepares synchronized
// UPnP candidates for the legacy direct race when ICE does not establish QUIC.
func prepareProgressivePath(
	ctx context.Context,
	cfg Config,
	mbox mailbox,
	rep Reporter,
	udpConn *net.UDPConn,
	self rendezvous.SelfInfo,
	peer rendezvous.SelfInfo,
	directCandidates []rendezvous.Candidate,
	relayCandidate *rendezvous.Candidate,
	code string,
	psk []byte,
) *progressivePathSelection {
	selection := &progressivePathSelection{
		peer:             peer,
		directCandidates: directCandidates,
		relayCandidate:   relayCandidate,
	}
	if cfg.Loopback {
		return selection
	}

	var upnpAttempt *delayedUPnPAttempt
	defer func() {
		if upnpAttempt == nil {
			return
		}
		outcome := upnpAttempt.stop()
		if outcome.mapping != selection.upnpMapping {
			cleanupUPnPMapping(outcome.mapping, rep)
		}
	}()

	rep.Stage(StageQUIC, StageStateRunning, "ice connectivity checks")
	progressiveUPnP := peerSupportsFeature(peer, featureProgressiveUPnPV1)
	startUPnPFallback := func() {
		if !progressiveUPnP || upnpAttempt != nil {
			return
		}
		upnpAttempt = startDelayedUPnPFallback(ctx, cfg, udpConn, self.Public, rep)
	}
	if !progressiveUPnP {
		rep.Logf("upnp/fallback unavailable peer lacks %s", featureProgressiveUPnPV1)
	}

	iceSession, iceErr := attemptICEQUICSession(ctx, cfg, mbox, rep, peer, code, psk, startUPnPFallback)
	switch {
	case iceErr == nil && iceSession != nil:
		if upnpAttempt != nil {
			stopAndCleanupUPnPFallback(upnpAttempt, rep, "ICE path selected")
			upnpAttempt = nil
		}
		selection.iceSession = iceSession
		return selection
	case errors.Is(iceErr, errICESkipped):
		rep.Logf("ice/skipped peer does not advertise %s", featureICEv1)
	case iceErr != nil:
		rep.Logf("ice/failed %v (continuing legacy punch path)", iceErr)
	}
	startUPnPFallback()

	if upnpAttempt == nil {
		return selection
	}
	outcome := upnpAttempt.wait()
	upnpAttempt.cancel()
	upnpAttempt = nil
	selection.upnpMapping = outcome.mapping
	if outcome.err != nil && !errors.Is(outcome.err, context.Canceled) {
		rep.Logf("upnp/fallback unavailable: %v", outcome.err)
	}

	syncCtx, cancelSync := context.WithTimeout(ctx, progressiveUPnPSyncTimeout(cfg))
	refreshedPeer, syncErr := synchronizeUPnPFallback(
		syncCtx,
		mbox,
		self,
		peer,
		outcome,
		code,
		cfg.Mode,
		psk,
		rep,
	)
	cancelSync()
	if syncErr != nil {
		rep.Logf("upnp/fallback candidate refresh failed: %v", syncErr)
		return selection
	}

	selection.peer = refreshedPeer
	refreshedDirect, refreshedRelay, refreshErr := selectPeerCandidates(self, refreshedPeer, cfg.Loopback)
	if refreshErr != nil {
		rep.Logf("upnp/fallback refreshed candidates unusable: %v", refreshErr)
		return selection
	}
	selection.directCandidates = refreshedDirect
	selection.relayCandidate = refreshedRelay
	rep.Logf("candidates/peer refreshed %s", formatCandidateList(refreshedPeer.Candidates))
	return selection
}

type legacyQUICPath struct {
	conn          *quic.Conn
	initiated     bool
	candidate     rendezvous.Candidate
	directOutcome string
	directSummary string
	cleanupFunc   func()
	cleanupOnce   sync.Once
}

// cleanup releases the listener and any dedicated relay UDP transport retained
// by the selected legacy path.
func (path *legacyQUICPath) cleanup() {
	if path == nil {
		return
	}
	path.cleanupOnce.Do(func() {
		if path.cleanupFunc != nil {
			path.cleanupFunc()
		}
	})
}

type legacyRaceWorkers struct {
	ctx           context.Context
	cancel        context.CancelFunc
	directResults chan directRaceResult
	relayResults  chan relayRaceResult
	wg            sync.WaitGroup
	finishOnce    sync.Once
}

// newLegacyRaceWorkers creates the bounded result channels and cancellation
// scope shared by direct and relay connection workers.
func newLegacyRaceWorkers(ctx context.Context, timeout time.Duration, directTargets int) *legacyRaceWorkers {
	workerCtx, cancel := context.WithTimeout(ctx, timeout)
	return &legacyRaceWorkers{
		ctx:           workerCtx,
		cancel:        cancel,
		directResults: make(chan directRaceResult, directTargets*3+1),
		relayResults:  make(chan relayRaceResult, 1),
	}
}

// launch starts one race worker tracked by finish.
func (workers *legacyRaceWorkers) launch(run func(context.Context)) {
	workers.wg.Add(1)
	go func() {
		defer workers.wg.Done()
		run(workers.ctx)
	}()
}

// finish cancels and joins every worker before releasing any late successful
// connection that lost the race.
func (workers *legacyRaceWorkers) finish() {
	if workers == nil {
		return
	}
	workers.finishOnce.Do(func() {
		workers.cancel()
		workers.wg.Wait()
		close(workers.directResults)
		close(workers.relayResults)
		drainRaceLosers(workers.directResults, workers.relayResults)
	})
}

// drainRaceLosers releases successful connections that completed after the
// direct-versus-relay race had already selected its winner.
func drainRaceLosers(direct <-chan directRaceResult, relay <-chan relayRaceResult) {
	for result := range direct {
		if result.discard != nil {
			result.discard()
		}
	}
	for result := range relay {
		if result.discard != nil {
			result.discard()
		}
	}
}

// resolveDirectTargets parses the authenticated peer candidates that can enter
// the legacy UDP punch race.
func resolveDirectTargets(candidates []rendezvous.Candidate, rep Reporter) []directTarget {
	targets := make([]directTarget, 0, len(candidates))
	for _, candidate := range candidates {
		peerUDP, err := net.ResolveUDPAddr("udp4", candidate.Addr)
		if err != nil {
			rep.Logf("direct candidate %s (%s) resolve failed: %v", candidate.Addr, candidate.Type, err)
			continue
		}
		targets = append(targets, directTarget{cand: candidate, addr: peerUDP})
	}
	return targets
}

// startLegacyPunching sends bounded UDP punch packets until path selection
// completes and returns an idempotent worker cleanup function.
func startLegacyPunching(
	ctx context.Context,
	udpConn *net.UDPConn,
	targets []directTarget,
	rep Reporter,
) func() {
	punchCtx, cancelPunch := context.WithCancel(ctx)
	stopPunch := make(chan struct{})
	var (
		punchWG sync.WaitGroup
		once    sync.Once
	)
	if len(targets) > 0 {
		punchTargets := make([]*net.UDPAddr, 0, len(targets))
		for _, target := range targets {
			punchTargets = append(punchTargets, target.addr)
		}
		punchWG.Add(1)
		go func() {
			defer punchWG.Done()
			punchLoop(punchCtx, udpConn, punchTargets, stopPunch, rep)
		}()
	}
	return func() {
		once.Do(func() {
			close(stopPunch)
			cancelPunch()
			punchWG.Wait()
		})
	}
}

// startLegacyDirectWorkers launches the authenticated accept path and three
// staggered dials for each direct candidate.
func startLegacyDirectWorkers(
	workers *legacyRaceWorkers,
	cfg Config,
	rep Reporter,
	udpConn *net.UDPConn,
	listener *quic.Listener,
	transport *quic.Transport,
	targets []directTarget,
	clientTLS *tls.Config,
	quicConf *quic.Config,
	psk []byte,
) {
	workers.launch(func(workerCtx context.Context) {
		rep.Logf("direct/accept waiting on %s", udpConn.LocalAddr())
		var lastErr error
		for {
			conn, err := listener.Accept(workerCtx)
			if err != nil {
				if lastErr != nil && workerCtx.Err() == nil {
					err = lastErr
				}
				workers.directResults <- directRaceResult{initiated: false, attempt: 0, err: err}
				return
			}
			discard := func() { _ = conn.CloseWithError(0, "unauthenticated direct path discarded") }
			authCtx, cancelAuth := context.WithTimeout(workerCtx, peerConfirmationTimeout(cfg.HandshakeTimeout))
			authErr := confirmQUICPeer(authCtx, conn, false, psk)
			cancelAuth()
			if authErr != nil {
				discard()
				lastErr = authErr
				rep.Logf("direct/accept rejected unauthenticated peer: %v", authErr)
				continue
			}
			workers.directResults <- directRaceResult{
				conn:      conn,
				initiated: false,
				attempt:   0,
				discard: func() {
					_ = conn.CloseWithError(0, "alternate direct path discarded")
				},
			}
			return
		}
	})

	launchDial := func(target directTarget, delay time.Duration, attempt int) {
		workers.launch(func(workerCtx context.Context) {
			rep.Logf("direct/dial-schedule target=%s type=%s attempt=%d delay=%s", target.addr.String(), target.cand.Type, attempt, delay)
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-workerCtx.Done():
					timer.Stop()
					rep.Logf("direct/dial-cancel target=%s type=%s attempt=%d", target.addr.String(), target.cand.Type, attempt)
					return
				case <-timer.C:
				}
			}
			rep.Logf("direct/dial-start target=%s type=%s attempt=%d", target.addr.String(), target.cand.Type, attempt)
			conn, err := transport.Dial(workerCtx, target.addr, clientTLS, quicConf)
			candidate := target.cand
			if err == nil && conn != nil {
				authCtx, cancelAuth := context.WithTimeout(workerCtx, peerConfirmationTimeout(cfg.HandshakeTimeout))
				err = confirmQUICPeer(authCtx, conn, true, psk)
				cancelAuth()
				if err != nil {
					_ = conn.CloseWithError(0, "direct peer authentication failed")
					conn = nil
				}
			}
			result := directRaceResult{conn: conn, initiated: true, candidate: &candidate, attempt: attempt, err: err}
			if conn != nil {
				result.discard = func() { _ = conn.CloseWithError(0, "alternate direct path discarded") }
			}
			workers.directResults <- result
		})
	}
	if len(targets) == 0 {
		return
	}
	baseDelay := time.Duration(0)
	if cfg.Mode == "send" {
		baseDelay = 200 * time.Millisecond
	}
	rep.Logf("starting direct race with %d candidate(s)", len(targets))
	for i, target := range targets {
		launchDial(target, baseDelay+time.Duration(i)*120*time.Millisecond, 1)
	}
	for i, target := range targets {
		launchDial(target, baseDelay+700*time.Millisecond+time.Duration(i)*120*time.Millisecond, 2)
	}
	for i, target := range targets {
		launchDial(target, baseDelay+1500*time.Millisecond+time.Duration(i)*120*time.Millisecond, 3)
	}
}

// launchLegacyRelayAttempt starts one bounded custom-relay connection attempt.
func launchLegacyRelayAttempt(
	workers *legacyRaceWorkers,
	cfg Config,
	relayAddr string,
	psk []byte,
) {
	workers.launch(func(workerCtx context.Context) {
		attemptCtx, cancel := context.WithTimeout(workerCtx, relayAttemptTimeout)
		defer cancel()
		conn, transport, err := dialRelay(attemptCtx, relayAddr, cfg)
		if err != nil {
			workers.relayResults <- relayRaceResult{err: err}
			return
		}
		discard := func() {
			_ = conn.CloseWithError(0, "alternate relay path discarded")
			if transport != nil && transport.Conn != nil {
				_ = transport.Conn.Close()
			}
		}
		if err := registerRelay(attemptCtx, conn, cfg.Mode, psk); err != nil {
			discard()
			workers.relayResults <- relayRaceResult{err: err}
			return
		}
		if err := confirmQUICPeer(attemptCtx, conn, cfg.Mode == "send", psk); err != nil {
			discard()
			workers.relayResults <- relayRaceResult{err: err}
			return
		}
		workers.relayResults <- relayRaceResult{conn: conn, transport: transport, discard: discard}
	})
}

// establishLegacyQUICPath races authenticated direct QUIC paths and the custom
// relay fallback, returning ownership of the selected carrier cleanup.
func establishLegacyQUICPath(
	ctx context.Context,
	cfg Config,
	rep Reporter,
	udpConn *net.UDPConn,
	directCandidates []rendezvous.Candidate,
	relayCandidate *rendezvous.Candidate,
	psk []byte,
) (path *legacyQUICPath, err error) {
	directTargets := resolveDirectTargets(directCandidates, rep)
	if len(directTargets) > 0 {
		rep.Logf("direct/targets %s", formatDirectTargets(directTargets))
	}
	if relayCandidate != nil {
		rep.Logf("relay/candidate %s (%s)", relayCandidate.Addr, relayCandidate.Type)
	}
	if len(directTargets) == 0 && relayCandidate == nil {
		return nil, errors.New("peer did not advertise any dialable UDP candidates")
	}

	stopPunching := startLegacyPunching(ctx, udpConn, directTargets, rep)
	defer stopPunching()

	quicTransport := &quic.Transport{Conn: udpConn}
	serverTLS, err := selfSignedTLS()
	if err != nil {
		return nil, err
	}
	serverTLS.NextProtos = []string{alpn}
	// QUIC uses an ephemeral self-signed carrier; PAKE key confirmation below
	// authenticates every candidate before it may win the race.
	clientTLS := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{alpn}} // #nosec G402 -- PAKE authenticates the selected peer.
	quicConf := &quic.Config{
		KeepAlivePeriod:      15 * time.Second,
		MaxIdleTimeout:       cfg.IdleTimeout,
		HandshakeIdleTimeout: cfg.HandshakeTimeout,
	}

	rep.Stage(StageQUIC, StageStateRunning, "punching + dialing")
	listener, err := quicTransport.Listen(serverTLS, quicConf)
	if err != nil {
		return nil, err
	}
	var relayTransport *quic.Transport
	var carrierCleanupOnce sync.Once
	cleanupCarrier := func() {
		carrierCleanupOnce.Do(func() {
			if relayTransport != nil && relayTransport.Conn != nil {
				_ = relayTransport.Conn.Close()
			}
			_ = listener.Close()
		})
	}
	defer func() {
		if path == nil {
			cleanupCarrier()
		}
	}()

	workers := newLegacyRaceWorkers(ctx, cfg.HandshakeTimeout, len(directTargets))
	defer workers.finish()
	startLegacyDirectWorkers(
		workers,
		cfg,
		rep,
		udpConn,
		listener,
		quicTransport,
		directTargets,
		clientTLS,
		quicConf,
		psk,
	)

	dialableCandidates := make([]rendezvous.Candidate, 0, len(directTargets))
	for _, target := range directTargets {
		dialableCandidates = append(dialableCandidates, target.cand)
	}

	var quicConn *quic.Conn
	initiated := cfg.Mode == "recv"
	preferInitiated := cfg.Mode == "recv"
	preferredPath := "accept"
	if preferInitiated {
		preferredPath = "dial"
	}
	pathKind := func(initiated bool) string {
		if initiated {
			return "dial"
		}
		return "accept"
	}
	usedCandidate := pickFallbackDirectCandidate(dialableCandidates)
	var firstErr error
	relayInFlight := false
	relayAttempts := 0
	directOutcome := "pending"
	directStatus := make(map[string]string, len(directTargets))
	for _, target := range directTargets {
		directStatus[target.cand.Type+"@"+target.cand.Addr] = "pending"
	}
	fallbackDelay := relayFallbackDelay
	if len(directTargets) == 0 && relayCandidate != nil {
		fallbackDelay = 0
		directOutcome = "no-response"
	}

	const nonPreferredGrace = 650 * time.Millisecond
	var provisional *directRaceResult
	var provisionalTimer *time.Timer
	var provisionalTimerCh <-chan time.Time
	var relayTimer *time.Timer
	var relayTimerCh <-chan time.Time
	stopProvisionalTimer := func() {
		if provisionalTimer == nil {
			return
		}
		if !provisionalTimer.Stop() {
			select {
			case <-provisionalTimer.C:
			default:
			}
		}
		provisionalTimer = nil
		provisionalTimerCh = nil
	}
	stopRelayTimer := func() {
		if relayTimer == nil {
			return
		}
		if !relayTimer.Stop() {
			select {
			case <-relayTimer.C:
			default:
			}
		}
		relayTimer = nil
		relayTimerCh = nil
	}
	scheduleRelayAttempt := func(delay time.Duration) {
		if relayCandidate == nil {
			return
		}
		if relayTimer == nil {
			relayTimer = time.NewTimer(delay)
		} else {
			if !relayTimer.Stop() {
				select {
				case <-relayTimer.C:
				default:
				}
			}
			relayTimer.Reset(delay)
		}
		relayTimerCh = relayTimer.C
	}
	defer stopRelayTimer()
	discardDirect := func(result directRaceResult) {
		if result.discard != nil {
			result.discard()
		}
	}
	adoptDirect := func(result directRaceResult) {
		quicConn = result.conn
		initiated = result.initiated
		if result.initiated && result.candidate != nil {
			usedCandidate = *result.candidate
			key := usedCandidate.Type + "@" + usedCandidate.Addr
			directStatus[key] = "won"
			directOutcome = "won"
			rep.Logf("direct race won on %s (%s) attempt=%d", usedCandidate.Addr, usedCandidate.Type, result.attempt)
			return
		}
		matched := classifyCandidateByRemote(result.conn.RemoteAddr(), dialableCandidates)
		if matched != nil {
			usedCandidate = *matched
			key := matched.Type + "@" + matched.Addr
			directStatus[key] = "won"
		}
		directOutcome = "won"
		rep.Logf("direct race accepted from %s", result.conn.RemoteAddr())
	}
	if relayCandidate != nil {
		rep.Logf("relay/fallback armed delay=%s", fallbackDelay)
		scheduleRelayAttempt(fallbackDelay)
	} else {
		rep.Logf("relay/fallback unavailable (no relay candidate)")
	}

waitLoop:
	for quicConn == nil {
		select {
		case result := <-workers.directResults:
			if result.err == nil && result.conn != nil {
				// To avoid split-brain, prefer a deterministic direction by role and
				// use the first non-preferred success after a short grace window.
				if result.initiated == preferInitiated {
					stopProvisionalTimer()
					if provisional != nil && provisional.conn != nil && provisional.conn != result.conn {
						discardDirect(*provisional)
					}
					provisional = nil
					adoptDirect(result)
					break waitLoop
				}
				if provisional == nil {
					provisionalResult := result
					provisional = &provisionalResult
					provisionalTimer = time.NewTimer(nonPreferredGrace)
					provisionalTimerCh = provisionalTimer.C
					rep.Logf(
						"direct race provisional path=%s waiting=%s for preferred=%s",
						pathKind(result.initiated),
						nonPreferredGrace,
						preferredPath,
					)
					continue
				}
				discardDirect(result)
				rep.Logf("direct race extra %s path discarded", pathKind(result.initiated))
				continue
			}
			if result.candidate != nil {
				key := result.candidate.Type + "@" + result.candidate.Addr
				outcome := classifyDialError(result.err)
				directStatus[key] = outcome
				rep.Logf("direct race failed on %s (%s) attempt=%d outcome=%s err=%v", result.candidate.Addr, result.candidate.Type, result.attempt, outcome, result.err)
			}
			if firstErr == nil {
				firstErr = result.err
			}
		case <-provisionalTimerCh:
			stopProvisionalTimer()
			if provisional != nil {
				rep.Logf(
					"direct race selecting provisional path=%s after waiting %s for preferred=%s",
					pathKind(provisional.initiated),
					nonPreferredGrace,
					preferredPath,
				)
				adoptDirect(*provisional)
				provisional = nil
				break waitLoop
			}
		case <-relayTimerCh:
			if relayCandidate == nil {
				relayTimerCh = nil
				continue
			}
			if relayInFlight {
				scheduleRelayAttempt(relayRetryDelay)
				continue
			}
			relayAttempts++
			if directOutcome == "pending" {
				directOutcome = "quic-timeout"
			}
			if relayAttempts == 1 {
				rep.Logf("falling back to relay %s", relayCandidate.Addr)
				rep.Stage(StageQUIC, StageStateRunning, "relay fallback")
			} else {
				rep.Logf("retrying relay fallback attempt=%d %s", relayAttempts, relayCandidate.Addr)
			}
			relayInFlight = true
			scheduleRelayAttempt(relayRetryDelay)
			launchLegacyRelayAttempt(workers, cfg, relayCandidate.Addr, psk)
		case relay := <-workers.relayResults:
			relayInFlight = false
			if relay.err == nil && relay.conn != nil {
				stopRelayTimer()
				stopProvisionalTimer()
				if provisional != nil {
					discardDirect(*provisional)
					provisional = nil
				}
				quicConn = relay.conn
				relayTransport = relay.transport
				initiated = cfg.Mode == "send"
				usedCandidate = *relayCandidate
				if len(directTargets) == 0 {
					directOutcome = "no-response"
				}
				break waitLoop
			}
			rep.Logf("relay fallback attempt %d failed: %v", relayAttempts, relay.err)
			if firstErr == nil {
				firstErr = relay.err
			}
		case <-workers.ctx.Done():
			if provisional != nil && provisional.conn != nil {
				stopProvisionalTimer()
				rep.Logf(
					"direct race selecting provisional path=%s because preferred=%s did not arrive in time",
					pathKind(provisional.initiated),
					preferredPath,
				)
				adoptDirect(*provisional)
				provisional = nil
				break waitLoop
			}
			if quicConn != nil {
				break waitLoop
			}
			if relayInFlight {
				relay := <-workers.relayResults
				relayInFlight = false
				if relay.err == nil && relay.conn != nil {
					quicConn = relay.conn
					relayTransport = relay.transport
					initiated = cfg.Mode == "send"
					usedCandidate = *relayCandidate
					break waitLoop
				}
				if firstErr == nil {
					firstErr = relay.err
				}
			}
			if directOutcome == "pending" {
				if len(directTargets) == 0 {
					directOutcome = "no-response"
				} else {
					directOutcome = "quic-timeout"
				}
			}
			if firstErr == nil {
				firstErr = workers.ctx.Err()
			}
			rep.Stage(StageQUIC, StageStateError, firstErr.Error())
			return nil, firstErr
		}
	}
	workers.finish()

	if quicConn == nil {
		err := firstErr
		if err == nil {
			err = errors.New("failed to establish QUIC")
		}
		rep.Stage(StageQUIC, StageStateError, err.Error())
		return nil, err
	}
	if directOutcome == "pending" {
		if usedCandidate.Type == "relay" {
			directOutcome = "quic-timeout"
		} else {
			directOutcome = "won"
		}
	}
	directSummary := summarizeDirectRace(directStatus)
	if directSummary != "" {
		rep.Logf("direct race outcome=%s details=%s", directOutcome, directSummary)
	} else {
		rep.Logf("direct race outcome=%s", directOutcome)
	}

	if usedCandidate.Type == "relay" {
		rep.Stage(StageQUIC, StageStateDone, "relay fallback")
	} else if initiated {
		rep.Logf("dialed QUIC peer %s", usedCandidate.Addr)
		rep.Stage(StageQUIC, StageStateDone, usedCandidate.Addr)
	} else {
		rep.Logf("accepted QUIC connection from %s", quicConn.RemoteAddr())
		rep.Stage(StageQUIC, StageStateDone, quicConn.RemoteAddr().String())
	}

	return &legacyQUICPath{
		conn:          quicConn,
		initiated:     initiated,
		candidate:     usedCandidate,
		directOutcome: directOutcome,
		directSummary: directSummary,
		cleanupFunc:   cleanupCarrier,
	}, nil
}
