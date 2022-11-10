package virtualfilter

import (
	"sync"
	"time"

	"github.com/Conflux-Chain/confura/node"
	"github.com/Conflux-Chain/confura/util"
	rpcutil "github.com/Conflux-Chain/confura/util/rpc"
	"github.com/openweb3/go-rpc-provider"
	"github.com/openweb3/web3go/types"
	web3Types "github.com/openweb3/web3go/types"
	"github.com/pkg/errors"
)

var (
	ethEmptyLogs = []types.Log{}
	nilRpcId     = rpc.ID("0x0")
)

// FilterApi offers support to proxy through full nodes to create and manage filters.
type FilterApi struct {
	sys       *FilterSystem
	filtersMu sync.Mutex
	filters   map[rpc.ID]*Filter
	clients   util.ConcurrentMap
}

// NewFilterApi returns a new FilterApi instance.
func NewFilterApi(system *FilterSystem, ttl time.Duration) *FilterApi {
	api := &FilterApi{
		sys:     system,
		filters: make(map[rpc.ID]*Filter),
	}

	go api.timeoutLoop(ttl)

	return api
}

// timeoutLoop runs at the interval set by 'timeout' and deletes filters
// that have not been recently used. It is started when the API is created.
func (api *FilterApi) timeoutLoop(timeout time.Duration) {
	ticker := time.NewTicker(timeout / 2)
	defer ticker.Stop()

	for range ticker.C {
		expired := api.expireFilters(timeout)

		// also uninstall delegate log filters
		for id, f := range expired {
			if f.typ == FilterTypeLog {
				api.sys.delegateUninstallFilter(id)
			}
		}
	}
}

// NewBlockFilter creates a proxy block filter from full node with specified node URL
func (api *FilterApi) NewBlockFilter(nodeUrl string) (rpc.ID, error) {
	client, err := api.loadOrGetFnClient(nodeUrl)
	if err != nil {
		return nilRpcId, err
	}

	// create a proxy block filter to the allocated full node
	fid, err := client.Filter.NewBlockFilter()
	if err != nil {
		return nilRpcId, err
	}

	pfid := rpc.NewID()
	api.addFilter(pfid, newFilter(FilterTypeBlock, &fnDelegateInfo{
		fid:      *fid,
		nodeName: rpcutil.Url2NodeName(nodeUrl),
	}))

	return pfid, nil
}

// NewPendingTransactionFilter creates a proxy pending txn filter from full node with specified node URL
func (api *FilterApi) NewPendingTransactionFilter(nodeUrl string) (rpc.ID, error) {
	client, err := api.loadOrGetFnClient(nodeUrl)
	if err != nil {
		return nilRpcId, err
	}

	// create a proxy pending txn filter to the allocated full node
	fid, err := client.Filter.NewPendingTransactionFilter()
	if err != nil {
		return nilRpcId, err
	}

	pfid := rpc.NewID()
	api.addFilter(pfid, newFilter(FilterTypePendingTxn, &fnDelegateInfo{
		fid:      *fid,
		nodeName: rpcutil.Url2NodeName(nodeUrl),
	}))

	return pfid, nil
}

// NewFilter creates a proxy log filter from full node with specified node URL and filter query condition
func (api *FilterApi) NewFilter(nodeUrl string, crit *web3Types.FilterQuery) (rpc.ID, error) {
	w3c, err := api.loadOrGetFnClient(nodeUrl)
	if err != nil {
		return nilRpcId, err
	}

	// create a delegate log filter to the allocated full node
	pfid, err := api.sys.delegateNewFilter(w3c)
	if err != nil {
		return nilRpcId, err
	}

	api.addFilter(pfid, newFilter(FilterTypePendingTxn, &fnDelegateInfo{
		nodeName: rpcutil.Url2NodeName(nodeUrl),
	}, crit))

	return pfid, errors.New("not supported yet")
}

// UninstallFilter removes the proxy filter with the given filter id.
func (api *FilterApi) UninstallFilter(nodeUrl string, id rpc.ID) (bool, error) {
	f, found := api.delFilter(id)
	if !found {
		return false, nil
	}

	if f.typ == FilterTypeLog {
		return api.sys.delegateUninstallFilter(id)
	}

	if !f.IsDelegateFullNode(nodeUrl) { // not the old delegate full node?
		// return directly not even bother to delete the deprecated filter
		return true, nil
	}

	client, err := api.loadOrGetFnClient(nodeUrl)
	if err != nil {
		return false, err
	}

	return client.Filter.UninstallFilter(id)
}

