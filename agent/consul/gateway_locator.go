package consul

import (
	"errors"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/hashicorp/consul/agent/consul/state"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/consul/api"
	"github.com/hashicorp/consul/ipaddr"
	"github.com/hashicorp/consul/lib"
	"github.com/hashicorp/consul/logging"
	"github.com/hashicorp/go-hclog"
	memdb "github.com/hashicorp/go-memdb"
)

// GatewayLocator assists in selecting an appropriate mesh gateway when wan
// federation via mesh gateways is enabled.
//
// This is exclusively used by the consul server itself when it needs to tunnel
// RPC or gossip through a mesh gateway to reach its ultimate destination.
//
// During secondary datacenter bootstrapping there is a phase where it is
// impossible for mesh gateways in the secondary datacenter to register
// themselves into the catalog to be discovered by the servers, so the servers
// maintain references for the mesh gateways in the primary in addition to its
// own local mesh gateways.
//
// After initial datacenter federation the primary mesh gateways are only used
// in extreme fallback situations (basically re-bootstrapping).
//
// For all other operations a consul server will ALWAYS contact a local mesh
// gateway to ultimately forward the request through a remote mesh gateway to
// reach its destination.
type GatewayLocator struct {
	logger            hclog.Logger
	srv               serverDelegate
	datacenter        string // THIS dc
	primaryDatacenter string

	// these ONLY contain ones that have the wanfed:1 meta
	gatewaysLock    sync.Mutex
	primaryGateways []string // WAN addrs
	localGateways   []string // LAN addrs

	// primaryMeshGatewayDiscoveredAddresses is the current fallback addresses
	// for the mesh gateways in the primary datacenter.
	primaryMeshGatewayDiscoveredAddresses     []string
	primaryMeshGatewayDiscoveredAddressesLock sync.Mutex

	// This will be closed the FIRST time we get some gateways populated
	primaryGatewaysReadyCh   chan struct{}
	primaryGatewaysReadyOnce sync.Once
}

// PrimaryMeshGatewayAddressesReadyCh returns a channel that will be closed
// when federation state replication ships back at least one primary mesh
// gateway (not via fallback config).
func (g *GatewayLocator) PrimaryMeshGatewayAddressesReadyCh() <-chan struct{} {
	return g.primaryGatewaysReadyCh
}

// PickGateway returns the address for a gateway suitable for reaching the
// provided datacenter.
func (g *GatewayLocator) PickGateway(dc string) string {
	item := g.pickGateway(dc == g.primaryDatacenter)
	g.logger.Trace("picking gateway for transit", "gateway", item, "source_datacenter", g.datacenter, "dest_datacenter", dc)
	return item
}

func (g *GatewayLocator) pickGateway(primary bool) string {
	addrs := g.listGateways(primary)
	return getRandomItem(addrs)
}

func (g *GatewayLocator) listGateways(primary bool) []string {
	g.gatewaysLock.Lock()
	defer g.gatewaysLock.Unlock()

	var addrs []string
	if primary {
		addrs = g.primaryGateways
	} else {
		addrs = g.localGateways
	}

	if primary && len(addrs) == 0 {
		addrs = g.PrimaryGatewayFallbackAddresses()
	}

	return addrs
}

// RefreshPrimaryGatewayFallbackAddresses is used to update the list of current
// fallback addresses for locating mesh gateways in the primary datacenter.
func (g *GatewayLocator) RefreshPrimaryGatewayFallbackAddresses(addrs []string) {
	sort.Strings(addrs)

	g.primaryMeshGatewayDiscoveredAddressesLock.Lock()
	defer g.primaryMeshGatewayDiscoveredAddressesLock.Unlock()

	if !lib.StringSliceEqual(addrs, g.primaryMeshGatewayDiscoveredAddresses) {
		g.primaryMeshGatewayDiscoveredAddresses = addrs
		g.logger.Info("updated fallback list of primary mesh gateways", "mesh_gateways", addrs)
	}
}

// PrimaryGatewayFallbackAddresses returns the current set of discovered
// fallback addresses for the mesh gateways in the primary datacenter.
func (g *GatewayLocator) PrimaryGatewayFallbackAddresses() []string {
	g.primaryMeshGatewayDiscoveredAddressesLock.Lock()
	defer g.primaryMeshGatewayDiscoveredAddressesLock.Unlock()

	out := make([]string, len(g.primaryMeshGatewayDiscoveredAddresses))
	copy(out, g.primaryMeshGatewayDiscoveredAddresses)
	return out
}

func getRandomItem(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	default:
		idx := int(rand.Int31n(int32(len(items))))
		return items[idx]
	}
}

type serverDelegate interface {
	blockingQuery(queryOpts structs.QueryOptionsCompat, queryMeta structs.QueryMetaCompat, fn queryFn) error
	PrimaryGatewayFallbackAddresses() []string
	IsLeader() bool
	LeaderLastContact() time.Time
}

