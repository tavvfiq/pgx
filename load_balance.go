package pgx

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/rs/zerolog/log"

	"maps"
	"math"
	"math/big"
	"net"
	"regexp"
	"strings"
	"time"
)

const NO_SERVERS_MSG = "could not find a server to connect to"
const MAX_RETRIES = 20
const REFRESH_INTERVAL_SECONDS = 300
const DEFAULT_FAILED_HOST_RECONNECT_DELAY_SECS = 5
const MAX_FAILED_HOST_RECONNECT_DELAY_SECS = 60
const MAX_INTERVAL_SECONDS = 600
const MAX_PREFERENCE_VALUE = 10
const CONTROL_CONN_TIMEOUT = 15 * time.Second

var ErrFallbackToOriginalBehaviour = errors.New("no preferred server available, fallback-to-topology-keys-only is set to true")

// -- Values for ClusterLoadInfo.flags --
// Use private address (host) of tservers to create a connection
const USE_HOSTS byte = 0

// Use public address (public_ip) of tservers to create a connection
const USE_PUBLIC_IP byte = 1

// Try both the addresses (host, public_ip) of tservers to create a connection
const TRY_HOSTS_PUBLIC_IP byte = 2

// Both the addresses (host, public_ip) of tserver to be tried, but no success with private addresses to create a connection
const HOSTS_EXHAUSTED byte = 3

// Indicate to the Go routine processing the requestChan that it should return a least loaded tserver host, port
const GET_LB_CONN byte = 4

// Indicate to the Go routine processing the requestChan that it should decrease the connection count for the given host by one
const DECREMENT_COUNT byte = 5

type ClusterLoadInfo struct {
	clusterName string
	ctx         context.Context
	config      *ConnConfig
	controlConn *Conn
	ctrlCtx     context.Context
	lastRefresh time.Time
	// map of host in primary cluster -> connection count
	hostLoadPrimary map[string]int
	// map of host in read replica cluster -> connection count
	hostLoadRR map[string]int
	// map of host -> port
	hostPort map[string]uint16
	// map of "cloud.region.zone" -> slice of hostnames of primary cluster
	zoneListPrimary map[string][]string
	// map of "cloud.region.zone" -> slice of hostnames of read replica custer
	zoneListRR map[string][]string
	// map of host -> int
	unavailableHosts map[string]int64
	// map of (private -> public) address of a node.
	hostPairs map[string]string
	flags     byte
}

type lbHost struct {
	hostname string
	port     uint16
	err      error
}

var clustersLoadInfo map[string]*ClusterLoadInfo

const LB_QUERY = "SELECT host,port,num_connections,node_type,cloud,region,zone,public_ip FROM yb_servers()"

// Only the Go routine spawned in init() reads this channel. Based on the flag, it
// - returns the least loaded tserver's host/port (GET_LB_CONN)
// - decrements connection count by one for closed connection (DECREMENT_COUNT)
var requestChan chan *ClusterLoadInfo

// Only the Go routine spawned in init() writes to this channel.
// It returns the least loaded tserver's host/port if successful else err
var hostChan chan *lbHost

func NewClusterLoadInfo(ctx context.Context, config *ConnConfig) *ClusterLoadInfo {
	info := new(ClusterLoadInfo)
	info.clusterName = LookupIP(config.Host)
	info.ctx = ctx
	info.config = config
	info.flags = GET_LB_CONN
	return info
}

func LookupIP(host string) string {
	addrs, err := net.LookupHost(host)
	if err == nil {
		for _, addr := range addrs {
			if strings.Contains(addr, ".") {
				return addr
			}
		}
		if len(addrs) > 0 {
			return addrs[0]
		}
	}
	return host
}

func init() {
	clustersLoadInfo = make(map[string]*ClusterLoadInfo)
	requestChan = make(chan *ClusterLoadInfo)
	hostChan = make(chan *lbHost)
	go produceHostName(requestChan, hostChan)
}

