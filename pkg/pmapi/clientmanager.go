package pmapi

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/getsentry/raven-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

var defaultProxyUseDuration = 24 * time.Hour

// ClientManager is a manager of clients.
type ClientManager struct {
	config       *ClientConfig
	roundTripper http.RoundTripper

	clients       map[string]*Client
	clientsLocker sync.Locker

	tokens       map[string]string
	tokensLocker sync.Locker

	expirations       map[string]*tokenExpiration
	expirationsLocker sync.Locker

	host, scheme string
	hostLocker   sync.Locker

	bridgeAuths chan ClientAuth
	clientAuths chan ClientAuth

	allowProxy       bool
	proxyProvider    *proxyProvider
	proxyUseDuration time.Duration
}

type ClientAuth struct {
	UserID string
	Auth   *Auth
}

type tokenExpiration struct {
	timer  *time.Timer
	cancel chan (struct{})
}

// NewClientManager creates a new ClientMan which manages clients configured with the given client config.
func NewClientManager(config *ClientConfig) (cm *ClientManager) {
	if err := raven.SetDSN(config.SentryDSN); err != nil {
		logrus.WithError(err).Error("Could not set up sentry DSN")
	}

	cm = &ClientManager{
		config:       config,
		roundTripper: http.DefaultTransport,

		clients:       make(map[string]*Client),
		clientsLocker: &sync.Mutex{},

		tokens:       make(map[string]string),
		tokensLocker: &sync.Mutex{},

		expirations:       make(map[string]*tokenExpiration),
		expirationsLocker: &sync.Mutex{},

		host:       RootURL,
		scheme:     RootScheme,
		hostLocker: &sync.Mutex{},

		bridgeAuths: make(chan ClientAuth),
		clientAuths: make(chan ClientAuth),

		proxyProvider:    newProxyProvider(dohProviders, proxyQuery),
		proxyUseDuration: defaultProxyUseDuration,
	}

	go cm.forwardClientAuths()

	return
}

// SetRoundTripper sets the roundtripper used by clients created by this client manager.
func (cm *ClientManager) SetRoundTripper(rt http.RoundTripper) {
	cm.roundTripper = rt
}

// GetRoundTripper sets the roundtripper used by clients created by this client manager.
func (cm *ClientManager) GetRoundTripper() (rt http.RoundTripper) {
	return cm.roundTripper
}

// GetClient returns a client for the given userID.
// If the client does not exist already, it is created.
func (cm *ClientManager) GetClient(userID string) *Client {
	if client, ok := cm.clients[userID]; ok {
		return client
	}

	cm.clients[userID] = newClient(cm, userID)

	return cm.clients[userID]
}

// GetAnonymousClient returns an anonymous client. It replaces any anonymous client that was already created.
func (cm *ClientManager) GetAnonymousClient() *Client {
	if client, ok := cm.clients[""]; ok {
		client.Logout()
	}

	cm.clients[""] = newClient(cm, "")

	return cm.clients[""]
}

// LogoutClient logs out the client with the given userID and ensures its sensitive data is successfully cleared.
func (cm *ClientManager) LogoutClient(userID string) {
	client, ok := cm.clients[userID]

	if !ok {
		return
	}

	delete(cm.clients, userID)

	go func() {
		if err := client.logout(); err != nil {
			// TODO: Try again! This should loop until it succeeds (might fail the first time due to internet).
		}
		client.clearSensitiveData()
		cm.clearToken(userID)
	}()

	return
}

// GetHost returns the host to make requests to.
// It does not include the protocol i.e. no "https://" (use GetScheme for that).
func (cm *ClientManager) GetHost() string {
	cm.hostLocker.Lock()
	defer cm.hostLocker.Unlock()

	return cm.host
}

// GetScheme returns the scheme with which to make requests to the host.
func (cm *ClientManager) GetScheme() string {
	cm.hostLocker.Lock()
	defer cm.hostLocker.Unlock()

	return cm.scheme
}

// GetRootURL returns the full root URL (scheme+host).
func (cm *ClientManager) GetRootURL() string {
	cm.hostLocker.Lock()
	defer cm.hostLocker.Unlock()

	return fmt.Sprintf("%v://%v", cm.scheme, cm.host)
}

// IsProxyAllowed returns whether the user has allowed us to switch to a proxy if need be.
func (cm *ClientManager) IsProxyAllowed() bool {
	cm.hostLocker.Lock()
	defer cm.hostLocker.Unlock()

	return cm.allowProxy
}

// AllowProxy allows the client manager to switch clients over to a proxy if need be.
func (cm *ClientManager) AllowProxy() {
	cm.hostLocker.Lock()
	defer cm.hostLocker.Unlock()

	cm.allowProxy = true
}

// DisallowProxy prevents the client manager from switching clients over to a proxy if need be.
func (cm *ClientManager) DisallowProxy() {
	cm.hostLocker.Lock()
	defer cm.hostLocker.Unlock()

	cm.allowProxy = false
	cm.host = RootURL
}

