// Package servercore is the core of the IRMA server library, allowing IRMA verifiers, issuers
// or attribute-based signature applications to perform IRMA sessions with irmaclient instances
// (i.e. the IRMA app). It exposes a small interface to expose to other programming languages
// through cgo. It is used by the irmaserver package but otherwise not meant for use in Go.
package servercore

import (
	"encoding/json"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/go-errors/errors"
	"github.com/jasonlvhit/gocron"
	"github.com/privacybydesign/gabi"
	"github.com/privacybydesign/gabi/big"
	"github.com/privacybydesign/irmago"
	"github.com/privacybydesign/irmago/server"
	"github.com/sirupsen/logrus"
)

type Server struct {
	conf      *server.Configuration
	sessions  sessionStore
	scheduler *gocron.Scheduler
}

func New(conf *server.Configuration) (*Server, error) {
	s := &Server{
		conf:      conf,
		scheduler: gocron.NewScheduler(),
		sessions: &memorySessionStore{
			m:    make(map[string]*session),
			conf: conf,
		},
	}
	s.scheduler.Every(10).Seconds().Do(func() {
		s.sessions.deleteExpired()
	})
	s.scheduler.Start()

	return s, s.verifyConfiguration(s.conf)
}

func (s *Server) verifyConfiguration(configuration *server.Configuration) error {
	if s.conf.Logger == nil {
		s.conf.Logger = logrus.New()
		s.conf.Logger.Level = logrus.DebugLevel
		s.conf.Logger.Formatter = &logrus.TextFormatter{}
	}
	server.Logger = s.conf.Logger
	irma.Logger = s.conf.Logger

	if s.conf.IrmaConfiguration == nil {
		var err error
		if s.conf.SchemesAssetsPath == "" {
			s.conf.IrmaConfiguration, err = irma.NewConfiguration(s.conf.SchemesPath)
		} else {
			s.conf.IrmaConfiguration, err = irma.NewConfigurationFromAssets(s.conf.SchemesPath, s.conf.SchemesAssetsPath)
		}
		if err != nil {
			return server.LogError(err)
		}
		if err = s.conf.IrmaConfiguration.ParseFolder(); err != nil {
			return server.LogError(err)
		}
	}

	if len(s.conf.IrmaConfiguration.SchemeManagers) == 0 {
		if s.conf.DownloadDefaultSchemes {
			if err := s.conf.IrmaConfiguration.DownloadDefaultSchemes(); err != nil {
				return server.LogError(err)
			}
		} else {
			return server.LogError(errors.New("no schemes found in irma_configuration folder " + s.conf.IrmaConfiguration.Path))
		}
	}
	if s.conf.SchemeUpdateInterval != 0 {
		s.conf.IrmaConfiguration.AutoUpdateSchemes(uint(s.conf.SchemeUpdateInterval))
	}

	if s.conf.IssuerPrivateKeys == nil {
		s.conf.IssuerPrivateKeys = make(map[irma.IssuerIdentifier]*gabi.PrivateKey)
	}
	if s.conf.IssuerPrivateKeysPath != "" {
		files, err := ioutil.ReadDir(s.conf.IssuerPrivateKeysPath)
		if err != nil {
			return server.LogError(err)
		}
		for _, file := range files {
			filename := file.Name()
			issid := irma.NewIssuerIdentifier(strings.TrimSuffix(filename, filepath.Ext(filename))) // strip .xml
			if _, ok := s.conf.IrmaConfiguration.Issuers[issid]; !ok {
				return server.LogError(errors.Errorf("Private key %s belongs to an unknown issuer", filename))
			}
			sk, err := gabi.NewPrivateKeyFromFile(filepath.Join(s.conf.IssuerPrivateKeysPath, filename))
			if err != nil {
				return server.LogError(err)
			}
			s.conf.IssuerPrivateKeys[issid] = sk
		}
	}
	for issid, sk := range s.conf.IssuerPrivateKeys {
		pk, err := s.conf.IrmaConfiguration.PublicKey(issid, int(sk.Counter))
		if err != nil {
			return server.LogError(err)
		}
		if pk == nil {
			return server.LogError(errors.Errorf("Missing public key belonging to private key %s-%d", issid.String(), sk.Counter))
		}
		if new(big.Int).Mul(sk.P, sk.Q).Cmp(pk.N) != 0 {
			return server.LogError(errors.Errorf("Private key %s-%d does not belong to corresponding public key", issid.String(), sk.Counter))
		}
	}

	if s.conf.URL != "" {
		if !strings.HasSuffix(s.conf.URL, "/") {
			s.conf.URL = s.conf.URL + "/"
		}
	} else {
		s.conf.Logger.Warn("No url parameter specified in configuration; unless an url is elsewhere prepended in the QR, the IRMA client will not be able to connect")
	}

	if s.conf.Email != "" {
		// Very basic sanity checks
		if !strings.Contains(s.conf.Email, "@") || strings.Contains(s.conf.Email, "\n") {
			return server.LogError(errors.New("Invalid email address specified"))
		}
		t := irma.NewHTTPTransport("https://metrics.privacybydesign.foundation/history/email")
		t.SetHeader("User-Agent", "irmaserver")
		var x string
		_ = t.Post("", &x, s.conf.Email)
	}

	return nil
}

