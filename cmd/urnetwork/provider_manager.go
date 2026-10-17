package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"deskaone-sdk/urnetwork/connect"
	"deskaone-sdk/urnetwork/protocol"
	"github.com/docopt/docopt-go"
)

type providerIdentity struct {
	ClientID string
	InstanceID string
}

type providerIdentityStore struct {
	sync.Mutex
	identities map[string]providerIdentity
	path string
}

func openProviderIdentityStore() (*providerIdentityStore, error) {
	p := filepath.Join(".urnetwork", "provider_manager.json")
	s := &providerIdentityStore{path: p, identities: map[string]providerIdentity{}}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.identities); err != nil {
		return nil, fmt.Errorf("invalid provider identity state: %w", err)
	}
	if s.identities == nil {
		s.identities = map[string]providerIdentity{}
	}
	return s, nil
}

func (s *providerIdentityStore) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(s.identities, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, body, 0600)
}

func (s *providerIdentityStore) identity(index int) (providerIdentity, error) {
	s.Lock()
	defer s.Unlock()
	key := strconv.Itoa(index)
	current := s.identities[key]
	if current.InstanceID != "" {
		if _, err := connect.ParseId(current.InstanceID); err != nil {
			return providerIdentity{}, err
		}
		return current, nil
	}
	current.InstanceID = connect.NewId().String()
	s.identities[key] = current
	return current, s.saveLocked()
}

func (s *providerIdentityStore) setClient(index int, prior providerIdentity, clientID connect.Id) error {
	s.Lock()
	defer s.Unlock()
	key := strconv.Itoa(index)
	current := s.identities[key]
	if current.InstanceID != prior.InstanceID {
		return errors.New("instance identity changed during authentication")
	}
	if current.ClientID != "" && current.ClientID != clientID.String() {
		return errors.New("server returned a different saved client ID")
	}
	current.ClientID = clientID.String()
	s.identities[key] = current
	return s.saveLocked()
}

type providerSession struct {
	cancel context.CancelFunc
	client *connect.Client
	transport *connect.PlatformTransport
	nat *connect.LocalUserNat
	provider *connect.RemoteUserNatProvider
}

func (s *providerSession) Close() {
	s.transport.Close()
	s.provider.Close()
	s.nat.Close()
	s.client.Close()
	s.cancel()
}

func authManagedClient(ctx context.Context, strategy *connect.ClientStrategy, apiURL, networkJWT string, saved providerIdentity) (string, connect.Id, error) {
	authCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	api := connect.NewBringYourApi(authCtx, strategy, apiURL)
	defer api.Close()
	api.SetByJwt(networkJWT)
	request := &connect.AuthNetworkClientArgs{
		Description: fmt.Sprintf("provider %s %s", runtime.GOOS, RequireVersion()),
		DeviceSpec: "",
	}
	if saved.ClientID != "" {
		id, err := connect.ParseId(saved.ClientID)
		if err != nil {
			return "", connect.Id{}, err
		}
		request.ClientId = &id
	}
	response, err := api.AuthNetworkClientSync(request)
	if err != nil {
		return "", connect.Id{}, err
	}
	if response == nil {
		return "", connect.Id{}, errors.New("empty auth-client response")
	}
	if response.Error != nil {
		return "", connect.Id{}, errors.New(response.Error.Message)
	}
	if response.ByClientJwt == "" {
		return "", connect.Id{}, errors.New("auth-client omitted by_client_jwt")
	}
	claims, err := connect.ParseByJwtUnverified(response.ByClientJwt)
	if err != nil {
		return "", connect.Id{}, err
	}
	if claims.ClientId == (connect.Id{}) {
		return "", connect.Id{}, errors.New("client JWT missing client_id")
	}
	return response.ByClientJwt, claims.ClientId, nil
}

func startManagedProvider(ctx context.Context, index int, saved providerIdentity, store *providerIdentityStore, selected *connect.ProxySettings, apiURL, connectURL, networkJWT string) (_ *providerSession, err error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	defer func() {
		if err != nil {
			cancel()
		}
	}()
	strategyConfig := connect.DefaultClientStrategySettings()
	strategyConfig.ProxySettings = selected
	if selected != nil {
		strategyConfig.EnableResilient = false
		strategyConfig.ExpandExtenderProfileCount = 0
		strategyConfig.MaxExtenderCount = 0
		strategyConfig.ExtenderHostnames = nil
	}
	strategy := connect.NewClientStrategy(sessionCtx, strategyConfig)
	clientJWT, clientID, err := authManagedClient(sessionCtx, strategy, apiURL, networkJWT, saved)
	if err != nil {
		return nil, err
	}
	if err := store.setClient(index, saved, clientID); err != nil {
		return nil, err
	}
	instanceID, err := connect.ParseId(saved.InstanceID)
	if err != nil {
		return nil, err
	}
	oob := connect.NewApiOutOfBandControl(sessionCtx, strategy, clientJWT, apiURL)
	client := connect.NewClient(sessionCtx, clientID, oob, connect.DefaultClientSettings())
	natConfig := connect.DefaultLocalUserNatSettings()
	natConfig.TcpBufferSettings.ConnectSettings = strategyConfig.ConnectSettings
	natConfig.UdpBufferSettings.ConnectSettings = strategyConfig.ConnectSettings
	nat := connect.NewLocalUserNat(sessionCtx, clientID.String(), natConfig)
	provider := connect.NewRemoteUserNatProvider(client, nat, connect.DefaultRemoteUserNatProviderSettings())
	client.ContractManager().SetProvideModes(map[protocol.ProvideMode]bool{
		protocol.ProvideMode_Public: true,
		protocol.ProvideMode_Network: true,
	})
	auth := &connect.ClientAuth{ByJwt: clientJWT, InstanceId: instanceID, AppVersion: RequireVersion()}
	var transport *connect.PlatformTransport
	if selected == nil {
		transport = connect.NewPlatformTransportWithDefaults(sessionCtx, strategy, client.RouteManager(), connectURL, auth)
	} else {
		transport = connect.NewPlatformTransportWithTargetMode(
			sessionCtx, strategy, client.RouteManager(), connectURL, auth,
			connect.TransportModeH1, connect.DefaultPlatformTransportSettings(),
		)
	}
	return &providerSession{cancel: cancel, client: client, nat: nat, provider: provider, transport: transport}, nil
}