func NewGatewayLocator(
	logger hclog.Logger,
	srv serverDelegate,
	datacenter string,
	primaryDatacenter string,
) *GatewayLocator {
	return &GatewayLocator{
		logger:                 logger.Named(logging.GatewayLocator),
		srv:                    srv,
		datacenter:             datacenter,
		primaryDatacenter:      primaryDatacenter,
		primaryGatewaysReadyCh: make(chan struct{}),
	}
}

var errGatewayLocalStateNotInitialized = errors.New("local state not initialized")

func (g *GatewayLocator) Run(stopCh <-chan struct{}) {
	var lastFetchIndex uint64
	retryLoopBackoff(stopCh, func() error {
		idx, err := g.runOnce(lastFetchIndex)
		if err != nil {
			return err
		}

		lastFetchIndex = idx

		return nil
	}, func(err error) {
		if !errors.Is(err, errGatewayLocalStateNotInitialized) {
			g.logger.Error("error tracking primary and local mesh gateways", "error", err)
		}
	})
}

func (g *GatewayLocator) runOnce(lastFetchIndex uint64) (uint64, error) {
	if err := g.checkLocalStateIsReady(); err != nil {
		return 0, err
	}

	// NOTE: we can't do RPC here because we won't have a token so we'll just
	// mostly assume that our FSM is caught up enough to answer locally.  If
	// this has drifted it's no different than a cache that drifts or an
	// inconsistent read.
	queryOpts := &structs.QueryOptions{
		MinQueryIndex:     lastFetchIndex,
		RequireConsistent: false,
	}

	var (
		results   []*structs.FederationState
		queryMeta structs.QueryMeta
	)
	err := g.srv.blockingQuery(
		queryOpts,
		&queryMeta,
		func(ws memdb.WatchSet, state *state.Store) error {
			// Get the existing stored version of this config that has replicated down.
			// We could phone home to get this but that would incur extra WAN traffic
			// when we already have enough information locally to figure it out
			// (assuming that our replicator is still functioning).
			idx, all, err := state.FederationStateList(ws)
			if err != nil {
				return err
			}

			queryMeta.Index = idx
			results = all

			return nil
		})
	if err != nil {
		return 0, err
	}

	g.updateFromState(results)

	return queryMeta.Index, nil
}

// checkLocalStateIsReady is inlined a bit from (*Server).forward(). We need to
// wait until our own state machine is safe to read from.
func (g *GatewayLocator) checkLocalStateIsReady() error {
	// Check if we can allow a stale read, ensure our local DB is initialized
	if !g.srv.LeaderLastContact().IsZero() {
		return nil // the raft leader talked to us
	}

	if g.srv.IsLeader() {
		return nil // we are the leader
	}

	return errGatewayLocalStateNotInitialized
}

func (g *GatewayLocator) updateFromState(results []*structs.FederationState) {
	var (
		local   structs.CheckServiceNodes
		primary structs.CheckServiceNodes
	)
	for _, config := range results {
		retained := retainGateways(config.MeshGateways)
		if config.Datacenter == g.datacenter {
			local = retained
		}
		// NOT else-if because conditionals are not mutually exclusive
		if config.Datacenter == g.primaryDatacenter {
			primary = retained
		}
	}

	primaryAddrs := renderGatewayAddrs(primary, true)
	localAddrs := renderGatewayAddrs(local, false)

	g.gatewaysLock.Lock()
	defer g.gatewaysLock.Unlock()

	changed := false
	primaryReady := false
	if !lib.StringSliceEqual(g.primaryGateways, primaryAddrs) {
		g.primaryGateways = primaryAddrs
		primaryReady = len(g.primaryGateways) > 0
		changed = true
	}
	if !lib.StringSliceEqual(g.localGateways, localAddrs) {
		g.localGateways = localAddrs
		changed = true
	}

	if changed {
		g.logger.Info(
			"new cached locations of mesh gateways",
			"primary", primaryAddrs,
			"local", localAddrs,
		)
	}

	if primaryReady {
		g.primaryGatewaysReadyOnce.Do(func() {
			close(g.primaryGatewaysReadyCh)
		})
	}
}

func retainGateways(full structs.CheckServiceNodes) structs.CheckServiceNodes {
	out := make([]structs.CheckServiceNode, 0, len(full))
	for _, csn := range full {
		if csn.Service.Meta[structs.MetaWANFederationKey] != "1" {
			continue
		}

		// only keep healthy ones
		ok := true
		for _, chk := range csn.Checks {
			if chk.Status == api.HealthCritical {
				ok = false
			}
		}

		if ok {
			out = append(out, csn)
		}
	}
	return out
}

func renderGatewayAddrs(gateways structs.CheckServiceNodes, wan bool) []string {
	out := make([]string, 0, len(gateways))
	for _, csn := range gateways {
		addr, port := csn.BestAddress(wan)
		completeAddr := ipaddr.FormatAddressPort(addr, port)
		out = append(out, completeAddr)
	}
	sort.Strings(out)
	return out
}