func (s *Server) StartSession(req interface{}) (*irma.Qr, string, error) {
	rrequest, err := server.ParseSessionRequest(req)
	if err != nil {
		return nil, "", err
	}

	request := rrequest.SessionRequest()
	action := request.Action()
	if action == irma.ActionIssuing {
		if err := s.validateIssuanceRequest(request.(*irma.IssuanceRequest)); err != nil {
			return nil, "", err
		}
	}

	session := s.newSession(action, rrequest)
	s.conf.Logger.WithFields(logrus.Fields{"action": action, "session": session.token}).Infof("Session started")
	if s.conf.Logger.IsLevelEnabled(logrus.DebugLevel) {
		s.conf.Logger.WithFields(logrus.Fields{"session": session.token}).Info("Session request: ", server.ToJson(rrequest))
	} else {
		s.conf.Logger.WithFields(logrus.Fields{"session": session.token}).Info("Session request (purged of attribute values): ", server.ToJson(purgeRequest(rrequest)))
	}
	return &irma.Qr{
		Type: action,
		URL:  s.conf.URL + session.token,
	}, session.token, nil
}

func (s *Server) GetSessionResult(token string) *server.SessionResult {
	session := s.sessions.get(token)
	if session == nil {
		s.conf.Logger.Warn("Session result requested of unknown session ", token)
		return nil
	}
	return session.result
}

func (s *Server) GetRequest(token string) irma.RequestorRequest {
	session := s.sessions.get(token)
	if session == nil {
		s.conf.Logger.Warn("Session request requested of unknown session ", token)
		return nil
	}
	return session.rrequest
}

func (s *Server) CancelSession(token string) error {
	session := s.sessions.get(token)
	if session == nil {
		return server.LogError(errors.Errorf("can't cancel unknown session %s", token))
	}
	session.handleDelete()
	return nil
}

func ParsePath(path string) (string, string, error) {
	pattern := regexp.MustCompile("(\\w+)/?(|commitments|proofs|status|statusevents)$")
	matches := pattern.FindStringSubmatch(path)
	if len(matches) != 3 {
		return "", "", server.LogWarning(errors.Errorf("Invalid URL: %s", path))
	}
	return matches[1], matches[2], nil
}

func (s *Server) SubscribeServerSentEvents(w http.ResponseWriter, r *http.Request, token string) error {
	session := s.sessions.get(token)
	if session == nil {
		return server.LogError(errors.Errorf("can't subscribe to server sent events of unknown session %s", token))
	}
	if session.status.Finished() {
		return server.LogError(errors.Errorf("can't subscribe to server sent events of finished session %s", token))
	}

	session.Lock()
	defer session.Unlock()
	session.eventSource().ServeHTTP(w, r)
	return nil
}