func replaceHostString(connString string, newHost string, port uint16) string {
	newConnString := connString
	if strings.HasPrefix(connString, "postgres://") || strings.HasPrefix(connString, "postgresql://") {
		if strings.Contains(connString, "@") {
			pattern := regexp.MustCompile("@([^/]*)/")
			// todo IPv6 handling
			newConnString = pattern.ReplaceAllString(connString, fmt.Sprintf("@%s:%d/", newHost, port))
		} else {
			pattern := regexp.MustCompile("://([^/]*)/")
			newConnString = pattern.ReplaceAllString(connString, fmt.Sprintf("://%s:%d/", newHost, port))
		}
	} else { // key = value (DSN style)
		pattern := regexp.MustCompile("host=([^ ]*) ")
		newConnString = pattern.ReplaceAllString(connString, fmt.Sprintf("host=%s ", newHost))
		pattern = regexp.MustCompile("port=([^ ]*) ")
		newConnString = pattern.ReplaceAllString(newConnString, fmt.Sprintf("port=%d ", port))
	}
	return newConnString
}

func produceHostName(in chan *ClusterLoadInfo, out chan *lbHost) {

	for {
		new, present := <-in

		if !present {
			log.Warn().Msg("The requestChannel is closed, load_balance feature will not work")
			break
		}
		if new.flags == DECREMENT_COUNT {
			names := strings.Split(new.clusterName, ",")
			if len(names) != 2 {
				log.Warn().Msgf("cannot parse names to update connection count: %s", new.clusterName)
			} else {
				cli, ok := clustersLoadInfo[LookupIP(names[0])]
				if ok {
					cnt, found := cli.hostLoadPrimary[names[1]]
					if found {
						if cnt != 0 {
							cli.hostLoadPrimary[names[1]] = cnt - 1
						}
					} else if cnt, found = cli.hostLoadRR[names[1]]; found {
						if cnt != 0 {
							cli.hostLoadRR[names[1]] = cnt - 1
						}
					}
				}
			}
			continue
		}
		old, present := clustersLoadInfo[new.clusterName]
		if !present {
			// There is no loadInfo available for this config. Create one.
			err := refreshLoadInfo(new)
			if err != nil {
				lb := &lbHost{
					hostname: "",
					err:      err,
				}
				out <- lb
				continue
			}
			publicIpAvailable := false
			for k, v := range new.hostPairs {
				if v != "" {
					publicIpAvailable = true
				}
				if new.clusterName == k {
					new.flags = USE_HOSTS
					break
				} else if new.clusterName == v {
					new.flags = USE_PUBLIC_IP
					break
				} else {
					new.flags = TRY_HOSTS_PUBLIC_IP
				}
			}
			if !publicIpAvailable {
				new.flags = USE_HOSTS
			}

			clustersLoadInfo[new.clusterName] = new

			out <- getHostWithLeastConns(new)
			// continue
		} else {
			old.config.topologyKeys = new.config.topologyKeys // Use the provided topology-keys.
			old.config.fallbackToTopologyKeysOnly = new.config.fallbackToTopologyKeysOnly
			old.config.failedHostReconnectDelaySecs = new.config.failedHostReconnectDelaySecs
			old.config.loadBalance = new.config.loadBalance
			old.config.connString = new.config.connString
			out <- refreshAndGetLeastLoadedHost(old, new.unavailableHosts)
			// continue
		}
	}
}

func connectLoadBalanced(ctx context.Context, config *ConnConfig) (c *Conn, err error) {
	newLoadInfo := NewClusterLoadInfo(ctx, config)
	requestChan <- newLoadInfo
	leastLoadedHost := <-hostChan
	if leastLoadedHost.err == ErrFallbackToOriginalBehaviour {
		return nil, leastLoadedHost.err
	}
	if leastLoadedHost.err != nil {
		return connect(ctx, config) // fallback to original behaviour
	}
	if leastLoadedHost.hostname == config.Host {
		/*
			Discarding rest of the fallback option to handle multi host urls,
			since we want to fallback to the next least loaded server and not the next host of the url.
		*/
		if len(config.Fallbacks) > 0 {
			config.Fallbacks = config.Fallbacks[:1]
		}
		config.connString = replaceHostString(config.connString, leastLoadedHost.hostname, leastLoadedHost.port)
		return connectWithRetries(ctx, config.controlHost, config, newLoadInfo, leastLoadedHost)
	} else {
		newConnString := replaceHostString(config.connString, leastLoadedHost.hostname, leastLoadedHost.port)
		newConfig, err := ParseConfig(newConnString)
		if err != nil {
			return nil, err
		}
		/*
			Replacing Host, port, Fallbacks list and connstring in the user config,
			as per the least loaded server received.
		*/
		config.Host = newConfig.Host
		config.Port = newConfig.Port
		config.Fallbacks = newConfig.Fallbacks
		config.connString = newConfig.connString
		return connectWithRetries(ctx, config.controlHost, config, newLoadInfo, leastLoadedHost)
	}
}

