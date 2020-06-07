package remotedialer

import (
	"encoding/json"
	"fmt"

	lru "github.com/hashicorp/golang-lru"
	"github.com/sirupsen/logrus"
)

var (
	ClientTokenProto   = "TokenRegister"
	ClientTokenAddress = "ClientTokenAddress"
)

var (
	tokenGetter = func() (string, error) {
		return "testtoken", nil
	}

	cacertGetter = func() (string, error) {
		return "testcacert", nil
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
	Token  string
	Cacert string
}

func writeClientToken(s *Session, connID, deadline int64) error {
	logrus.Debugln("-------writeClientToken", s.clientKey, connID)
	var (
		ct  *clientToken
		err error
	)

	token, err := tokenGetter()
	if err != nil {
		return err
	}

	cacert, err := cacertGetter()
	if err != nil {
		return err
	}

	ct = &clientToken{
		Token:  token,
		Cacert: cacert,
	}

	ctbytes, err := json.Marshal(ct)
	if err != nil {
		return err
	}

	if _, err := s.writeMessage(newMessage(connID, deadline, ctbytes)); err != nil {
		return err
	}

	return nil
}

type clientTokenCache struct {
	cache *lru.Cache
	len   int
}

func newClientTokenCache(len int) *clientTokenCache {
	cache, err := lru.New(len)
	if err != nil {
		logrus.Errorln(err)
	}
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

func (ctc *clientTokenCache) remove(key string) {
	if ctc.cache.Contains(key) {
		logrus.Debugf("---------Remove Client %s Token Cache ", key)
		ctc.cache.Remove(key)
	}
}

func (ctc *clientTokenCache) contains(key string) bool {
	return ctc.cache.Contains(key)
}

func (ctc *clientTokenCache) resize(len int) {
	ctc.cache.Resize(len)
}