func (s *Server) HandleProtocolMessage(
	path string,
	method string,
	headers map[string][]string,
	message []byte,
) (status int, output []byte, result *server.SessionResult) {
	// Parse path into session and action
	if len(path) > 0 { // Remove any starting and trailing slash
		if path[0] == '/' {
			path = path[1:]
		}
		if path[len(path)-1] == '/' {
			path = path[:len(path)-1]
		}
	}

	s.conf.Logger.WithFields(logrus.Fields{"method": method, "path": path}).Debugf("Routing protocol message")
	if len(message) > 0 {
		s.conf.Logger.Trace("POST body: ", string(message))
	}
	s.conf.Logger.Trace("HTTP headers: ", server.ToJson(headers))
	token, noun, err := ParsePath(path)
	if err != nil {
		status, output = server.JsonResponse(nil, server.RemoteError(server.ErrorUnsupported, ""))
		return
	}

	// Fetch the session
	session := s.sessions.get(token)
	if session == nil {
		s.conf.Logger.Warnf("Session not found: %s", token)
		status, output = server.JsonResponse(nil, server.RemoteError(server.ErrorSessionUnknown, ""))
		return
	}
	session.Lock()
	defer session.Unlock()

	// However we return, if the session status has been updated
	// then we should inform the user by returning a SessionResult
	defer func() {
		if session.status != session.prevStatus {
			session.prevStatus = session.status
			result = session.result
		}
	}()

	// Route to handler
	switch len(noun) {
	case 0:
		if method == http.MethodDelete {
			session.handleDelete()
			status = http.StatusOK
			return
		}
		if method == http.MethodGet {
			h := http.Header(headers)
			min := &irma.ProtocolVersion{}
			max := &irma.ProtocolVersion{}
			if err := json.Unmarshal([]byte(h.Get(irma.MinVersionHeader)), min); err != nil {
				status, output = server.JsonResponse(nil, session.fail(server.ErrorMalformedInput, err.Error()))
				return
			}
			if err := json.Unmarshal([]byte(h.Get(irma.MaxVersionHeader)), max); err != nil {
				status, output = server.JsonResponse(nil, session.fail(server.ErrorMalformedInput, err.Error()))
				return
			}
			status, output = server.JsonResponse(session.handleGetRequest(min, max))
			return
		}
		status, output = server.JsonResponse(nil, session.fail(server.ErrorInvalidRequest, ""))
		return
	default:
		if noun == "statusevents" {
			err := server.RemoteError(server.ErrorInvalidRequest, "server sent events not supported by this server")
			status, output = server.JsonResponse(nil, err)
			return
		}

		if method == http.MethodGet && noun == "status" {
			status, output = server.JsonResponse(session.handleGetStatus())
			return
		}

		// Below are only POST enpoints
		if method != http.MethodPost {
			status, output = server.JsonResponse(nil, session.fail(server.ErrorInvalidRequest, ""))
			return
		}

		if noun == "commitments" && session.action == irma.ActionIssuing {
			commitments := &irma.IssueCommitmentMessage{}
			if err := irma.UnmarshalValidate(message, commitments); err != nil {
				status, output = server.JsonResponse(nil, session.fail(server.ErrorMalformedInput, ""))
				return
			}
			status, output = server.JsonResponse(session.handlePostCommitments(commitments))
			return
		}
		if noun == "proofs" && session.action == irma.ActionDisclosing {
			disclosure := irma.Disclosure{}
			if err := irma.UnmarshalValidate(message, &disclosure); err != nil {
				status, output = server.JsonResponse(nil, session.fail(server.ErrorMalformedInput, ""))
				return
			}
			status, output = server.JsonResponse(session.handlePostDisclosure(disclosure))
			return
		}
		if noun == "proofs" && session.action == irma.ActionSigning {
			signature := &irma.SignedMessage{}
			if err := irma.UnmarshalValidate(message, signature); err != nil {
				status, output = server.JsonResponse(nil, session.fail(server.ErrorMalformedInput, ""))
				return
			}
			status, output = server.JsonResponse(session.handlePostSignature(signature))
			return
		}

		status, output = server.JsonResponse(nil, session.fail(server.ErrorInvalidRequest, ""))
		return
	}
}
