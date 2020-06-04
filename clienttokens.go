package remotedialer

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/sirupsen/logrus"
)

const (
	ClientTokenAddress = "client-token-address"
	ClientTokenProto   = "client-token-proto"
)

var (
	tokenGetter = func() (string, error) {
		return "test", nil
	}

	cacertGetter = func() (string, error) {
		return "test", nil
	}
)

type clientTokenGetter func() (string, error)

func RegisterTokenGetter(tokenGet, cacertGet clientTokenGetter) {
	if tokenGet != nil {
		tokenGetter = tokenGet
	}
	if cacertGet != nil {
		cacertGetter = cacertGet
	}
}

type clientToken struct {
	token  string
	cacert string
}

func writeClientToken(conn *connection, message *message) error {
	defer conn.Close()

	token, err := tokenGetter()
	if err != nil {
		return err
	}

	cacert, err := cacertGetter()
	if err != nil {
		return err
	}

	ct := &clientToken{
		token:  token,
		cacert: cacert,
	}

	ctbytes, err := json.Marshal(ct)
	if err != nil {
		return err
	}

	conn.Write(ctbytes)
	if err != nil {
		return err
	}

	return nil
}

func getClientToken(s *Session, prefix string) (*clientToken, error) {
	connID := atomic.AddInt64(&s.nextConnID, 1)
	conn := newConnection(connID, s, "", "")

	s.Lock()
	s.conns[connID] = conn
	if PrintTunnelData {
		logrus.Debugf("CONNECTIONS %d %d", s.sessionKey, len(s.conns))
	}
	s.Unlock()

	var err error
	if prefix == "" {
		_, err = s.writeMessage(newClientToken(connID, 3*time.Second, ClientTokenProto, ClientTokenAddress))

	} else {
		_, err = s.writeMessage(newClientToken(connID, 3*time.Second, prefix+"::"+ClientTokenProto, ClientTokenAddress))
	}

	if err != nil {
		s.closeConnection(connID, err)
		return nil, err
	}

	data, err := ioutil.ReadAll(conn)
	if err != nil {
		s.closeConnection(connID, err)
		return nil, err
	}

	ct := &clientToken{}
	if err := json.Unmarshal(data, ct); err != nil {
		s.closeConnection(connID, err)
		return nil, err
	}

	return ct, nil
}

type clientTokenCache struct {
	cache *lru.Cache
	len   int
}

func newClientTokenCache(len int) *clientTokenCache {
	cache, _ := lru.New(len)
	return &clientTokenCache{
		cache: cache,
		len:   len,
	}
}

func (ctc *clientTokenCache) get(key string) (*clientToken, error) {
	v, ok := ctc.cache.Get(key)
	if !ok {
		return nil, fmt.Errorf("key %s not found", key)
	}

	ct, ok := v.(clientToken)
	if !ok {
		return nil, fmt.Errorf("value type not clientToken")
	}

	return &ct, nil
}

func (ctc *clientTokenCache) add(key string, value clientToken) {
	if ctc.cache.Contains(key) {
		ctc.cache.Remove(key)
	}

	ctc.cache.Add(key, value)
}

func (ctc *clientTokenCache) contains(key string) bool {
	return ctc.cache.Contains(key)
}
