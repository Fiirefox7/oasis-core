package oasis

import (
	"fmt"

	fileSigner "github.com/oasisprotocol/oasis-core/go/common/crypto/signature/signers/file"
	"github.com/oasisprotocol/oasis-core/go/common/identity"
)

// SeedCfg is the Oasis seed node configuration.
type SeedCfg struct {
	Name string

	DisableAddrBookFromGenesis bool
}

// Seed is an Oasis seed node.
type Seed struct { // nolint: maligned
	*Node

	disableAddrBookFromGenesis bool

	consensusPort  uint16
	libp2pSeedPort uint16
}

func (seed *Seed) AddArgs(args *argBuilder) error {
	otherSeeds := []*Seed{}
	for _, s := range seed.net.seeds {
		if s.Name == seed.Name {
			continue
		}
		otherSeeds = append(otherSeeds, s)
	}

	args.debugDontBlameOasis().
		debugAllowRoot().
		debugAllowTestKeys().
		debugSetRlimit().
		workerCertificateRotation(true).
		tendermintCoreAddress(seed.consensusPort).
		workerP2pPort(seed.libp2pSeedPort).
		appendSeedNodes(otherSeeds).
		seedMode()

	if seed.disableAddrBookFromGenesis {
		args.tendermintSeedDisableAddrBookFromGenesis()
	}

	return nil
}

// NewSeed provisions a new seed node and adds it to the network.
func (net *Network) NewSeed(cfg *SeedCfg) (*Seed, error) {
	seedName := fmt.Sprintf("seed-%d", len(net.seeds))
	host, err := net.GetNamedNode(seedName, nil)
	if err != nil {
		return nil, err
	}

	// Pre-provision the node identity, so that we can figure out what
	// to pass all the actual nodes in advance, instead of having to
	// start the node and fork out to `oasis-node debug tendermint show-node-id`.
	signerFactory, err := fileSigner.NewFactory(host.dir.String(), identity.RequiredSignerRoles...)
	if err != nil {
		return nil, fmt.Errorf("oasis/seed: failed to create seed signer factory: %w", err)
	}
	seedIdentity, err := identity.LoadOrGenerate(host.dir.String(), signerFactory, false)
	if err != nil {
		return nil, fmt.Errorf("oasis/seed: failed to provision seed identity: %w", err)
	}
	host.p2pSigner = seedIdentity.P2PSigner.Public()

	seedNode := &Seed{
		Node:                       host,
		disableAddrBookFromGenesis: cfg.DisableAddrBookFromGenesis,
		consensusPort:              host.getProvisionedPort(nodePortConsensus),
		libp2pSeedPort:             host.getProvisionedPort(nodePortP2PSeed),
	}
	net.seeds = append(net.seeds, seedNode)
	host.features = append(host.features, seedNode)

	return seedNode, nil
}
