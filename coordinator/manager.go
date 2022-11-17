package coordinator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	mathrand "math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/scroll-tech/go-ethereum/core/types"
	"github.com/scroll-tech/go-ethereum/log"
	"github.com/scroll-tech/go-ethereum/rpc"

	"scroll-tech/common/message"

	"scroll-tech/database/orm"

	"scroll-tech/coordinator/config"
	"scroll-tech/coordinator/verifier"
)

const (
	proofAndPkBufferSize = 10
)

type rollerStatus int32

const (
	rollerAssigned rollerStatus = iota
	rollerProofValid
	rollerProofInvalid
)

type rollerProofStatus struct {
	pk     string
	status rollerStatus
}

// Contains all the information on an ongoing proof generation session.
type session struct {
	// session id
	id uint64
	// A list of all participating rollers and if they finished proof generation for this session.
	// The map key is a hexadecimal encoding of the roller public key, as byte slices
	// can not be compared explicitly.
	rollers      map[string]rollerStatus
	roller_names map[string]string
	// session start time
	startTime time.Time
	// finish channel is used to pass the public key of the rollers who finished proving process.
	finishChan chan rollerProofStatus
}

// Manager is responsible for maintaining connections with active rollers,
// sending the challenges, and receiving proofs. It also regulates the reward
// distribution. All read and write logic and connection handling happens through
// a modular websocket server, contained within the Manager. Incoming messages are
// then passed to the Manager where the actual handling logic resides.
type Manager struct {
	// The manager context.
	ctx context.Context

	// The roller manager configuration.
	cfg *config.RollerManagerConfig

	// The indicator whether the backend is running or not.
	running int32
	// The websocket server which holds the connections with active rollers.
	server *server

	// A mutex guarding the boolean below.
	mu sync.RWMutex
	// A map containing all active proof generation sessions.
	sessions map[uint64]session
	// A map containing proof failed or verify failed proof.
	failedSessionInfos map[uint64]SessionInfo

	// A direct connection to the Halo2 verifier, used to verify
	// incoming proofs.
	verifier *verifier.Verifier

	// db interface
	orm orm.BlockResultOrm
}

// New returns a new instance of Manager. The instance will be not fully prepared,
// and still needs to be finalized and ran by calling `manager.Start`.
func New(ctx context.Context, cfg *config.RollerManagerConfig, orm orm.BlockResultOrm) (*Manager, error) {
	var v *verifier.Verifier
	if cfg.VerifierEndpoint != "" {
		var err error
		v, err = verifier.NewVerifier(cfg.VerifierEndpoint)
		if err != nil {
			return nil, err
		}
	}

	log.Info("Start rollerManager successfully.")

	return &Manager{
		ctx:                ctx,
		cfg:                cfg,
		server:             newServer(cfg.Endpoint),
		sessions:           make(map[uint64]session),
		failedSessionInfos: make(map[uint64]SessionInfo),
		verifier:           v,
		orm:                orm,
	}, nil
}

// Start the Manager module.
func (m *Manager) Start() error {
	if m.isRunning() {
		return nil
	}

	// m.orm may be nil in scroll tests
	if m.orm != nil {
		// clean up assigned but not submitted task
		blocks, err := m.orm.GetBlockResults(map[string]interface{}{"status": orm.BlockAssigned})
		if err == nil {
			for _, block := range blocks {
				if err := m.orm.UpdateBlockStatus(block.BlockTrace.Number.ToInt().Uint64(), orm.BlockUnassigned); err != nil {
					log.Error("fail to reset block_status as Unassigned")
				}
			}
		} else {
			log.Error("fail to fetch assigned blocks")
		}
	}

	if err := m.server.start(); err != nil {
		return err
	}
	atomic.StoreInt32(&m.running, 1)

	go m.Loop()
	return nil
}

// Stop the Manager module, for a graceful shutdown.
func (m *Manager) Stop() {
	if !m.isRunning() {
		return
	}

	// Stop accepting connections
	if err := m.server.stop(); err != nil {
		log.Error("Server shutdown failed", "error", err)
		return
	}

	atomic.StoreInt32(&m.running, 0)
}

// isRunning returns an indicator whether manager is running or not.
func (m *Manager) isRunning() bool {
	return atomic.LoadInt32(&m.running) == 1
}

