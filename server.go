package remotedialer

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var (
	errFailedAuth       = errors.New("failed authentication")
	errWrongMessageType = errors.New("wrong websocket message type")
)

type Authorizer func(req *http.Request) (clientKey string, authed bool, err error)
type ErrorWriter func(rw http.ResponseWriter, req *http.Request, code int, err error)

func DefaultErrorWriter(rw http.ResponseWriter, req *http.Request, code int, err error) {
	rw.Write([]byte(err.Error()))
	rw.WriteHeader(code)
}

type Server struct {
	PeerID      string
	PeerToken   string
	authorizer  Authorizer
	errorWriter ErrorWriter
	sessions    *sessionManager
	peers       map[string]peer
	peerLock    sync.Mutex
}

func New(tokenCacheLen int, auth Authorizer, errorWriter ErrorWriter) *Server {

	s := &Server{
		peers:       map[string]peer{},
		authorizer:  auth,
		errorWriter: errorWriter,
		sessions:    newSessionManager(),
	}

	if tokenCacheLen > 0 {
		s.sessions.setTokenCache(tokenCacheLen)
	}

	return s
}

func (s *Server) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	clientKey, authed, peer, err := s.auth(req)
	logrus.Debugf("------ServeHTTP auth clientKey [%v],  authed [%v], peer [%v], [err] [%v]",
		clientKey, authed, peer, err)

	if err != nil {
		s.errorWriter(rw, req, 400, err)
		return
	}
	if !authed {
		s.errorWriter(rw, req, 401, errFailedAuth)
		return
	}

	logrus.Infof("Handling backend connection request [%s]", clientKey)

	upgrader := websocket.Upgrader{
		HandshakeTimeout: 5 * time.Second,
		CheckOrigin:      func(r *http.Request) bool { return true },
		Error:            s.errorWriter,
	}

	wsConn, err := upgrader.Upgrade(rw, req, nil)
	if err != nil {
		logrus.Errorf("Error during upgrade for host [%v] [%v]\n", clientKey, err)
		s.errorWriter(rw, req, 400, errors.Wrapf(err, "Error during upgrade for host [%v]", clientKey))
		return
	}

	session := s.sessions.add(clientKey, wsConn, peer)
	defer s.sessions.remove(session)

	// Don't need to associate req.Context() to the Session, it will cancel otherwise
	code, err := session.Serve(context.Background())
	if err != nil {
		// Hijacked so we can't write to the client
		logrus.Infof("error in remotedialer server [%d]: %v", code, err)
	}
}

func (s *Server) auth(req *http.Request) (clientKey string, authed, peer bool, err error) {
	id := req.Header.Get(ID)
	token := req.Header.Get(Token)
	logrus.Debugf("---------Auth id [%s], token [%s]", id, token)
	if id != "" && token != "" {
		// peer authentication
		s.peerLock.Lock()
		p, ok := s.peers[id]
		s.peerLock.Unlock()

		if ok && p.token == token {
			return id, true, true, nil
		}
	}

	id, authed, err = s.authorizer(req)
	return id, authed, false, err
}

func (s *Server) GetClusterToken(clientKey string, deadline time.Duration) (*clientToken, error) {
	if s.sessions.tokenCache.contains(clientKey) {
		ct, err := s.sessions.tokenCache.get(clientKey)
		if err != nil {
			return nil, err
		}

		return ct, nil
	}

	conn, err := s.DialWithClientToken(clientKey, deadline, ClientTokenProto, ClientTokenAddress)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	data, err := ioutil.ReadAll(conn)
	if err != nil {
		return nil, err
	}

	ct := &clientToken{}
	if err := json.Unmarshal(data, ct); err != nil {
		return nil, err
	}

	if ct.Token == "" || ct.Cacert == "" {
		return nil, fmt.Errorf("cluster %s token or cacert not found", clientKey)
	}

	s.sessions.tokenCache.add(clientKey, *ct)
	return ct, nil
}
