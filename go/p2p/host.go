package p2p

import (
	"fmt"
	"net"
	"time"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/net/conngater"
	"github.com/libp2p/go-libp2p/p2p/net/connmgr"
	"github.com/multiformats/go-multiaddr"
	"github.com/spf13/viper"

	"github.com/oasisprotocol/oasis-core/go/common/crypto/signature"
	"github.com/oasisprotocol/oasis-core/go/common/node"
	"github.com/oasisprotocol/oasis-core/go/common/version"
	"github.com/oasisprotocol/oasis-core/go/p2p/api"
)

// HostConfig describes a set of settings for a host.
type HostConfig struct {
	Signer signature.Signer

	UserAgent  string
	ListenAddr multiaddr.Multiaddr
	Port       uint16

	ConnManagerConfig
	ConnGaterConfig
}

// NewHost constructs a new libp2p host.
func NewHost(cfg *HostConfig) (host.Host, *conngater.BasicConnectionGater, error) {
	id := api.SignerToPrivKey(cfg.Signer)

	// Set up a connection manager so we can limit the number of connections.
	cm, err := NewConnManager(&cfg.ConnManagerConfig)
	if err != nil {
		return nil, nil, err
	}

	// Set up a connection gater so we can block peers.
	cg, err := NewConnGater(&cfg.ConnGaterConfig)
	if err != nil {
		return nil, nil, err
	}

	host, err := libp2p.New(
		libp2p.UserAgent(cfg.UserAgent),
		libp2p.ListenAddrs(cfg.ListenAddr),
		libp2p.Identity(id),
		libp2p.ConnectionManager(cm),
		libp2p.ConnectionGater(cg),
	)
	if err != nil {
		return nil, nil, err
	}

	// We need to return the gater as it is not accessible via the host.
	return host, cg, nil
}

// NewHost constructs a new libp2p host.
func (cfg *HostConfig) NewHost() (host.Host, *conngater.BasicConnectionGater, error) {
	return NewHost(cfg)
}

// Load loads host configuration.
func (cfg *HostConfig) Load() error {
	userAgent := fmt.Sprintf("oasis-core/%s", version.SoftwareVersion)
	port := viper.GetUint16(CfgHostPort)

	// Listen for connections on all interfaces.
	listenAddr, err := multiaddr.NewMultiaddr(
		fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", port),
	)
	if err != nil {
		return fmt.Errorf("failed to create multiaddress: %w", err)
	}

	var cmCfg ConnManagerConfig
	if err = cmCfg.Load(); err != nil {
		return fmt.Errorf("failed to load connection manager config: %w", err)
	}

	var cgCfg ConnGaterConfig
	if err = cgCfg.Load(); err != nil {
		return fmt.Errorf("failed to load connection gater config: %w", err)
	}

	cfg.UserAgent = userAgent
	cfg.Port = port
	cfg.ListenAddr = listenAddr
	cfg.ConnManagerConfig = cmCfg
	cfg.ConnGaterConfig = cgCfg

	return nil
}

// ConnManagerConfig describes a set of settings for a connection manager.
type ConnManagerConfig struct {
	MinPeers        int
	MaxPeers        int
	GracePeriod     time.Duration
	PersistentPeers []peer.ID
}

// NewConnManager constructs a new connection manager.
func NewConnManager(cfg *ConnManagerConfig) (*connmgr.BasicConnMgr, error) {
	gracePeriod := connmgr.WithGracePeriod(cfg.GracePeriod)
	cm, err := connmgr.NewConnManager(cfg.MinPeers, cfg.MaxPeers, gracePeriod)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection manager: %w", err)
	}
	for _, pid := range cfg.PersistentPeers {
		cm.Protect(pid, "")
	}
	return cm, nil
}

// NewConnManager constructs a new connection manager.
func (cfg *ConnManagerConfig) NewConnManager() (*connmgr.BasicConnMgr, error) {
	return NewConnManager(cfg)
}

// Load loads connection manager configuration.
func (cfg *ConnManagerConfig) Load() error {
	persistentPeersMap := make(map[core.PeerID]struct{})
	for _, pp := range viper.GetStringSlice(CfgConnMgrPersistentPeers) {
		var addr node.ConsensusAddress
		if err := addr.UnmarshalText([]byte(pp)); err != nil {
			return fmt.Errorf("malformed address (expected pubkey@IP:port): %w", err)
		}

		pid, err := api.PublicKeyToPeerID(addr.ID)
		if err != nil {
			return fmt.Errorf("invalid public key (%s): %w", addr.ID, err)
		}

		persistentPeersMap[pid] = struct{}{}
	}

	persistentPeers := make([]peer.ID, 0)
	for pid := range persistentPeersMap {
		persistentPeers = append(persistentPeers, pid)
	}

	cfg.MinPeers = viper.GetInt(CfgConnMgrMaxNumPeers)
	cfg.MaxPeers = cfg.MinPeers + peersHighWatermarkDelta
	cfg.GracePeriod = viper.GetDuration(CfgConnMgrPeerGracePeriod)
	cfg.PersistentPeers = persistentPeers

	return nil
}

// ConnGaterConfig describes a set of settings for a connection gater.
type ConnGaterConfig struct {
	BlockedPeers []net.IP
}

// NewConnGater constructs a new connection gater.
func NewConnGater(cfg *ConnGaterConfig) (*conngater.BasicConnectionGater, error) {
	// Set up a connection gater and block blacklisted peers.
	cg, err := conngater.NewBasicConnectionGater(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create connection gater: %w", err)
	}

	for _, ip := range cfg.BlockedPeers {
		if err = cg.BlockAddr(ip); err != nil {
			return nil, fmt.Errorf("connection gater failed to block IP (%s): %w", ip, err)
		}
	}
	return cg, nil
}

// NewConnGater constructs a new connection gater.
func (cfg *ConnGaterConfig) NewConnGater() (*conngater.BasicConnectionGater, error) {
	return NewConnGater(cfg)
}

// Load loads connection gater configuration.
func (cfg *ConnGaterConfig) Load() error {
	blockedPeers := make([]net.IP, 0)
	for _, blockedIP := range viper.GetStringSlice(CfgConnGaterBlockedPeerIPs) {
		parsedIP := net.ParseIP(blockedIP)
		if parsedIP == nil {
			return fmt.Errorf("malformed blocked IP: %s", blockedIP)
		}
		blockedPeers = append(blockedPeers, parsedIP)
	}

	cfg.BlockedPeers = blockedPeers

	return nil
}
