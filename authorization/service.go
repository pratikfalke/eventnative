package authorization

import (
	"errors"
	"github.com/google/uuid"
	"github.com/ksensehq/eventnative/logging"
	"github.com/ksensehq/eventnative/resources"
	"github.com/spf13/viper"
	"strings"
	"sync"
	"time"
)

const (
	serviceName  = "authorization"
	viperAuthKey = "server.auth"

	defaultTokenId = "defaultid"
)

type Service struct {
	sync.RWMutex

	tokensHolder *TokensHolder
}

func NewService() (*Service, error) {
	service := &Service{}

	reloadSec := viper.GetInt("server.auth_reload_sec")
	if reloadSec == 0 {
		return nil, errors.New("server.auth_reload_sec can't be empty")
	}

	var tokens []Token
	err := viper.UnmarshalKey(viperAuthKey, &tokens)
	if err == nil {
		service.tokensHolder = reformat(tokens)
	} else {
		auth := viper.GetStringSlice(viperAuthKey)

		if len(auth) == 1 {
			authSource := auth[0]
			if strings.HasPrefix(authSource, "http://") || strings.HasPrefix(authSource, "https://") {
				resources.Watch(serviceName, authSource, resources.LoadFromHttp, service.updateTokens, time.Duration(reloadSec)*time.Second)
			} else if strings.HasPrefix(authSource, "file://") {
				resources.Watch(serviceName, strings.Replace(authSource, "file://", "", 1), resources.LoadFromFile, service.updateTokens, time.Duration(reloadSec)*time.Second)
			} else if strings.HasPrefix(authSource, "{") && strings.HasSuffix(authSource, "}") {
				tokensHolder, err := parseFromBytes([]byte(authSource))
				if err != nil {
					return nil, err
				}
				service.tokensHolder = tokensHolder
			} else {
				//plain token
				service.tokensHolder = fromStrings(auth)
			}
		} else {
			//array of tokens
			service.tokensHolder = fromStrings(auth)
		}

	}

	if service.tokensHolder.IsEmpty() {
		//autogenerated
		generatedTokenSecret := uuid.New().String()
		generatedToken := Token{
			Id:           defaultTokenId,
			ClientSecret: generatedTokenSecret,
			ServerSecret: generatedTokenSecret,
			Origins:      []string{},
		}

		service.tokensHolder = reformat([]Token{generatedToken})
		logging.Warn("Empty 'server.auth' config keys. Auto generate token:", generatedTokenSecret)
	}

	return service, nil
}

//GetClientOrigins return origins by client_secret
func (s *Service) GetClientOrigins(clientSecret string) ([]string, bool) {
	s.RLock()
	defer s.RUnlock()

	origins, ok := s.tokensHolder.clientTokensOrigins[clientSecret]
	return origins, ok
}

//GetServerOrigins return origins by server_secret
func (s *Service) GetServerOrigins(serverSecret string) ([]string, bool) {
	s.RLock()
	defer s.RUnlock()

	origins, ok := s.tokensHolder.serverTokensOrigins[serverSecret]
	return origins, ok
}

//GetAllTokenIds return all token ids
func (s *Service) GetAllTokenIds() []string {
	s.RLock()
	defer s.RUnlock()

	return s.tokensHolder.ids
}

//GetAllIdsByToken return token ids by token identity(client_secret/server_secret/token id)
func (s *Service) GetAllIdsByToken(tokenIdentity []string) (ids []string) {
	s.RLock()
	defer s.RUnlock()

	deduplication := map[string]bool{}
	for _, tokenFilter := range tokenIdentity {
		tokenObj, _ := s.tokensHolder.all[tokenFilter]
		deduplication[tokenObj.Id] = true
	}

	for id := range deduplication {
		ids = append(ids, id)
	}
	return
}

//GetTokenId return token id by client_secret/server_secret/token id
//return "" if token wasn't found
func (s *Service) GetTokenId(tokenFilter string) string {
	s.RLock()
	defer s.RUnlock()

	token, ok := s.tokensHolder.all[tokenFilter]
	if ok {
		return token.Id
	}
	return ""
}

//parse and set tokensHolder with lock
func (s *Service) updateTokens(payload []byte) {
	tokenHolder, err := parseFromBytes(payload)
	if err != nil {
		logging.Errorf("Error updating authorization tokens: %v", err)
	} else {
		s.Lock()
		s.tokensHolder = tokenHolder
		s.Unlock()
	}
}