func provideManaged(opts docopt.Opts, cfg managedProviderConfig) {
	apiURL, err := opts.String("--api_url")
	if err != nil || apiURL == "" {
		apiURL = DefaultApiUrl
	}
	connectURL, err := opts.String("--connect_url")
	if err != nil || connectURL == "" {
		connectURL = DefaultConnectUrl
	}

	if human, err := opts.String("--max-memory"); err == nil && human != "" {
		mem, err := connect.ParseByteCount(human)
		if err != nil {
			fmt.Printf("[provider] invalid --max-memory: %v\n", err)
			return
		}
		if 0 < mem {
			debug.SetMemoryLimit(int64(mem))
		}
	}
	networkBytes, err := os.ReadFile(filepath.Join(".urnetwork", "jwt"))
	if err != nil {
		fmt.Printf("[provider] missing network JWT: run go run ./cmd/urnetwork auth first (%v)\n", err)
		return
	}
	networkJWT := strings.TrimSpace(string(networkBytes))
	claims, err := connect.ParseByJwtUnverified(networkJWT)
	if err != nil || claims.ClientId != (connect.Id{}) {
		fmt.Println("[provider] .urnetwork/jwt must contain a network JWT (not a client JWT)")
		return
	}
	store, err := openProviderIdentityStore()
	if err != nil {
		fmt.Printf("[provider] identity store: %v\n", err)
		return
	}
	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(event.Ctx())
	defer cancel()
	loadCtx, cancelLoad := context.WithTimeout(ctx, 90*time.Second)
	proxies, err := managedLoadSources(loadCtx, cfg)
	cancelLoad()
	if err != nil {
		fmt.Printf("[provider] proxy sources: %v\n", err)
		return
	}
	fmt.Printf("[provider] MAX_CONNECTION=%d CONCURRENCY=%d PROXIES=%d\n",
		cfg.maxConnection, cfg.concurrency, len(proxies))
	if len(proxies) == 0 {
		fmt.Println("[provider] using direct mode")
	}
	sem := make(chan struct{}, cfg.concurrency)
	var workers sync.WaitGroup
	for i := 0; i < cfg.maxConnection; i++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			identity, err := store.identity(index)
			if err != nil {
				fmt.Printf("[provider %d] identity error: %v\n", index+1, err)
				return
			}
			attempt := 0
			for ctx.Err() == nil {
				var selected *connect.ProxySettings
				if len(proxies) > 0 {
					selected = proxies[(index+attempt)%len(proxies)]
				}
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				session, err := startManagedProvider(ctx, index, identity, store, selected, apiURL, connectURL, networkJWT)
				<-sem // limits simultaneous authentication and provider setup
				if err == nil {
					route := "direct"
					if selected != nil {
						route = string(selected.Protocol) + "://" + selected.Address
					}
					fmt.Printf("[provider %d] authenticated client=%s instance=%s route=%s\n",
						index+1, session.client.ClientId(), identity.InstanceID, route)
					ticker := time.NewTicker(10*time.Second)
					wasConnected := false
					lastConnectedAt := time.Now()
					lastStatusLog := time.Now()
					retryWithNext := false
					for !retryWithNext {
						connected := session.transport.IsConnected()
						if connected != wasConnected {
							wasConnected = connected
							fmt.Printf("[provider %d] connected=%t\n", index+1, connected)
						}
						if connected {
							lastConnectedAt = time.Now()
						}
						select {
						case <-ctx.Done():
							ticker.Stop()
							session.Close()
							return
						case <-ticker.C:
							if !connected && time.Since(lastStatusLog) >= 30*time.Second {
								fmt.Printf("[provider %d] waiting for platform connection (check proxy availability)\n", index+1)
								lastStatusLog = time.Now()
							}
							if !connected && len(proxies) > 1 && time.Since(lastConnectedAt) >= 90*time.Second {
								fmt.Printf("[provider %d] disconnected for 90s; trying next proxy\n", index+1)
								retryWithNext = true
							}
						}
					}
					ticker.Stop()
					session.Close()
					attempt++
					continue
				}
				if ctx.Err() != nil {
					return
				}
				fmt.Printf("[provider %d] init error: %v (retry in 30s)\n", index+1, err)
				attempt++
				select {
				case <-ctx.Done():
					return
				case <-time.After(30*time.Second):
				}
			}
		}(i)
	}
	if port, err := opts.Int("--port"); err == nil && port > 0 {
		server := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: &Status{}}
		go func() {
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Printf("[provider] status server: %v\n", err)
				cancel()
			}
		}()
		defer server.Shutdown(context.Background())
	}
	workers.Wait()
}
