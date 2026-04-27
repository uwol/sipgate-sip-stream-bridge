package bridge

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	siplib "github.com/emiago/sipgo/sip"
	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/config"
	"github.com/sipgate/sipgate-sip-stream-bridge/internal/observability"
	sip "github.com/sipgate/sipgate-sip-stream-bridge/internal/sip"
)

// PortPool is a bounded, channel-based RTP port pool.
// Acquire/Release are O(1) and lock-free (select on buffered channel).
// Acquire is non-blocking — returns error immediately when pool is exhausted (SIP-04).
type PortPool struct {
	ports chan int
}

// NewPortPool creates a PortPool pre-loaded with ports [min, max).
// Returns an error if min >= max (invalid range).
func NewPortPool(min, max int) (*PortPool, error) {
	if min >= max {
		return nil, fmt.Errorf("RTP_PORT_MIN (%d) must be less than RTP_PORT_MAX (%d)", min, max)
	}
	ch := make(chan int, max-min)
	for p := min; p < max; p++ {
		ch <- p
	}
	return &PortPool{ports: ch}, nil
}

// Acquire is non-blocking — returns error if pool is exhausted (SIP-04).
func (p *PortPool) Acquire() (int, error) {
	select {
	case port := <-p.ports:
		return port, nil
	default:
		return 0, fmt.Errorf("RTP port pool exhausted")
	}
}

// Release returns a port to the pool. Call via defer in StartSession.
func (p *PortPool) Release(port int) {
	select {
	case p.ports <- port:
	default:
		// programming error: port was never acquired or released twice — drop silently
	}
}

// CallManager stores active call sessions and delegates port management to PortPool.
// Satisfies the sip.CallManagerIface defined in internal/sip/handler.go.
type CallManager struct {
	sessions sync.Map // key: callID string → value: *CallSession
	portPool *PortPool
	cfg      config.Config
	log      zerolog.Logger
	metrics  *observability.Metrics
}

// NewCallManager creates a CallManager with the given port pool, config, logger, and metrics.
func NewCallManager(portPool *PortPool, cfg config.Config, log zerolog.Logger, metrics *observability.Metrics) *CallManager {
	return &CallManager{
		portPool: portPool,
		cfg:      cfg,
		log:      log,
		metrics:  metrics,
	}
}

// ActiveCount returns the number of currently active call sessions.
// Used by Phase 8 /health endpoint and DrainAll polling loop.
func (m *CallManager) ActiveCount() int {
	count := 0
	m.sessions.Range(func(_, _ any) bool { count++; return true })
	return count
}

// DrainAll sends SIP BYE to every active call and waits until all sessions self-exit.
// Sessions call m.sessions.Delete(callID) via defer in StartSession when their goroutine exits.
// Uses polling (50ms tick) rather than a drain WaitGroup to avoid modifying the session close path.
// CRITICAL: call handler.SetShutdown() BEFORE DrainAll to prevent new sessions during drain.
func (m *CallManager) DrainAll(ctx context.Context) {
	// BYE every active session
	m.sessions.Range(func(key, value any) bool {
		s := value.(*CallSession)
		m.log.Info().Str("call_id", s.callID).Msg("shutdown: sending BYE to active call")
		_ = s.dlg.Bye(context.Background())
		return true
	})

	// Wait until sessions map is empty (sessions self-delete on goroutine exit)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		count := 0
		m.sessions.Range(func(_, _ any) bool { count++; return true })
		if count == 0 {
			return
		}
		select {
		case <-ctx.Done():
			remaining := 0
			m.sessions.Range(func(_, _ any) bool { remaining++; return true })
			m.log.Warn().Int("remaining", remaining).Msg("shutdown: drain timeout — abandoning active sessions")
			return
		case <-ticker.C:
		}
	}
}

// AcquirePort delegates to portPool.Acquire().
func (m *CallManager) AcquirePort() (int, error) {
	return m.portPool.Acquire()
}

// ReleasePort delegates to portPool.Release().
func (m *CallManager) ReleasePort(port int) {
	m.portPool.Release(port)
}

// TransferCall sends an in-dialog REFER for an active call.
// callID is the SIP Call-ID used as the session map key.
func (m *CallManager) TransferCall(callID, target string) error {
	v, ok := m.sessions.Load(callID)
	if !ok {
		return fmt.Errorf("call not found: %s", callID)
	}
	session := v.(*CallSession)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	referTo, err := normalizeReferTarget(target, m.cfg.SIPDomain)
	if err != nil {
		return err
	}

	if err := session.sendRefer(ctx, referTo); err != nil {
		return err
	}

	m.log.Info().Str("call_id", callID).Str("target", referTo.String()).Msg("transfer REFER accepted by remote peer")
	return nil
}

// EndCall sends SIP BYE for a single active call.
func (m *CallManager) EndCall(callID string) error {
	v, ok := m.sessions.Load(callID)
	if !ok {
		return fmt.Errorf("call not found: %s", callID)
	}

	session := v.(*CallSession)
	session.transferMu.Lock()
	defer session.transferMu.Unlock()

	if session.dlg == nil {
		m.log.Debug().Str("call_id", callID).Msg("end call requested but dialog is nil")
		return fmt.Errorf("call dialog unavailable: %s", callID)
	}

	if err := session.dlg.Bye(context.Background()); err != nil {
		m.log.Warn().Err(err).Str("call_id", callID).Msg("failed to send BYE")
		return err
	}

	m.log.Info().Str("call_id", callID).Msg("BYE sent")
	return nil
}