func connectWithRetries(ctx context.Context, controlHost string, config *ConnConfig,
	newLoadInfo *ClusterLoadInfo, leastLoadedHost *lbHost) (c *Conn, er error) {
	var timeout time.Duration = 0
	if ctxDeadline, ok := ctx.Deadline(); ok {
		timeout = time.Until(ctxDeadline)
	}
	conn, err := connect(ctx, config)
	for i := 0; i < MAX_RETRIES && err != nil; i++ {
		decrementConnCount(config.controlHost + "," + config.Host)
		log.Warn().Msgf("Adding %s to unavailableHosts due to %s", config.Host, err.Error())
		newLoadInfo.unavailableHosts = map[string]int64{leastLoadedHost.hostname: time.Now().Unix()}
		requestChan <- newLoadInfo
		leastLoadedHost = <-hostChan
		if leastLoadedHost.err != nil {
			return nil, leastLoadedHost.err
		}
		if timeout > 0 {
			ctx, _ = context.WithTimeout(context.Background(), timeout)
		} else {
			ctx = context.Background()
		}
		if leastLoadedHost.hostname == config.Host {
			conn, err = connect(ctx, config)
		} else {
			/*
				Replacing Host, port, Fallbacks list and connstring in the user config,
				as per the least loaded server received.
			*/
			newConnString := replaceHostString(config.connString, leastLoadedHost.hostname, leastLoadedHost.port)
			newConfig, err1 := ParseConfig(newConnString)
			if err1 != nil {
				return nil, err1
			}
			config.Host = newConfig.Host
			config.Port = newConfig.Port
			config.Fallbacks = newConfig.Fallbacks
			config.controlHost = controlHost
			config.connString = newConfig.connString
			conn, err = connect(ctx, config)
		}
	}
	if err != nil {
		decrementConnCount(config.controlHost + "," + config.Host)
	}
	return conn, err
}

func decrementConnCount(str string) {
	requestChan <- &ClusterLoadInfo{
		clusterName: str,
		flags:       DECREMENT_COUNT,
	}
}

func markHostAway(li *ClusterLoadInfo, h string) {
	log.Warn().Msgf("Marking host %s as unreachable", h)
	delete(li.hostLoadPrimary, h)
	delete(li.hostLoadRR, h)
	delete(li.hostPairs, h)
	if li.unavailableHosts == nil {
		li.unavailableHosts = make(map[string]int64)
	}
	li.unavailableHosts[h] = time.Now().Unix()
}

