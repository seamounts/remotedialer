package remotedialer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/seamounts/remotedialer/internal/netpool"
	"github.com/sirupsen/logrus"
)

type Session struct {
	sync.Mutex

	nextConnID       int64
	clientKey        string
	sessionKey       int64
	conn             *wsConn
	conns            map[int64]*connection
	remoteClientKeys map[string]map[int]bool
	auth             ConnectAuthorizer
	pingCancel       context.CancelFunc
	pingWait         sync.WaitGroup
	dialer           Dialer
	client           bool
	sm               *sessionManager

	netConnPools map[string]net.Conn
}

// PrintTunnelData No tunnel logging by default
var PrintTunnelData bool

func init() {
	if os.Getenv("CATTLE_TUNNEL_DATA_DEBUG") == "true" {
		PrintTunnelData = true
	}
}

func NewClientSession(auth ConnectAuthorizer, conn *websocket.Conn) *Session {
	return &Session{
		clientKey: "client",
		conn:      newWSConn(conn),
		conns:     map[int64]*connection{},
		auth:      auth,
		client:    true,
	}
}

func newSession(sessionKey int64, clientKey string, conn *websocket.Conn) *Session {
	return &Session{
		nextConnID:       1,
		clientKey:        clientKey,
		sessionKey:       sessionKey,
		conn:             newWSConn(conn),
		conns:            map[int64]*connection{},
		remoteClientKeys: map[string]map[int]bool{},
	}
}

func (s *Session) startPings(rootCtx context.Context) {
	ctx, cancel := context.WithCancel(rootCtx)
	s.pingCancel = cancel
	s.pingWait.Add(1)

	go func() {
		defer s.pingWait.Done()

		t := time.NewTicker(PingWriteInterval)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.conn.Lock()
				if err := s.conn.conn.WriteControl(websocket.PingMessage, []byte(""), time.Now().Add(time.Second)); err != nil {
					logrus.WithError(err).Error("Error writing ping")
				}
				logrus.Debug("Wrote ping")
				s.conn.Unlock()
			}
		}
	}()
}

func (s *Session) stopPings() {
	if s.pingCancel == nil {
		return
	}

	s.pingCancel()
	s.pingWait.Wait()
}

func (s *Session) Serve(ctx context.Context) (int, error) {
	if s.client {
		s.startPings(ctx)
	}

	for {
		msType, reader, err := s.conn.NextReader()
		if err != nil {
			return 400, err
		}

		if msType != websocket.BinaryMessage {
			return 400, errWrongMessageType
		}

		if err := s.serveMessage(reader); err != nil {
			return 500, err
		}
	}
}

func (s *Session) serveMessage(reader io.Reader) error {
	message, err := newServerMessage(reader)
	if err != nil {
		return err
	}

	if PrintTunnelData {
		logrus.Debug("REQUEST ", message)
	}

	if message.messageType == TokenConnect {
		if s.auth == nil || !s.auth(message.proto, message.address) {
			return errors.New("clientConnectWithToken not allowed")
		}
		s.clientTokenConnect(message)
		return nil
	}

	if message.messageType == Connect {
		if s.auth == nil || !s.auth(message.proto, message.address) {
			return errors.New("connect not allowed")
		}
		s.clientConnect(message)
		return nil
	}

	s.Lock()
	if message.messageType == AddClient && s.remoteClientKeys != nil {
		err := s.addRemoteClient(message.address)
		s.Unlock()
		return err
	} else if message.messageType == RemoveClient {
		err := s.removeRemoteClient(message.address)
		s.Unlock()
		return err
	}
	conn := s.conns[message.connID]
	s.Unlock()

	if conn == nil {
		if message.messageType == Data {
			err := fmt.Errorf("connection not found %s/%d/%d", s.clientKey, s.sessionKey, message.connID)
			newErrorMessage(message.connID, err).WriteTo(s.conn)
		}
		return nil
	}

	switch message.messageType {
	case Data:
		if _, err := io.Copy(conn.tunnelWriter(), message); err != nil {
			s.closeConnection(message.connID, err)
		}
	case Error:
		s.closeConnection(message.connID, message.Err())
	}

	return nil
}

func parseAddress(address string) (string, int, error) {
	parts := strings.SplitN(address, "/", 2)
	if len(parts) != 2 {
		return "", 0, errors.New("not / separated")
	}
	v, err := strconv.Atoi(parts[1])
	return parts[0], v, err
}

func (s *Session) addRemoteClient(address string) error {
	clientKey, sessionKey, err := parseAddress(address)
	if err != nil {
		return fmt.Errorf("invalid remote Session %s: %v", address, err)
	}

	keys := s.remoteClientKeys[clientKey]
	if keys == nil {
		keys = map[int]bool{}
		s.remoteClientKeys[clientKey] = keys
	}
	keys[int(sessionKey)] = true

	logrus.Infof("ADD REMOTE CLIENT %s, SESSION %d", address, s.sessionKey)
	if PrintTunnelData {
		logrus.Debugf("ADD REMOTE CLIENT %s, SESSION %d", address, s.sessionKey)
	}

	return nil
}