// GetFilterLogs returns the logs for the proxy filter with the given id.
func (api *FilterApi) GetFilterLogs(nodeUrl string, id rpc.ID) ([]types.Log, error) {
	f, found := api.getFilter(id)

	if !found || f.typ != FilterTypeLog { // not found or wrong filter type?
		return nil, errFilterNotFound
	}

	if !f.IsDelegateFullNode(nodeUrl) { // not the old delegate full node?
		// remove the deprecated filter for data consistency
		api.delFilter(id)
		// uninstall the delegate log filter
		api.sys.delegateUninstallFilter(id)

		return nil, errFilterNotFound
	}

	w3c, err := api.loadOrGetFnClient(nodeUrl)
	if err != nil {
		return nil, err
	}

	return api.sys.GetLogs(w3c, f.crit)
}

// GetFilterChanges returns the data for the proxy filter with the given id since
// last time it was called. This can be used for polling.
func (api *FilterApi) GetFilterChanges(nodeUrl string, id rpc.ID) (interface{}, error) {
	f, found := api.getFilter(id)
	if !found {
		return nil, errFilterNotFound
	}

	if !f.IsDelegateFullNode(nodeUrl) { // not the old delegate full node?
		// remove the deprecated filter for data consistency
		api.delFilter(id)

		// uninstall the delegate log filter
		if f.typ == FilterTypeLog {
			return api.sys.delegateUninstallFilter(id)
		}

		return nil, errFilterNotFound
	}

	// refresh filter last polling time
	api.refreshFilterPollingTime(id)

	w3c, err := api.loadOrGetFnClient(nodeUrl)
	if err != nil {
		return nil, err
	}

	if f.typ == FilterTypeLog {
		return api.sys.GetFilterLogs(w3c, f.crit)
	}

	result, err := w3c.Filter.GetFilterChanges(f.del.fid)
	if IsFilterNotFoundError(err) {
		// lazily remove deprecated delegate filter
		api.delFilter(id)
	}

	return result, err
}

func (api *FilterApi) loadOrGetFnClient(nodeUrl string) (*node.Web3goClient, error) {
	nodeName := rpcutil.Url2NodeName(nodeUrl)
	client, _, err := api.clients.LoadOrStoreFnErr(nodeName, func(interface{}) (interface{}, error) {
		client, err := rpcutil.NewEthClient(nodeUrl, rpcutil.WithClientHookMetrics(true))
		if err != nil {
			return nil, err
		}

		return &node.Web3goClient{Client: client, URL: nodeUrl}, nil
	})

	if err != nil {
		return nil, err
	}

	return client.(*node.Web3goClient), nil
}

// proxy filter management

func (api *FilterApi) getFilter(id rpc.ID) (*Filter, bool) {
	api.filtersMu.Lock()
	defer api.filtersMu.Unlock()

	f, ok := api.filters[id]
	return f, ok
}

func (api *FilterApi) addFilter(id rpc.ID, filter *Filter) {
	api.filtersMu.Lock()
	defer api.filtersMu.Unlock()

	api.filters[id] = filter
}

func (api *FilterApi) delFilter(id rpc.ID) (*Filter, bool) {
	api.filtersMu.Lock()
	defer api.filtersMu.Unlock()

	f, found := api.filters[id]
	if found {
		delete(api.filters, id)
	}

	return f, found
}

func (api *FilterApi) refreshFilterPollingTime(id rpc.ID) {
	api.filtersMu.Lock()
	defer api.filtersMu.Unlock()

	if f, found := api.filters[id]; found {
		f.lastPollingTime = time.Now().Unix()
	}
}

func (api *FilterApi) expireFilters(ttl time.Duration) map[rpc.ID]*Filter {
	api.filtersMu.Lock()
	defer api.filtersMu.Unlock()

	efilters := make(map[rpc.ID]*Filter)

	for id, f := range api.filters {
		elapsed := time.Duration(time.Now().Unix()-f.lastPollingTime) * time.Second
		if elapsed >= ttl { // expired
			efilters[id] = f
			delete(api.filters, id)
		}
	}

	return efilters
}