// IsProxyEnabled returns whether we are currently proxying requests.
func (cm *ClientManager) IsProxyEnabled() bool {
	cm.hostLocker.Lock()
	defer cm.hostLocker.Unlock()

	return cm.host != RootURL
}

// SwitchToProxy returns a usable proxy server.
// TODO: Perhaps the name could be better -- we aren't only switching to a proxy but also to the standard API.
func (cm *ClientManager) SwitchToProxy() (proxy string, err error) {
	cm.hostLocker.Lock()
	defer cm.hostLocker.Unlock()

	logrus.Info("Attempting to switch to a proxy")

	if proxy, err = cm.proxyProvider.findProxy(); err != nil {
		err = errors.Wrap(err, "failed to find a usable proxy")
		return
	}

	logrus.WithField("proxy", proxy).Info("Switching to a proxy")

	// If the host is currently the RootURL, it's the first time we are enabling a proxy.
	// This means we want to disable it again in 24 hours.
	if cm.host == RootURL {
		go func() {
			<-time.After(cm.proxyUseDuration)
			cm.host = RootURL
		}()
	}

	cm.host = proxy

	return
}

// GetConfig returns the config used to configure clients.
func (cm *ClientManager) GetConfig() *ClientConfig {
	return cm.config
}

// GetToken returns the token for the given userID.
func (cm *ClientManager) GetToken(userID string) string {
	cm.tokensLocker.Lock()
	defer cm.tokensLocker.Unlock()

	return cm.tokens[userID]
}

// GetBridgeAuthChannel returns a channel on which client auths can be received.
func (cm *ClientManager) GetBridgeAuthChannel() chan ClientAuth {
	return cm.bridgeAuths
}

// getClientAuthChannel returns a channel on which clients should send auths.
func (cm *ClientManager) getClientAuthChannel() chan ClientAuth {
	return cm.clientAuths
}

// forwardClientAuths handles all incoming auths from clients before forwarding them on the bridge auth channel.
func (cm *ClientManager) forwardClientAuths() {
	for auth := range cm.clientAuths {
		logrus.Debug("ClientManager received auth from client")
		cm.handleClientAuth(auth)
		logrus.Debug("ClientManager is forwarding auth to bridge")
		cm.bridgeAuths <- auth
	}
}

// setToken sets the token for the given userID with the given expiration time.
func (cm *ClientManager) setToken(userID, token string, expiration time.Duration) {
	// We don't want to set tokens of anonymous clients.
	if userID == "" {
		return
	}

	cm.tokensLocker.Lock()
	defer cm.tokensLocker.Unlock()

	logrus.WithField("userID", userID).Info("Updating token")

	cm.tokens[userID] = token

	cm.setTokenExpiration(userID, expiration)
}

// setTokenExpiration will ensure the token is refreshed if it expires.
// If the token already has an expiration time set, it is replaced.
func (cm *ClientManager) setTokenExpiration(userID string, expiration time.Duration) {
	cm.expirationsLocker.Lock()
	defer cm.expirationsLocker.Unlock()

	if exp, ok := cm.expirations[userID]; ok {
		exp.timer.Stop()
		close(exp.cancel)
	}

	cm.expirations[userID] = &tokenExpiration{
		timer:  time.NewTimer(expiration),
		cancel: make(chan struct{}),
	}

	go cm.watchTokenExpiration(userID)
}

func (cm *ClientManager) clearToken(userID string) {
	cm.tokensLocker.Lock()
	defer cm.tokensLocker.Unlock()

	logrus.WithField("userID", userID).Info("Clearing token")

	delete(cm.tokens, userID)
}

// handleClientAuth updates or clears client authorisation based on auths received.
func (cm *ClientManager) handleClientAuth(ca ClientAuth) {
	// If we aren't managing this client, there's nothing to do.
	if _, ok := cm.clients[ca.UserID]; !ok {
		logrus.WithField("userID", ca.UserID).Info("Handling auth for unmanaged client")
		return
	}

	// If the auth is nil, we should clear the token.
	// TODO: Maybe we should trigger a client logout here? Then we don't have to remember to log it out ourself.
	if ca.Auth == nil {
		cm.clearToken(ca.UserID)
		return
	}

	cm.setToken(ca.UserID, ca.Auth.GenToken(), time.Duration(ca.Auth.ExpiresIn)*time.Second)
}

func (cm *ClientManager) watchTokenExpiration(userID string) {
	expiration := cm.expirations[userID]

	select {
	case <-expiration.timer.C:
		logrus.WithField("userID", userID).Info("Auth token expired! Refreshing")
		cm.clients[userID].AuthRefresh(cm.tokens[userID])

	case <-expiration.cancel:
		logrus.WithField("userID", userID).Info("Auth was refreshed before it expired")
	}
}