func (s *Session) removeRemoteClient(address string) error {
	clientKey, sessionKey, err := parseAddress(address)
	if err != nil {
		return fmt.Errorf("invalid remote Session %s: %v", address, err)
	}

	keys := s.remoteClientKeys[clientKey]
	delete(keys, int(sessionKey))
	if len(keys) == 0 {
		delete(s.remoteClientKeys, clientKey)
	}
	// remove tokenCache
	s.sm.tokenCache.remove(clientKey)

	logrus.Infof("REMOVE REMOTE CLIENT %s, SESSION %d", address, s.sessionKey)
	if PrintTunnelData {
		logrus.Debugf("REMOVE REMOTE CLIENT %s, SESSION %d", address, s.sessionKey)
	}

	return nil
}

func (s *Session) closeConnection(connID int64, err error) {
	s.Lock()
	conn := s.conns[connID]
	delete(s.conns, connID)
	if PrintTunnelData {
		logrus.Debugf("CONNECTIONS %d %d", s.sessionKey, len(s.conns))
	}
	s.Unlock()

	if conn != nil {
		conn.tunnelClose(err)
	}
}

func (s *Session) clientConnect(message *message) {
	conn := newConnection(message.connID, s, message.proto, message.address)

	s.Lock()
	s.conns[message.connID] = conn
	if PrintTunnelData {
		logrus.Debugf("CONNECTIONS %d %d", s.sessionKey, len(s.conns))
	}
	s.Unlock()

	dialer := s.dialer
	if dialer == nil {
		dialer = func(network, address string) (net.Conn, error) {
			destaddr := fmt.Sprintf("%s:%s", network, address)
			s.Lock()
			netconn, ok := s.netConnPools[destaddr]
			s.Unlock()
			if !ok {
				netconn, err := net.DialTimeout(network, address, time.Duration(message.deadline)*time.Millisecond)
				if err != nil {
					return nil, err
				}
				s.Lock()
				s.netConnPools[destaddr] = netpool.WrapConn(netconn)
				s.Unlock()
			}

			return netconn, nil
		}
	}

	go clientDial(dialer, conn, message)
}

func (s *Session) clientTokenConnect(message *message) {
	var (
		netConn net.Conn
		err     error
	)

	conn := newConnection(message.connID, s, message.proto, message.address)
	defer conn.Close()

	s.Lock()
	s.conns[message.connID] = conn
	if PrintTunnelData {
		logrus.Debugf("CLIENT TOKEN CONNECTIONS %d %d", s.sessionKey, len(s.conns))
	}
	s.Unlock()

	if s.dialer != nil {

		netConn, err = s.dialer(message.proto, message.address)
		if err != nil {
			conn.tunnelClose(err)
			return
		}

		close := func(err error) error {
			if err == nil {
				err = io.EOF
			}
			conn.doTunnelClose(err)
			netConn.Close()
			return err
		}
		_, err = io.Copy(conn, netConn)
		if err := close(err); err != nil {
			conn.writeErr(err)
		}

	} else {
		err := writeClientToken(s, conn.connID, message.deadline)
		if err != nil {
			s.closeConnection(conn.connID, err)
		}
	}
}

func (s *Session) serverConnect(deadline time.Duration, proto, address string) (net.Conn, error) {
	connID := atomic.AddInt64(&s.nextConnID, 1)
	conn := newConnection(connID, s, proto, address)

	s.Lock()
	s.conns[connID] = conn
	if PrintTunnelData {
		logrus.Debugf("CONNECTIONS %d %d", s.sessionKey, len(s.conns))
	}
	s.Unlock()

	if _, err := s.writeMessage(newConnect(connID, deadline, proto, address)); err != nil {
		s.closeConnection(connID, err)
		return nil, err
	}

	return conn, nil
}

func (s *Session) serverTokenConnect(deadline time.Duration, proto, address string) (net.Conn, error) {
	connID := atomic.AddInt64(&s.nextConnID, 1)
	conn := newConnection(connID, s, proto, address)

	s.Lock()
	s.conns[connID] = conn
	if PrintTunnelData {
		logrus.Debugf("TOKEN CONNECTIONS %d %d", s.sessionKey, len(s.conns))
	}
	s.Unlock()

	if _, err := s.writeMessage(newTokenConnect(connID, deadline, proto, address)); err != nil {
		s.closeConnection(connID, err)
		return nil, err
	}

	return conn, nil
}

func (s *Session) writeMessage(message *message) (int, error) {
	if PrintTunnelData {
		logrus.Debug("WRITE ", message)
	}
	return message.WriteTo(s.conn)
}

func (s *Session) Close() {
	s.Lock()
	defer s.Unlock()

	s.stopPings()

	for _, connection := range s.conns {
		connection.tunnelClose(errors.New("tunnel disconnect"))
	}

	s.conns = map[int64]*connection{}

	for _, netconn := range s.netConnPools {
		if pc, ok := netconn.(*netpool.NetConn); ok {
			pc.MarkUnusable()
			pc.Close()
		} else {
			netconn.Close()
		}
	}
	s.netConnPools = map[string]net.Conn{}
}

func (s *Session) sessionAdded(clientKey string, sessionKey int64) {
	client := fmt.Sprintf("%s/%d", clientKey, sessionKey)
	_, err := s.writeMessage(newAddClient(client))
	if err != nil {
		s.conn.conn.Close()
	}
}

func (s *Session) sessionRemoved(clientKey string, sessionKey int64) {
	client := fmt.Sprintf("%s/%d", clientKey, sessionKey)
	_, err := s.writeMessage(newRemoveClient(client))
	if err != nil {
		s.conn.conn.Close()
	}
}
