package remotedialer

import (
	"net"
	"time"
)

type Dialer func(network, address string) (net.Conn, error)

func (s *Server) HasSession(clientKey string) bool {
	_, err := s.sessions.getDialer(clientKey, 0)
	return err == nil
}

func (s *Server) CloseSession(clientKey string) {
	sessions := s.sessions.clients[clientKey]
	if len(sessions) == 0 {
		return
	}

	for _, session := range sessions {
		session.Close()
		session.conn.conn.Close()
	}

	delete(s.sessions.clients, clientKey)
}

func (s *Server) Dial(clientKey string, deadline time.Duration, proto, address string) (net.Conn, error) {
	d, err := s.sessions.getDialer(clientKey, deadline)
	if err != nil {
		return nil, err
	}

	return d(proto, address)
}

func (s *Server) DialWithClientToken(clientKey string, deadline time.Duration, proto, address string) (net.Conn, error) {
	return s.sessions.getClientTokenConn(clientKey, deadline, proto, address)
}

func (s *Server) Dialer(clientKey string, deadline time.Duration) Dialer {
	return func(proto, address string) (net.Conn, error) {
		return s.Dial(clientKey, deadline, proto, address)
	}
}