// Loop keeps the manager running.
func (m *Manager) Loop() {
	var (
		tick   = time.NewTicker(time.Second * 3)
		traces []*types.BlockResult
	)
	defer tick.Stop()

	for {
		select {
		case <-tick.C:
			if len(traces) == 0 && m.orm != nil {
				var err error
				numIdleRollers := m.GetNumberOfIdleRollers()
				// TODO: add cache
				if traces, err = m.orm.GetBlockResults(
					map[string]interface{}{"status": orm.BlockUnassigned},
					fmt.Sprintf(
						"ORDER BY number %s LIMIT %d;",
						m.cfg.OrderSession,
						numIdleRollers,
					),
				); err != nil {
					log.Error("failed to get blockResult", "error", err)
					continue
				}
			}
			// Select roller and send message
			for len(traces) > 0 && m.StartProofGenerationSession(traces[0]) {
				traces = traces[1:]
			}
		case msg := <-m.server.msgChan:
			if err := m.HandleMessage(msg.pk, msg.message); err != nil {
				log.Error(
					"could not handle message",
					"error", err,
				)
			}
		case <-m.ctx.Done():
			if m.ctx.Err() != nil {
				log.Error(
					"manager context canceled with error",
					"error", m.ctx.Err(),
				)
			}
			return
		}
	}
}

// HandleMessage handle a message from a roller.
func (m *Manager) HandleMessage(pk string, payload []byte) error {
	// Recover message
	msg := &message.Msg{}
	if err := json.Unmarshal(payload, msg); err != nil {
		return err
	}

	switch msg.Type {
	case message.Error:
		// Just log it for now.
		log.Error("error message received from roller", "message", msg)
		// TODO: handle in m.failedSessionInfos
		return nil
	case message.Register:
		// We shouldn't get this message, as the sequencer should handle registering at the start
		// of the connection.
		return errors.New("attempted handshake at the wrong time")
	case message.BlockTrace:
		// We shouldn't get this message, as the sequencer should always be the one to send it
		return errors.New("received illegal message")
	case message.Proof:
		return m.HandleZkProof(pk, msg.Payload)
	default:
		return fmt.Errorf("unrecognized message type %v", msg.Type)
	}
}