func refreshLoadInfo(li *ClusterLoadInfo) error {
	li.ctrlCtx, _ = context.WithTimeout(context.Background(), CONTROL_CONN_TIMEOUT)
	if li.controlConn == nil || li.controlConn.IsClosed() {
		var err error
		ctrlConfig, err := ParseConfig(li.config.connString)
		if err != nil {
			log.Err(err).Msgf("refreshLoadInfo(): ParseConfig for control connection failed, %s", err.Error())
			return err
		}
		/*
			Replacing Host, port, Fallbacks list and connstring in the user config,
			as per the host on which control connection is attempted.
		*/
		li.config.Host = LookupIP(ctrlConfig.Host)
		li.config.Port = ctrlConfig.Port
		li.config.Fallbacks = ctrlConfig.Fallbacks
		li.config.connString = ctrlConfig.connString
		li.config.ConnectTimeout = CONTROL_CONN_TIMEOUT
		li.controlConn, err = connect(li.ctrlCtx, li.config)
		if err != nil {
			log.Warn().Msgf("Could not create control connection to %s\n", li.config.Host)
			// remove its hostLoad entry
			markHostAway(li, li.config.Host)
			li.controlConn = nil
			// Attempt connection to other servers which are already fetched in cli.
			if len(li.hostPairs) > 0 {
				log.Warn().Msgf("Attempting control connection to %d other servers ...\n", len(li.hostPairs))
			}
			for h := range li.hostPairs {
				newConnString := replaceHostString(li.config.connString, h, li.hostPort[h])
				/*
					Replacing Host, port, Fallbacks list and connstring in the user config,
					as per the host on which control connection is attempted.
				*/
				if ctrlConfig, err = ParseConfig(newConnString); err == nil {
					li.config.Host = ctrlConfig.Host
					li.config.Port = ctrlConfig.Port
					li.config.Fallbacks = ctrlConfig.Fallbacks
					li.config.connString = ctrlConfig.connString
					li.config.ConnectTimeout = CONTROL_CONN_TIMEOUT
					li.ctrlCtx, _ = context.WithTimeout(context.Background(), CONTROL_CONN_TIMEOUT)
					if li.controlConn, err = connect(li.ctrlCtx, li.config); err == nil {
						log.Info().Msgf("Created control connection to host %s", h)
						break
					}
					log.Warn().Msgf("Could not create control connection to host %s", h)
					markHostAway(li, li.config.Host)
					li.controlConn = nil
				}
			}
			if err != nil {
				log.Err(err).Msg("Failed to create control connection")
				return err
			}
		}
		li.config.controlHost = li.config.Host
	}
	// defer li.controlConn.Close(li.ctrlCtx)

	rows, err := li.controlConn.Query(li.ctrlCtx, LB_QUERY)
	if err != nil {
		log.Err(err).Msgf("Could not query load information: %s", err.Error())
		markHostAway(li, li.config.controlHost)
		li.controlConn = nil
		return refreshLoadInfo(li)
	}
	defer rows.Close()
	var host, nodeType, cloud, region, zone, publicIP string
	var port, numConns int
	newHostLoadPrimary := make(map[string]int)
	newHostLoadRR := make(map[string]int)
	newHostPort := make(map[string]uint16)
	newZoneListPrimary := make(map[string][]string)
	newZoneListRR := make(map[string][]string)
	newHostPairs := make(map[string]string)
	if li.unavailableHosts == nil {
		li.unavailableHosts = make(map[string]int64)
	}
	for rows.Next() {
		err := rows.Scan(&host, &port, &numConns, &nodeType, &cloud, &region, &zone, &publicIP)
		if err != nil {
			log.Err(err).Msgf("Could not read load information: %s", err.Error())
			markHostAway(li, li.config.controlHost)
			li.controlConn = nil
			return refreshLoadInfo(li)
		} else {
			host = LookupIP(host)
			publicIP = LookupIP(publicIP)
			newHostPairs[host] = publicIP
			tk := cloud + "." + region + "." + zone
			tk_star := cloud + "." + region // Used for topology_keys of type: cloud.region.*
			if nodeType == "primary" {
				setUpZoneList(newZoneListPrimary, tk, tk_star, host)
				newHostLoadPrimary[host] = li.hostLoadPrimary[host]
			} else {
				setUpZoneList(newZoneListRR, tk, tk_star, host)
				newHostLoadRR[host] = li.hostLoadRR[host]
			}
			newHostPort[host] = uint16(port)
		}
	}

	rsError := rows.Err()
	if rsError != nil {
		log.Err(err).Msgf("refreshLoadInfo(): Could not read load information, Rows.Err(): %s", rsError.Error())
		markHostAway(li, li.config.controlHost)
		li.controlConn = nil
		return refreshLoadInfo(li)
	}
	li.hostPort = newHostPort
	li.zoneListPrimary = newZoneListPrimary
	li.zoneListRR = newZoneListRR
	li.hostPairs = newHostPairs
	li.hostLoadPrimary = newHostLoadPrimary
	li.hostLoadRR = newHostLoadRR
	li.lastRefresh = time.Now()
	for uh, t := range li.unavailableHosts {
		if time.Now().Unix()-t > li.config.failedHostReconnectDelaySecs {
			// clear the unavailable-hosts list
			log.Info().Msgf("Removing %s from unavailableHosts Map", uh)
			if _, found := li.hostLoadPrimary[uh]; found {
				li.hostLoadPrimary[uh] = 0
			} else if _, found = li.hostLoadRR[uh]; found {
				li.hostLoadRR[uh] = 0
			}
			delete(li.unavailableHosts, uh)
		}
	}
	return nil
}

func setUpZoneList(zoneList map[string][]string, tk string, tk_star string, host string) {
	hosts, ok := zoneList[tk]
	if !ok {
		hosts = make([]string, 0)
	}
	hosts_star, ok_star := zoneList[tk_star]
	if !ok_star {
		hosts_star = make([]string, 0)
	}
	hosts = append(hosts, host)
	hosts_star = append(hosts_star, host)
	zoneList[tk] = hosts
	zoneList[tk_star] = hosts_star
}