// HandleReferNotify hangs up the original dialog leg when REFER progress NOTIFY is received.
// Best-effort behavior: missing/ended sessions are ignored to tolerate retransmits and races.
func (m *CallManager) HandleReferNotify(callID string) {
	v, ok := m.sessions.Load(callID)
	if !ok {
		m.log.Debug().Str("call_id", callID).Msg("REFER NOTIFY for unknown call — session already ended")
		return
	}

	session := v.(*CallSession)
	session.transferMu.Lock()
	defer session.transferMu.Unlock()

	if session.dlg == nil {
		m.log.Debug().Str("call_id", callID).Msg("REFER NOTIFY received but dialog is nil — skipping BYE")
		return
	}

	if err := session.dlg.Bye(context.Background()); err != nil {
		m.log.Warn().Err(err).Str("call_id", callID).Msg("REFER NOTIFY hangup failed")
		return
	}

	m.log.Info().Str("call_id", callID).Msg("REFER NOTIFY hangup sent (BYE)")
}

func normalizeReferTarget(target, defaultDomain string) (siplib.Uri, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return siplib.Uri{}, fmt.Errorf("target must not be empty")
	}

	if !strings.HasPrefix(t, "sip:") && !strings.HasPrefix(t, "sips:") {
		if strings.Contains(t, "@") {
			t = "sip:" + t
		} else {
			if defaultDomain == "" {
				return siplib.Uri{}, fmt.Errorf("target is missing domain and SIP_DOMAIN is empty")
			}
			t = "sip:" + t + "@" + defaultDomain
		}
	}

	var uri siplib.Uri
	if err := siplib.ParseUri(t, &uri); err != nil {
		return siplib.Uri{}, fmt.Errorf("invalid transfer target: %w", err)
	}
	return uri, nil
}

func (s *CallSession) sendRefer(ctx context.Context, referTo siplib.Uri) error {
	s.transferMu.Lock()
	defer s.transferMu.Unlock()

	recipient := referTo
	if contact := s.dlg.InviteRequest.Contact(); contact != nil {
		recipient = contact.Address
	}

	req := siplib.NewRequest(siplib.REFER, recipient)
	req.AppendHeader(&siplib.ReferToHeader{Address: referTo})

	res, err := s.dlg.Do(ctx, req)
	if err != nil {
		return fmt.Errorf("send REFER failed: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("REFER rejected with %d %s", res.StatusCode, res.Reason)
	}

	return nil
}

// StartSession dials WS and runs the RTP bridge. Called in a goroutine by onInvite AFTER
// the 200 OK has been sent and ACK received (RespondSDP is handled synchronously in onInvite).
// Blocks until the call ends. Port is released on all exit paths via defer.
func (m *CallManager) StartSession(
	dlg *sipgo.DialogServerSession,
	req *siplib.Request,
	callerSDP *sip.CallerSDP,
	rtpPort int,
	audioPT uint8,
	localSRTPKey []byte,
	localSRTPSalt []byte,
	log zerolog.Logger,
) {
	callID := req.CallID().Value()
	// CON-02: always release port — covers WS dial failure and call end.
	defer m.portPool.Release(rtpPort)

	// streamSid: MZ + 32 hex chars (Twilio Media Streams convention)
	// callSidToken: CA + 32 hex chars (Twilio callSid convention, distinct from SIP Call-ID)
	streamSid := "MZ" + strings.ReplaceAll(uuid.New().String(), "-", "")
	callSidToken := "CA" + strings.ReplaceAll(uuid.New().String(), "-", "")

	callerRTPAddr := &net.UDPAddr{
		IP:   net.ParseIP(callerSDP.RTPAddr),
		Port: callerSDP.RTPPort,
	}

	// Compute media format fields from negotiated codec.
	encoding, sampleRate := sip.MediaFormatForPT(audioPT)

	// Call is already answered (200 OK sent + ACK received in onInvite). Dial WS now.
	// If WS fails we send BYE (can't reject after 200 OK).
	wsCtx, wsCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer wsCancel()
	wsConn, err := dialWS(wsCtx, m.cfg.WSTargetURL)
	if err != nil {
		log.Error().Err(err).Str("call_id", callID).Msg("dialWS failed — sending BYE")
		_ = dlg.Bye(context.Background())
		return
	}

	session := &CallSession{
		callID:          callID,
		callSidToken:    callSidToken,
		streamSid:       streamSid,
		dlg:             dlg,
		rtpPort:         rtpPort,
		callerRTP:       callerRTPAddr,
		dtmfPT:          callerSDP.DTMFPayloadType,
		audioPT:         audioPT,
		silenceFrame:    sip.SilenceFrameForPT(audioPT),
		mediaEncoding:   encoding,
		mediaSampleRate: sampleRate,
		localSRTPKey:    localSRTPKey,
		localSRTPSalt:   localSRTPSalt,
		remoteSRTPKey:   callerSDP.RemoteSRTPKey,
		remoteSRTPSalt:  callerSDP.RemoteSRTPSalt,
		cfg:             m.cfg,
		log:             log,
		metrics:         m.metrics,
		onTransfer:      func(target string) error { return m.TransferCall(callID, target) },
	}

	m.sessions.Store(callID, session)
	if m.metrics != nil {
		m.metrics.ActiveCalls.Inc()
	}
	defer func() {
		m.sessions.Delete(callID)
		if m.metrics != nil {
			m.metrics.ActiveCalls.Dec()
		}
	}()

	session.run(dlg.Context(), wsConn)
}