// HandleZkProof handle a ZkProof submitted from a roller.
// For now only proving/verifying error will lead to setting status as skipped.
// db/unmarshal errors will not because they are errors on the business logic side.
func (m *Manager) HandleZkProof(pk string, payload []byte) error {
	var dbErr error
	var success bool

	msg := &message.ProofMsg{}
	if err := json.Unmarshal(payload, msg); err != nil {
		return err
	}

	// Assess if the proof generation session for the given ID is still active.
	// We hold the read lock until the end of the function so that there is no
	// potential race for channel deletion.
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[msg.ID]
	if !ok {
		return fmt.Errorf("proof generation session for id %v does not exist", msg.ID)
	}
	proofTimeSec := uint64(time.Since(s.startTime).Seconds())

	// Ensure this roller is eligible to participate in the session.
	if status, ok := s.rollers[pk]; !ok {
		return fmt.Errorf("roller %s is not eligible to partake in proof session %v", pk, msg.ID)
	} else if status == rollerProofValid {
		// In order to prevent DoS attacks, it is forbidden to repeatedly submit valid proofs.
		// TODO: Defend invalid proof resubmissions by one of the following two methods:
		// (i) slash the roller for each submission of invalid proof
		// (ii) set the maximum failure retry times
		log.Warn("roller has already submitted valid proof in proof session", "roller", pk, "proof id", msg.ID)
		return nil
	}
	log.Info("Received zk proof", "proof id", msg.ID)

	defer func() {
		// TODO: maybe we should use db tx for the whole process?
		// Roll back current proof's status.
		if dbErr != nil {
			if err := m.orm.UpdateBlockStatus(msg.ID, orm.BlockUnassigned); err != nil {
				log.Error("fail to reset block_status as Unassigned", "msg.ID", msg.ID)
			}
		}
		// set proof status
		var status rollerStatus
		if success && dbErr == nil {
			status = rollerProofValid
		} else {
			status = rollerProofInvalid
		}
		// notify the session that the roller finishes the proving process
		s.finishChan <- rollerProofStatus{pk, status}
	}()

	if msg.Status != message.StatusOk {
		log.Error("Roller failed to generate proof", "msg.ID", msg.ID, "error", msg.Error)
		if dbErr = m.orm.UpdateBlockStatus(msg.ID, orm.BlockFailed); dbErr != nil {
			log.Error("failed to update blockResult status", "status", orm.BlockFailed, "error", dbErr)
		}
		// record the failed session.
		m.addFailedSession(&s, msg.Error)
		return nil
	}

	// store proof content
	if dbErr = m.orm.UpdateProofByNumber(m.ctx, msg.ID, msg.Proof.Proof, msg.Proof.FinalPair, proofTimeSec); dbErr != nil {
		log.Error("failed to store proof into db", "error", dbErr)
		return dbErr
	}
	if dbErr = m.orm.UpdateBlockStatus(msg.ID, orm.BlockProved); dbErr != nil {
		log.Error("failed to update blockResult status", "status", orm.BlockProved, "error", dbErr)
		return dbErr
	}

	if m.verifier != nil {
		blockResults, err := m.orm.GetBlockResults(map[string]interface{}{"number": msg.ID})
		if len(blockResults) == 0 {
			if err != nil {
				log.Error("failed to get blockResults", "error", err)
			}
			return err
		}

		success, err = m.verifier.VerifyProof(blockResults[0], msg.Proof)
		if err != nil {
			// record failed session.
			m.addFailedSession(&s, err.Error())
			// TODO: this is only a temp workaround for testnet, we should return err in real cases
			success = false
			log.Error("Failed to verify zk proof", "proof id", msg.ID, "error", err)
			// TODO: Roller needs to be slashed if proof is invalid.
		} else {
			log.Info("Verify zk proof successfully", "verification result", success, "proof id", msg.ID)
		}
	} else {
		success = true
		log.Info("Verifier disabled, VerifyProof skipped")
		log.Info("Verify zk proof successfully", "verification result", success, "proof id", msg.ID)
	}

	var status orm.BlockStatus
	if success {
		status = orm.BlockVerified
	} else {
		// Set status as skipped if verification fails.
		// Note that this is only a workaround for testnet here.
		// TODO: In real cases we should reset to orm.BlockUnassigned
		// so as to re-distribute the task in the future
		status = orm.BlockFailed
	}
	if dbErr = m.orm.UpdateBlockStatus(msg.ID, status); dbErr != nil {
		log.Error("failed to update blockResult status", "status", status, "error", dbErr)
	}

	return dbErr
}

// CollectProofs collects proofs corresponding to a proof generation session.
func (m *Manager) CollectProofs(id uint64, s session) {
	timer := time.NewTimer(time.Duration(m.cfg.CollectionTime) * time.Minute)

	for {
		select {
		case <-timer.C:
			m.mu.Lock()

			// Ensure proper clean-up of resources.
			defer func() {
				delete(m.sessions, id)
				m.mu.Unlock()
			}()

			// Pick a random winner.
			// First, round up the keys that actually sent in a valid proof.
			var participatingRollers []string
			for pk, status := range s.rollers {
				if status == rollerProofValid {
					participatingRollers = append(participatingRollers, pk)
				}
			}
			// Ensure we got at least one proof before selecting a winner.
			if len(participatingRollers) == 0 {
				// record failed session.
				errMsg := "proof generation session ended without receiving any valid proofs"
				m.addFailedSession(&s, errMsg)
				log.Warn(errMsg, "session id", id)
				// Set status as skipped.
				// Note that this is only a workaround for testnet here.
				// TODO: In real cases we should reset to orm.BlockUnassigned
				// so as to re-distribute the task in the future
				if err := m.orm.UpdateBlockStatus(id, orm.BlockFailed); err != nil {
					log.Error("fail to reset block_status as Unassigned", "id", id)
				}
				return
			}

			// Now, select a random index for this slice.
			randIndex := mathrand.Intn(len(participatingRollers))
			_ = participatingRollers[randIndex]
			// TODO: reward winner
			return
		case ret := <-s.finishChan:
			m.mu.Lock()
			s.rollers[ret.pk] = ret.status
			m.mu.Unlock()
		}
	}
}

// GetRollerChan returns the channel in which newly created wrapped roller connections are sent.
func (m *Manager) GetRollerChan() chan *Roller {
	return m.server.rollerChan
}