func getHostWithLeastConns(li *ClusterLoadInfo) *lbHost {
	leastCnt := int(math.MaxInt32)
	leastLoaded := ""
	leastLoadedservers := make([]string, 0)
	zonelist := make(map[string][]string)
	hostload := make(map[string]int)
	if li.config.loadBalance == "only-rr" || li.config.loadBalance == "prefer-rr" {
		maps.Copy(zonelist, li.zoneListRR)
		maps.Copy(hostload, li.hostLoadRR)
	} else if li.config.loadBalance == "only-primary" || li.config.loadBalance == "prefer-primary" {
		maps.Copy(zonelist, li.zoneListPrimary)
		maps.Copy(hostload, li.hostLoadPrimary)
	} else {
		maps.Copy(zonelist, li.zoneListRR)
		maps.Copy(hostload, li.hostLoadRR)
		for k, v := range li.zoneListPrimary {
			hosts := zonelist[k]
			hosts = append(hosts, v...)
			zonelist[k] = hosts
		}
		maps.Copy(hostload, li.hostLoadPrimary)
	}
	if li.config.topologyKeys != nil {
		for i := 0; i < len(li.config.topologyKeys); i++ {
			var servers []string
			for _, tk := range li.config.topologyKeys[i] {
				toCheckStar := strings.Split(tk, ".")
				if toCheckStar[2] == "*" {
					tk = toCheckStar[0] + "." + toCheckStar[1]
				}
				servers = append(servers, zonelist[tk]...)
			}
			for _, h := range servers {
				if !isHostAway(li, h) {
					if hostload[h] < leastCnt {
						leastLoadedservers = nil
						leastLoadedservers = append(leastLoadedservers, h)
						leastCnt = hostload[h]
					} else if hostload[h] == leastCnt {
						leastLoadedservers = append(leastLoadedservers, h)
					}
				}
			}
			if leastCnt != int(math.MaxInt32) && len(leastLoadedservers) != 0 {
				break
			}
		}
	}
	if leastCnt == int(math.MaxInt32) && len(leastLoadedservers) == 0 {
		if !(li.config.loadBalance == "prefer-primary" || li.config.loadBalance == "prefer-rr") {
			if li.config.topologyKeys == nil || !li.config.fallbackToTopologyKeysOnly {
				leastCnt, leastLoadedservers = getHosts(li, hostload)
			} else {
				lbh := &lbHost{
					err: ErrFallbackToOriginalBehaviour,
				}
				return lbh
			}
		} else {
			leastCnt, leastLoadedservers = getHosts(li, hostload)
			if leastCnt == int(math.MaxInt32) && len(leastLoadedservers) == 0 {
				if li.config.loadBalance == "prefer-rr" {
					leastCnt, leastLoadedservers = getHosts(li, li.hostLoadPrimary)
				} else {
					leastCnt, leastLoadedservers = getHosts(li, li.hostLoadRR)
				}
			}
		}
	}

	if len(leastLoadedservers) != 0 {
		randomIndex, err := rand.Int(rand.Reader, big.NewInt(int64(len(leastLoadedservers))))
		if err != nil {
			log.Err(err).Msg("Could not select a leastloadedserver randomly")
		}
		leastLoaded = leastLoadedservers[randomIndex.Int64()]
	}

	if leastLoaded == "" {
		if li.flags == TRY_HOSTS_PUBLIC_IP {
			// remove all host (private ips) from unavailable list
			for h := range li.hostPairs {
				delete(li.unavailableHosts, h)
			}
			li.flags = HOSTS_EXHAUSTED
			return getHostWithLeastConns(li)
		}
		lbh := &lbHost{
			hostname: "",
			err:      errors.New(NO_SERVERS_MSG),
		}
		log.Warn().Msg("No hosts found, returning with NO_SERVERS_MSG")
		return lbh
	}
	leastLoadedToUse := leastLoaded
	if li.flags == USE_PUBLIC_IP || li.flags == HOSTS_EXHAUSTED {
		leastLoadedToUse = li.hostPairs[leastLoaded]
		if leastLoadedToUse == "" {
			lbh := &lbHost{
				hostname: "",
				err:      errors.New(NO_SERVERS_MSG),
			}
			log.Warn().Msg("No hosts and public ip found, returning with NO_SERVERS_MSG")
			return lbh
		}
	}
	lbh := &lbHost{
		hostname: leastLoadedToUse,
		port:     li.hostPort[leastLoaded],
		err:      nil,
	}
	if leastLoaded == leastLoadedToUse {
		if cnt, found := li.hostLoadPrimary[leastLoadedToUse]; found {
			li.hostLoadPrimary[leastLoadedToUse] = cnt + 1
		} else {
			li.hostLoadRR[leastLoadedToUse] = leastCnt + 1
		}
	} else {
		_, foundpublic := li.hostLoadPrimary[leastLoadedToUse]
		_, foundprivate := li.hostLoadPrimary[leastLoaded]
		if foundpublic || foundprivate {
			li.hostLoadPrimary[leastLoadedToUse] = leastCnt + 1
		} else {
			li.hostLoadRR[leastLoadedToUse] = leastCnt + 1
		}
	}
	return lbh
}

