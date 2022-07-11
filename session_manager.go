package webtransport

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"

	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/http3"
	"github.com/lucas-clemente/quic-go/quicvarint"
	"github.com/marten-seemann/webtransport-go/internal/logging"
)

// sessionKey is used as a map key in the sessions map
type sessionKey struct {
	qconn http3.StreamCreator
	id    sessionID
}

// session is the map value in the sessions map
type session struct {
	created chan struct{} // is closed once the session map has been initialized
	counter int           // how many streams are waiting for this session to be established
	conn    *Conn
}

type sessionManager struct {
	logger    logging.Logger
	refCount  sync.WaitGroup
	ctx       context.Context
	ctxCancel context.CancelFunc

	timeout time.Duration

	mx       sync.Mutex
	sessions map[sessionKey]*session

	// conns unique list for handling datagram
	conns map[http3.StreamCreator]struct{}
}

func newSessionManager(logger logging.Logger, timeout time.Duration) *sessionManager {
	m := &sessionManager{
		logger:   logger,
		timeout:  timeout,
		sessions: make(map[sessionKey]*session),
		conns:    make(map[http3.StreamCreator]struct{}),
	}
	m.ctx, m.ctxCancel = context.WithCancel(context.Background())
	return m
}

// AddStream adds a new stream to a WebTransport session.
// If the WebTransport session has not yet been established,
// it starts a new go routine and waits for establishment of the session.
// If that takes longer than timeout, the stream is reset.
func (m *sessionManager) AddStream(qconn http3.StreamCreator, str quic.Stream, id sessionID) {
	key := sessionKey{qconn: qconn, id: id}

	m.mx.Lock()
	defer m.mx.Unlock()

	sess, ok := m.sessions[key]
	if ok && sess.conn != nil {
		sess.conn.addStream(str)
		return
	}
	if !ok {
		sess = &session{created: make(chan struct{})}
		m.sessions[key] = sess
	}
	sess.counter++

	m.refCount.Add(1)
	go func() {
		defer m.refCount.Done()
		m.handleStream(str, sess, key)
	}()
}

func (m *sessionManager) AddUniStream(qconn http3.StreamCreator, str quic.ReceiveStream, id sessionID) {
	key := sessionKey{qconn: qconn, id: id}

	m.mx.Lock()
	defer m.mx.Unlock()

	sess, ok := m.sessions[key]
	if ok && sess.conn != nil {
		sess.conn.addUniStream(str)
		return
	}
	if !ok {
		sess = &session{created: make(chan struct{})}
		m.sessions[key] = sess
	}
	sess.counter++

	m.refCount.Add(1)
	go func() {
		defer m.refCount.Done()
		m.handleUniStream(str, sess, key)
	}()
}

func (m *sessionManager) handleStream(str quic.Stream, session *session, key sessionKey) {
	t := time.NewTimer(m.timeout)
	defer t.Stop()

	// When multiple streams are waiting for the same session to be established,
	// the timeout is calculated for every stream separately.
	select {
	// case <-session.conn.ctx.Done():
	case <-session.created:
		session.conn.addStream(str)
	case <-t.C:
		str.CancelRead(WebTransportBufferedStreamRejectedErrorCode)
		str.CancelWrite(WebTransportBufferedStreamRejectedErrorCode)
	case <-m.ctx.Done():
	}

	m.mx.Lock()
	defer m.mx.Unlock()

	session.counter--
	// Once no more streams are waiting for this session to be established,
	// and this session is still outstanding, delete it from the map.
	if session.counter == 0 && session.conn == nil {
		delete(m.sessions, key)
	}
}

func (m *sessionManager) handleUniStream(str quic.ReceiveStream, session *session, key sessionKey) {
	t := time.NewTimer(m.timeout)
	defer t.Stop()

	// When multiple streams are waiting for the same session to be established,
	// the timeout is calculated for every stream separately.
	select {
	case <-session.created:
		session.conn.addUniStream(str)
	case <-t.C:
		str.CancelRead(WebTransportBufferedStreamRejectedErrorCode)
	case <-m.ctx.Done():
	}

	m.mx.Lock()
	defer m.mx.Unlock()

	session.counter--
	// Once no more streams are waiting for this session to be established,
	// and this session is still outstanding, delete it from the map.
	if session.counter == 0 && session.conn == nil {
		delete(m.sessions, key)
	}
}

// AddSession adds a new WebTransport session.
func (m *sessionManager) AddSession(qconn http3.StreamCreator, id sessionID, conn *Conn) {
	m.mx.Lock()
	defer m.mx.Unlock()

	key := sessionKey{qconn: qconn, id: id}
	if sess, ok := m.sessions[key]; ok {
		sess.conn = conn
		close(sess.created)
		go m.handleDatagram(qconn)
		return
	}
	c := make(chan struct{})
	close(c)
	m.sessions[key] = &session{created: c, conn: conn}

	if _, ok := m.conns[qconn]; !ok {
		m.conns[qconn] = struct{}{}
		go m.handleDatagram(qconn)
	}
}

func (m *sessionManager) handleDatagram(qconn http3.StreamCreator) {
	for {
		select {
		case <-m.ctx.Done():
			m.logger.Debugf("datagram handling done in a one quic connection.")
			return
		default:
		}

		data, err := qconn.(quic.Connection).ReceiveMessage()
		if err != nil {
			m.logger.Debugf("ReceiveMessage from quic.Connection failed: %s", err)
			return
		}
		if len(data) == 0 {
			m.logger.Infof("got empty datagram message")
			return
		}

		r := quicvarint.NewReader(bytes.NewReader(data))
		v, err := quicvarint.Read(r)
		if err != nil {
			m.logger.Errorf("reading session id failed: %s", err)
			continue
		}
		sessionID := sessionID(v)
		key := sessionKey{qconn: qconn, id: sessionID}
		if sess, ok := m.sessions[key]; ok {
			appdata, err := io.ReadAll(r)
			if err != nil {
				m.logger.Errorf("failed to read all datagram message: %+v", err)
				return
			}
			m.logger.Debugf("Datagram %dbytes received on session id %d", len(appdata), sessionID)
			sess.conn.handleDatagram(appdata)
		}
	}
}

func (m *sessionManager) Close() {
	m.ctxCancel()
	m.refCount.Wait()
}