// APIs collect API services.
func (m *Manager) APIs() []rpc.API {
	return []rpc.API{
		{
			Namespace: "roller",
			Public:    true,
			Service:   RollerDebugAPI(m),
		},
	}
}

// StartProofGenerationSession starts a proof generation session
func (m *Manager) StartProofGenerationSession(trace *types.BlockResult) bool {
	roller := m.SelectRoller()
	if roller == nil || roller.isClosed() {
		return false
	}

	id := (*big.Int)(trace.BlockTrace.Number).Uint64()
	log.Info("start proof generation session", "id", id)

	var dbErr error
	defer func() {
		if dbErr != nil {
			if err := m.orm.UpdateBlockStatus(id, orm.BlockUnassigned); err != nil {
				log.Error("fail to reset block_status as Unassigned", "id", id)
			}
		}
	}()

	pk := roller.AuthMsg.Identity.PublicKey
	log.Info("roller is picked", "name", roller.AuthMsg.Identity.Name, "public_key", pk)

	msg, err := createBlockTracesMsg(trace)
	if err != nil {
		log.Error(
			"could not create block traces message",
			"error", err,
		)
		return false
	}
	if err := roller.sendMessage(msg); err != nil {
		log.Error(
			"could not send traces message to roller",
			"error", err,
		)
		return false
	}

	s := session{
		id: id,
		rollers: map[string]rollerStatus{
			pk: rollerAssigned,
		},
		roller_names: map[string]string{
			pk: roller.AuthMsg.Identity.Name,
		},
		startTime:  time.Now(),
		finishChan: make(chan rollerProofStatus, proofAndPkBufferSize),
	}

	// Create a proof generation session.
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()

	dbErr = m.orm.UpdateBlockStatus(id, orm.BlockAssigned)
	go m.CollectProofs(id, s)

	return true
}

// SelectRoller randomly get one idle roller.
func (m *Manager) SelectRoller() *Roller {
	allRollers := m.server.conns.getAll()
	for len(allRollers) > 0 {
		idx := mathrand.Intn(len(allRollers))
		conn := allRollers[idx]
		pk := conn.AuthMsg.Identity.PublicKey
		if conn.isClosed() {
			log.Debug("roller is closed", "public_key", pk)
			// Delete closed connection.
			m.server.conns.delete(conn)
			// Delete the offline roller.
			allRollers[idx], allRollers = allRollers[0], allRollers[1:]
			continue
		}
		// Ensure the roller is not currently working on another session.
		if !m.IsRollerIdle(pk) {
			log.Debug("roller is busy", "public_key", pk)
			// Delete the busy roller.
			allRollers[idx], allRollers = allRollers[0], allRollers[1:]
			continue
		}
		return conn
	}
	return nil
}

// IsRollerIdle determines whether this roller is idle.
func (m *Manager) IsRollerIdle(hexPk string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// We need to iterate over all sessions because finished sessions will be deleted until the
	// timeout. So a busy roller could be marked as idle in a finished session.
	for _, sess := range m.sessions {
		for pk, status := range sess.rollers {
			if pk == hexPk && status == rollerAssigned {
				return false
			}
		}
	}

	return true
}

// GetNumberOfIdleRollers returns the number of idle rollers in maintain list
func (m *Manager) GetNumberOfIdleRollers() int {
	cnt := 0
	// m.server.conns doesn't have any lock
	for _, roller := range m.server.conns.getAll() {
		if m.IsRollerIdle(roller.AuthMsg.Identity.PublicKey) {
			cnt++
		}
	}
	return cnt
}

func createBlockTracesMsg(traces *types.BlockResult) (message.Msg, error) {
	idAndTraces := message.BlockTraces{
		ID:     traces.BlockTrace.Number.ToInt().Uint64(),
		Traces: traces,
	}

	payload, err := json.Marshal(idAndTraces)
	if err != nil {
		return message.Msg{}, err
	}

	return message.Msg{
		Type:    message.BlockTrace,
		Payload: payload,
	}, nil
}

func (m *Manager) addFailedSession(s *session, errMsg string) {
	m.failedSessionInfos[s.id] = *newSessionInfo(s, orm.BlockFailed, errMsg, true)
}