func getHosts(li *ClusterLoadInfo, hostLoad map[string]int) (int, []string) {
	leastCnt := int(math.MaxInt32)
	leastLoadedservers := make([]string, 0)
	for h := range hostLoad {
		if !isHostAway(li, h) {
			if hostLoad[h] < leastCnt {
				leastLoadedservers = nil
				leastLoadedservers = append(leastLoadedservers, h)
				leastCnt = hostLoad[h]
			} else if hostLoad[h] == leastCnt {
				leastLoadedservers = append(leastLoadedservers, h)
			}
		}
	}
	return leastCnt, leastLoadedservers
}

func isHostAway(li *ClusterLoadInfo, h string) bool {
	for awayHost := range li.unavailableHosts {
		if h == awayHost || h == li.hostPairs[awayHost] {
			return true
		}
	}
	return false
}

func refreshAndGetLeastLoadedHost(li *ClusterLoadInfo, awayHosts map[string]int64) *lbHost {
	if time.Now().Unix()-li.lastRefresh.Unix() > li.config.refreshInterval {
		err := refreshLoadInfo(li)
		if err != nil {
			return &lbHost{
				hostname: "",
				err:      err,
			}
		}
	}

	for h := range awayHosts {
		li.unavailableHosts[h] = awayHosts[h]
	}
	return getHostWithLeastConns(li)
}

// expects the toplogykeys in the format 'cloud1.region1.zone1,cloud1.region1.zone2,...'
func validateTopologyKeys(s string) ([]string, error) {
	tkeys := strings.Split(s, ",")
	for _, tk := range tkeys {
		zones1 := strings.Split(tk, ".")
		zones2 := strings.Split(tk, ":")
		if len(zones1) != 3 || len(zones2) > 2 {
			return nil, errors.New("toplogy_keys '" + s +
				"' not in correct format, should be specified as '<cloud>.<region>.<zone>,...'")
		}
	}
	return tkeys, nil
}

// expects the loadBalance to be one of "true", "false", "only-rr", "only-primary", "prefer-rr", "prefer-primary" and "any"
func validateLoadBalance(s string) bool {
	switch s {
	case
		"true",
		"false",
		"only-rr",
		"only-primary",
		"prefer-rr",
		"prefer-primary",
		"any":
		return true
	}

	return false
}

// For test purpose
func GetHostLoad() map[string]map[string]int {
	hl := make(map[string]map[string]int)
	for cluster := range clustersLoadInfo {
		hl[cluster] = make(map[string]int)
		for host, cnt := range clustersLoadInfo[cluster].hostLoadPrimary {
			hl[cluster][host] = cnt
		}
		for host, cnt := range clustersLoadInfo[cluster].hostLoadRR {
			hl[cluster][host] = cnt
		}
	}
	return hl
}

// For test purpose
func GetAZInfo() map[string]map[string][]string {
	az := make(map[string]map[string][]string)
	for n, cli := range clustersLoadInfo {
		az[n] = make(map[string][]string)
		copyZoneList(az[n], cli.zoneListPrimary)
		copyZoneList(az[n], cli.zoneListRR)
	}
	return az
}

func copyZoneList(azn map[string][]string, zl map[string][]string) {
	for z, hosts := range zl {
		q := strings.Split(z, ".")
		if len(q) == 3 {
			newzl := make([]string, len(hosts))
			copy(newzl, hosts)
			azn[z] = newzl
		}
	}

}

// For test purpose
func EmptyHostLoad() map[string]map[string]int {
	for cluster := range clustersLoadInfo {
		for host := range clustersLoadInfo[cluster].hostLoadPrimary {
			delete(clustersLoadInfo[cluster].hostLoadPrimary, host)
		}
		for host := range clustersLoadInfo[cluster].hostLoadRR {
			delete(clustersLoadInfo[cluster].hostLoadRR, host)
		}
	}
	return nil
}
