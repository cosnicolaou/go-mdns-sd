// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package mdns

// This MDNS is used only to announce veyron name servers to each other.  It can run in
// conjunction with any native MDNS (at least on Linux).
//
// The discovery protocol and MDNS are from RFCs 6762 and 6763:
// 	client		->	question _veyronns._tcp.local. PTR
// 	each server	->	response _veyronns._tcp.local. PTR <hostid>._veyronns._tcp.local
//					 <hostid>._veyronns._tcp.local. TXT <descriptive string>*
//				         <hostid>._veyronns._tcp.local. SRV <hostid>.local. <port> 0 0
//					 <hostid>.local. A <v4 ip address>
//					 <hostid>.local. AAAA <v6 ip address>
//
// This server uses all ip interfaces that have non loop back addresses.  It does not leak information
// between interfaces.

// There are three main types here.   MDNS represents the service itself.   One can have multiple of
// these running at once, each bound to different multicast-address:port.
//
// Each MDNS connects to all IP interfaces that have ipv4 or ipv6 addresses.  A multicastIfc exists
// for each of these interfaces (ipv4 and ipv6 each have their own multicastIfc since they are considered
// to be different networks even if on the same wire).
//
// Each multicastIfc has a cache of information learned from its network.

import (
	"code.google.com/p/mdns/go_dns"
	"errors"
	"fmt"
	"log"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"
)

// All incoming network messages carries enough context for a network appropriate response.
type msgFromNet struct {
	mifc   *multicastIfc // Interface to reply on
	sender *net.UDPAddr  // Address to reply to (if non-multicast: TODO)
	msg    *dns.Msg
}

// A multicast interface that we are listening on.  Each physical interface can have both v4 and v6 multicast interfaces.
type multicastIfc struct {
	// Info about the physical interface and address range covered (just for debugging).
	ifc net.Interface

	// Multicast Address
	addr *net.UDPAddr

	// IP version
	ipver int

	// Address ranges on this interface (used for detecting changed interfaces)
	addresses []*net.IPNet

	// The connection for talking on the internet.
	conn *net.UDPConn

	// We keep the cache interface specific because, absent connectivity info, we have to treat each network as separate.
	cache *rrCache

	// MDNS we are a child of.
	mdns *MDNS

	// Set to true to terminate any waiting thread.
	doneLock sync.Mutex
	done     bool
}

func newMulticastIfc(ipver int, ifc net.Interface, addr *net.UDPAddr, addresses []*net.IPNet, mdns *MDNS) *multicastIfc {
	return &multicastIfc{
		ifc:       ifc,
		addr:      addr,
		addresses: addresses,
		cache:     newRRCache(),
		mdns:      mdns,
		ipver:     ipver,
	}
}

func (m *multicastIfc) run() bool {
	m.doneLock.Lock()
	defer m.doneLock.Unlock()
	return !m.done
}

func (m *multicastIfc) stop() {
	m.doneLock.Lock()
	m.done = true
	m.doneLock.Unlock()
}

func (m *multicastIfc) String() string {
	return fmt.Sprintf("%d v%d %s multicast addr %s", m.ifc.Index, m.ipver, m.ifc.Name, m.addr)
}

// Append host addresses to the answer section.
func (m *multicastIfc) appendHostAddresses(msg *dns.Msg, host string, rrtype int, ttl uint32) {
	hostDN := hostFQDN(host)
	for _, address := range m.addresses {
		switch rrtype {
		case dns.TypeALL:
			msg.Answer = append(msg.Answer, NewAddressRR(hostDN, 0x8000|dns.ClassINET, ttl, address.IP))
		case dns.TypeA:
			if v4 := address.IP.To4(); v4 != nil {
				msg.Answer = append(msg.Answer, NewAddressRR(hostDN, 0x8000|dns.ClassINET, ttl, v4))
			}
		case dns.TypeAAAA:
			if v4 := address.IP.To4(); v4 == nil {
				msg.Answer = append(msg.Answer, NewAddressRR(hostDN, 0x8000|dns.ClassINET, ttl, address.IP))
			}
		}
	}
}

func (m *multicastIfc) appendSrvRR(msg *dns.Msg, service, host string, port uint16, ttl uint32) {
	hostDN := hostFQDN(host)
	uniqueServiceDN := instanceFQDN(host, service)
	msg.Answer = append(msg.Answer, NewSrvRR(uniqueServiceDN, 0x8000|dns.ClassINET, ttl, hostDN, port, 0, 0))
}

func (m *multicastIfc) appendTxtRR(msg *dns.Msg, service, host string, txt []string, ttl uint32) {
	uniqueServiceDN := instanceFQDN(host, service)
	msg.Answer = append(msg.Answer, NewTxtRR(uniqueServiceDN, 0x8000|dns.ClassINET, ttl, txt))
}

// Append service discovery records to the answer section.
func (m *multicastIfc) appendDiscoveryRecords(msg *dns.Msg, service, host string, port uint16, txt []string, ttl uint32) {
	serviceDN := serviceFQDN(service)
	uniqueServiceDN := instanceFQDN(host, service)
	msg.Answer = append(msg.Answer, NewPtrRR(serviceDN, dns.ClassINET, ttl, uniqueServiceDN))
	m.appendTxtRR(msg, service, host, txt, ttl)
	m.appendSrvRR(msg, service, host, port, ttl)
	m.appendHostAddresses(msg, host, dns.TypeALL, ttl)
}

// Send a message on a multicast net and cache it locally.
func (m *multicastIfc) sendMessage(msg *dns.Msg) {
	if m.mdns.debug {
		log.Printf("sending message %v\n", msg)
	}
	buf, ok := msg.Pack()
	if !ok {
		log.Printf("can't pack address message\n")
		return
	}
	if _, err := m.conn.WriteTo(buf, m.addr); err != nil {
		log.Printf("WriteTo failed %v %v", m.addr, err)
	}

	// Cache these RRs in case we ask about ourself.
	for _, rr := range msg.Answer {
		if m.cache.Add(rr) {
			m.mdns.changedRR(rr)
		}
	}
}

// Announce the address records for a host.
func (m *multicastIfc) announceHost(host string, ttl uint32) {
	msg := newDnsMsg(0, true, true)
	m.appendHostAddresses(msg, host, dns.TypeALL, ttl)
	m.sendMessage(msg)
}

// Announce a service and how to reach it.
func (m *multicastIfc) announceService(service, host string, port uint16, txt []string, ttl uint32) {
	msg := newDnsMsg(0, true, true)
	m.appendDiscoveryRecords(msg, service, host, port, txt, ttl)
	m.sendMessage(msg)
}

// Ask a question.
func (m *multicastIfc) sendQuestion(q []dns.Question) {
	msg := newDnsMsg(0, false, false)
	msg.Question = q
	m.sendMessage(msg)
}

type lookupRequest struct {
	name   string
	rrtype uint16
	rc     chan dns.RR
}

type announceRequest struct {
	service string
	host    string
	port    uint16
	txt     []string
}

type watchedService struct {
	c   *sync.Cond
	gen int
}

type MDNS struct {
	// Addresses to multicast on.
	v4addr, v6addr *net.UDPAddr

	// Multicast interfaces to listen on.
	mifcsLock sync.RWMutex
	mifcs     map[string]*multicastIfc

	// Set to true to get threads to exit.
	doneLock sync.Mutex
	done     bool

	// Channel to pass incoming networlmessages to the main loop.
	fromNet chan *msgFromNet

	// All access methods turn into channel requests to the main loop to make synchronization trivial.
	announce     chan announceRequest
	lookup       chan lookupRequest
	refreshAlarm chan struct{}
	cleanupAlarm chan struct{}

	// The host name.
	hostName string

	// Services we are announcing and their ports
	services map[string]announceRequest

	// Services whose memberships are being watched or subscribed to.
	watchedLock sync.RWMutex
	watched     map[string]*watchedService
	subscribed  map[string]bool

	// TTL to use for outgoing RRs.
	ttl uint32

	debug    bool
	loopback bool
}

func losecolons(x string) string {
	var y string
	for _, c := range x {
		if c != ':' {
			y = y + string(c)
		}
	}
	return y
}

// Create a new MDNS service.
func NewMDNS(host, v4addr, v6addr string, loopback, debug bool) (s *MDNS, err error) {
	s = new(MDNS)
	if v4addr == "" {
		v4addr = "224.0.0.251:5353"
	}
	if v6addr == "" {
		v6addr = "[FF02::FB]:5353"
	}
	if s.v4addr, err = net.ResolveUDPAddr("udp", v4addr); err != nil {
		return nil, err
	}
	if s.v6addr, err = net.ResolveUDPAddr("udp", v6addr); err != nil {
		return nil, err
	}
	s.debug = debug
	s.loopback = loopback
	s.hostName = host
	s.ttl = 120

	// Allocate channels for communications internal to MDNS
	s.fromNet = make(chan *msgFromNet, 10)
	s.announce = make(chan announceRequest)
	s.lookup = make(chan lookupRequest)
	s.refreshAlarm = make(chan struct{})
	s.cleanupAlarm = make(chan struct{})

	s.services = make(map[string]announceRequest, 0)
	s.watched = make(map[string]*watchedService, 0)
	s.subscribed = make(map[string]bool, 0)
	s.mifcs = make(map[string]*multicastIfc, 0)

	highesthwaddr, err := s.ScanInterfaces()
	if err != nil {
		log.Fatalf("scanning interfaces: %s", err)
	}

	// If we weren't passed in a host name, use the highest lexicographic hardware address.
	if len(s.hostName) == 0 {
		s.hostName = losecolons(highesthwaddr)
	}

	go s.mainLoop()
	go s.alarmClock()

	// Announce ourselves if the host name is set.
	if len(s.hostName) > 0 {
		s.refreshAlarm <- struct{}{}
	}

	return s, nil
}

func equalAddresses(al, bl []*net.IPNet) bool {
	// We're assuming no duplicates in the lists.
	if len(al) != len(bl) {
		return false
	}
	// N squared but for small N.
L:
	for _, a := range al {
		for _, b := range bl {
			if a.IP.Equal(b.IP) {
				continue L
			}
		}
		return false
	}
	return true
}

// ScanInterfaces looks for changes in the interface list and makes sure we are using them
// for mdns.
func (s *MDNS) ScanInterfaces() (string, error) {
	highesthwaddr := ""

	// Figure out which interfaces we have that we need to listen on.
	ifcs, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	newmifcs := make(map[string]*multicastIfc, 0)

	for _, ifc := range ifcs {
		addresses, addrErr := ifc.Addrs()
		if addrErr != nil {
			log.Printf("Addrs() failed: %s", addrErr)
			continue
		}

		// See if interface has non-loopback v4 or v6 interfaces.  Remember the useful addresses.
		hasv4 := false
		hasv6 := false
		var okAddresses []*net.IPNet
		hwaddr := ifc.HardwareAddr.String()
		if hwaddr > highesthwaddr {
			highesthwaddr = hwaddr
		}
		key := fmt.Sprintf("%s+%d+%s", ifc.Name, ifc.Index, hwaddr)

		for _, address := range addresses {
			switch address := address.(type) {
			case *net.IPNet:
				// We either use loopback or non-loopback interfaces (generally loopback is for testing).
				if (address.IP.IsLoopback() && !s.loopback) || (!address.IP.IsLoopback() && s.loopback) {
					log.Printf("skipping ifc %d %s %s\n", ifc.Index, ifc.Name, address)
					continue
				}

				if address.IP.To4() != nil {
					hasv4 = true
				} else {
					hasv6 = true
				}
				okAddresses = append(okAddresses, address)
			}
		}
		if okAddresses == nil {
			continue
		}
		if hasv4 {
			newmifcs["4+"+key] = newMulticastIfc(4, ifc, s.v4addr, okAddresses, s)
		}
		if hasv6 {
			newmifcs["6+"+key] = newMulticastIfc(6, ifc, s.v6addr, okAddresses, s)
		}
	}

	// If any interfaces disappeared or changed addresses, remove them.
	s.mifcsLock.Lock()
	defer s.mifcsLock.Unlock()
	for k, m := range s.mifcs {
		if newm, ok := newmifcs[k]; ok {
			if equalAddresses(newm.addresses, m.addresses) {
				continue
			}
		}
		m.stop()
		log.Printf("removing ifc %s", m)
		delete(s.mifcs, k)
	}

	// Create any missing interfaces.
	for k, newm := range newmifcs {
		if _, ok := s.mifcs[k]; ok {
			continue
		}
		conn, err := net.ListenMulticastUDP("udp", &newm.ifc, newm.addr)
		if err != nil {
			log.Printf("ListenMulticastUDP %s: %v\n", newm, err)
			continue
		}
		if err := SetMulticastTTL(conn, newm.ipver, 255); err != nil {
			log.Printf("SetMulticastTTL %s: %v\n", newm, err)
		}
		if err := SetMulticastLoopback(conn, newm.ipver, true); err != nil {
			log.Printf("SetMulticastLoopback %s: %v\n", newm, err)
		}
		newm.conn = conn
		s.mifcs[k] = newm
		go s.udpListener(newm)

		// Broadcast a request for any services to which we are subscribed.  If we are
		// also an instance of the service we will respond to our own request with a
		// broadcast.  If we have any watchers for the service, they too will be awakened
		// by the responses.
		s.watchedLock.RLock()
		for sdn := range s.subscribed {
			newm.sendQuestion([]dns.Question{{sdn, dns.TypePTR, dns.ClassINET}})
		}
		s.watchedLock.RUnlock()
	}
	return highesthwaddr, nil
}

// Change the ttl for outgoing records to something other than the default.
func (s *MDNS) SetOutgoingTTL(ttl uint32) {
	s.ttl = ttl
}

// A go routine to listen for packets on a network.  Pass to the main loop with sufficient information to
// answer on the same interface.
func (s *MDNS) udpListener(ifc *multicastIfc) {
	log.Printf("MDNS listening on %s with %v", ifc, ifc.addresses)

	b := make([]byte, 2048)
	for ifc.run() && s.run() {
		n, a, err := ifc.conn.ReadFromUDP(b)
		if err != nil {
			log.Printf("error reading from udp: %v", err)
		}

		// convert to dns packet
		msg := new(dns.Msg)
		if !msg.Unpack(b[0:n]) {
			log.Printf("couldn't unpack %d byte dns msg from %v", n, a)
		} else {
			s.fromNet <- &msgFromNet{ifc, a, msg}
		}
	}
}

// A go routine to wake up the main loop periodicly.  We need this to 'refresh' what we have advertised to the network.
func (s *MDNS) alarmClock() {
	for s.run() {
		time.Sleep(110 * time.Second)
		s.refreshAlarm <- struct{}{}
		s.cleanupAlarm <- struct{}{}
	}
}

func serviceFQDN(service string) string {
	if strings.HasSuffix(service, ".") {
		return service
	}
	return "_" + service + "._tcp.local."
}

func instanceFQDN(instance, service string) string {
	if strings.HasSuffix(instance, ".") {
		return instance
	}
	return instance + "." + serviceFQDN(service)
}

func instanceUnqualify(instance, service string) string {
	return strings.TrimSuffix(instance, "."+serviceFQDN(service))
}

func hostFQDN(host string) string {
	if strings.HasSuffix(host, ".") {
		return host
	}
	return host + ".local."
}

func hostUnqualify(host string) string {
	host = strings.TrimSuffix(host, ".local.")
	return strings.TrimSuffix(host, ".")
}

func serviceFQDNFromInstanceFQDN(instance string) string {
	pieces := strings.SplitAfterN(instance, ".", 2)
	if len(pieces) != 2 {
		return ""
	}
	return pieces[1]
}

func (s *MDNS) answerA(m *msgFromNet, q dns.Question, msg *dns.Msg) {
	if q.Name == hostFQDN(s.hostName) {
		m.mifc.appendHostAddresses(msg, s.hostName, dns.TypeA, s.ttl)
		return
	}
	for _, req := range s.services {
		if q.Name == hostFQDN(req.host) {
			m.mifc.appendHostAddresses(msg, req.host, dns.TypeA, s.ttl)
			return
		}
	}
}

func (s *MDNS) answerAAAA(m *msgFromNet, q dns.Question, msg *dns.Msg) {
	if q.Name == hostFQDN(s.hostName) {
		m.mifc.appendHostAddresses(msg, s.hostName, dns.TypeAAAA, s.ttl)
		return
	}
	for _, req := range s.services {
		if q.Name == hostFQDN(req.host) {
			m.mifc.appendHostAddresses(msg, req.host, dns.TypeAAAA, s.ttl)
			return
		}
	}
}

func (s *MDNS) answerPTR(m *msgFromNet, q dns.Question, msg *dns.Msg) {
	for service, req := range s.services {
		if q.Name == serviceFQDN(service) {
			m.mifc.appendDiscoveryRecords(msg, service, req.host, req.port, req.txt, s.ttl)
			return
		}
	}
}

func (s *MDNS) answerSRV(m *msgFromNet, q dns.Question, msg *dns.Msg) {
	for service, req := range s.services {
		if q.Name == instanceFQDN(req.host, service) {
			m.mifc.appendSrvRR(msg, service, req.host, req.port, s.ttl)
			m.mifc.appendHostAddresses(msg, req.host, dns.TypeALL, s.ttl)
			break
		}
	}
}

func (s *MDNS) answerTXT(m *msgFromNet, q dns.Question, msg *dns.Msg) {
	for service, req := range s.services {
		if q.Name == instanceFQDN(req.host, service) {
			m.mifc.appendTxtRR(msg, service, req.host, req.txt, s.ttl)
			break
		}
	}
}

// Answer a question received from the network if it is for our host address or a service we know about.
func (s *MDNS) answerQuestionFromNet(m *msgFromNet) {
	msg := newDnsMsg(0, true, true)
	for _, q := range m.msg.Question {
		switch q.Qtype {
		case dns.TypeA:
			s.answerA(m, q, msg)
		case dns.TypeAAAA:
			s.answerAAAA(m, q, msg)
		case dns.TypePTR:
			s.answerPTR(m, q, msg)
		case dns.TypeSRV:
			s.answerSRV(m, q, msg)
		case dns.TypeTXT:
			s.answerTXT(m, q, msg)
		case dns.TypeALL:
			s.answerA(m, q, msg)
			s.answerAAAA(m, q, msg)
			s.answerPTR(m, q, msg)
			s.answerSRV(m, q, msg)
			s.answerTXT(m, q, msg)
		}
	}
	if len(msg.Answer) > 0 {
		m.mifc.sendMessage(msg)
	}
}

// Main loop, acts on incoming messages and resolution requests and announcements.  We do pretty much everything
// in this loop to sequentialize all structure access.
func (s *MDNS) mainLoop() {
	for s.run() {
		select {
		case m := <-s.fromNet:
			if m.msg.Response {
				// Cache the information.
				if s.debug {
					log.Printf("%s: response %v\n", s.hostName, m.msg)
				}
				for _, rr := range m.msg.Answer {
					if m.mifc.cache.Add(rr) {
						s.changedRR(rr)
					}
				}
			} else {
				// Answer the question (only if we have a host name)
				if s.hostName == "" {
					break
				}
				if s.debug {
					log.Printf("%s: question %v\n", s.hostName, m.msg)
				}
				s.answerQuestionFromNet(m)
			}
		case req := <-s.announce:
			// Adding a service
			s.services[req.service] = req
			log.Printf("adding service %s %s %d %v\n", req.service, req.host, req.port, req.txt)

			// Tell all the networks about the name
			for _, mifc := range s.mifcs {
				mifc.announceService(req.service, req.host, req.port, req.txt, s.ttl)
			}
		case req := <-s.lookup:
			// Reply with all matching requests from all interfaces and then close the channel.
			for _, mifc := range s.mifcs {
				mifc.cache.Lookup(req.name, req.rrtype, req.rc)
			}
			close(req.rc)
		case <-s.refreshAlarm:
			// Reannounce all services.  We need to do this before the TTLs run out.  As a side
			// effect this reannounces the host address RRs.
			if len(s.hostName) == 0 {
				break
			}
			if len(s.services) == 0 {
				for _, mifc := range s.mifcs {
					mifc.announceHost(s.hostName, s.ttl)
				}
			} else {
				for service, req := range s.services {
					for _, mifc := range s.mifcs {
						mifc.announceService(service, req.host, req.port, req.txt, s.ttl)
					}
				}
			}
		case <-s.cleanupAlarm:
			for _, mifc := range s.mifcs {
				rrs := mifc.cache.CleanExpired()
				for _, rr := range rrs {
					s.changedRR(rr)
				}
			}
		}
	}
}

// Stop all udpListeners.
func (s *MDNS) Stop() {
	s.doneLock.Lock()
	s.done = true
	s.doneLock.Unlock()
	for _, mifc := range s.mifcs {
		mifc.conn.Close()
	}
}

func (s *MDNS) run() bool {
	s.doneLock.Lock()
	defer s.doneLock.Unlock()
	return !s.done
}

// Announce a service.  If the host name is empty, we just use the host name from NewMDNS.  If the host name ends in .local. we strip it off.
func (s *MDNS) AddService(service, host string, port uint16, txt ...string) error {
	if len(service) == 0 {
		return errors.New("service name cannot be null")
	}
	if len(host) == 0 {
		host = s.hostName
	} else {
		host = hostUnqualify(host)
	}
	s.announce <- announceRequest{service, host, port, txt}
	return nil
}

// Resolve a particular RR type.
func (s *MDNS) ResolveRR(dn string, rrtype uint16) []dns.RR {
	dn = hostFQDN(dn)
	rrs := make([]dns.RR, 0)
	for i := 0; i < 3; i++ {
		// Try cache.
		req := lookupRequest{dn, rrtype, make(chan dns.RR, 10)}
		s.lookup <- req
		for rr := <-req.rc; rr != nil; rr = <-req.rc {
			rrs = append(rrs, rr)
		}
		if len(rrs) > 0 || i >= 3 {
			break
		}

		// Ask the net to resolve it
		q := make([]dns.Question, 1)
		q[0] = dns.Question{dn, rrtype, dns.ClassINET}
		for _, mifc := range s.mifcs {
			mifc.sendQuestion(q)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return rrs
}

// Resolve an address from the cache.
func (s *MDNS) resolveAddressFromCache(dn string, rrmap map[string]net.IP, minttl uint32) uint32 {
	req := lookupRequest{dn, dns.TypeALL, make(chan dns.RR, 10)}
	s.lookup <- req
	for rr := <-req.rc; rr != nil; rr = <-req.rc {
		switch rr := rr.(type) {
		case *dns.RR_A:
			ip := AtoIP(rr)
			rrmap[ip.String()] = ip
		case *dns.RR_AAAA:
			ip := AAAAtoIP(rr)
			rrmap[ip.String()] = ip
		}
	}
	return minttl
}

// ResolveToAddress return all IP addresses for a domain name (from all interfaces).  These come from A and AAAA RR's for the name <host>.local.
// We use a map to dedup replies and then make a slice out of the map values. It also returns the lowest TTL of all the address records.
func (s *MDNS) ResolveAddress(dn string) ([]net.IP, uint32) {
	dn = hostFQDN(dn)
	rrmap := make(map[string]net.IP, 0)
	minttl := uint32(7 * 24 * 60 * 60)
	for i := 0; i < 3; i++ {
		minttl = s.resolveAddressFromCache(dn, rrmap, minttl)
		if len(rrmap) != 0 || i >= 3 {
			break
		}

		// if the cache has no answers, ask the nets and wait for replies to be collected
		q := make([]dns.Question, 2)
		q[0] = dns.Question{dn, dns.TypeA, dns.ClassINET}
		q[1] = dns.Question{dn, dns.TypeAAAA, dns.ClassINET}
		for _, mifc := range s.mifcs {
			mifc.sendQuestion(q)
		}
		time.Sleep(50 * time.Millisecond)
	}

	var ips []net.IP
	for _, ip := range rrmap {
		ips = append(ips, ip)
	}
	return ips, minttl
}

// SubscriberToService declare our interest in a service.  This should elicit responses from everyone implementing that service.  This is
// orthogonal to offering the service ourselves.
func (s *MDNS) SubscribeToService(service string) {
	serviceDN := serviceFQDN(service)
	q := []dns.Question{{serviceDN, dns.TypePTR, dns.ClassINET}}
	s.watchedLock.Lock()
	s.subscribed[serviceDN] = true
	s.watchedLock.Unlock()
	s.mifcsLock.RLock()
	defer s.mifcsLock.RUnlock()
	for _, mifc := range s.mifcs {
		mifc.sendQuestion(q)
	}
}

type ServiceInstance struct {
	Name   string
	SrvRRs []*dns.RR_SRV
	TxtRRs []*dns.RR_TXT
}

// ResolveInstance returns the address records, the port, and the min ttl for a single service instance.
func (s *MDNS) ResolveInstance(instance, service string) ServiceInstance {
	si := ServiceInstance{Name: instanceUnqualify(instance, service)}
	dn := instanceFQDN(instance, service)
	for _, rr := range s.ResolveRR(dn, dns.TypeSRV) {
		switch rr := rr.(type) {
		case *dns.RR_SRV:
			si.SrvRRs = append(si.SrvRRs, rr)
		}
	}
	for _, rr := range s.ResolveRR(dn, dns.TypeTXT) {
		switch rr := rr.(type) {
		case *dns.RR_TXT:
			si.TxtRRs = append(si.TxtRRs, rr)
		}
	}
	return si
}

// ServiceMemberDiscovery returns all the members of a service (i.e. with a PTR record).
func (s *MDNS) ServiceMemberDiscovery(service string) []string {
	dn := serviceFQDN(service)

	// Conmpute all unique members.
	memberMap := make(map[string]struct{}, 0)
	req := lookupRequest{dn, dns.TypePTR, make(chan dns.RR, 10)}
	s.lookup <- req
	for rr := <-req.rc; rr != nil; rr = <-req.rc {
		switch rr := rr.(type) {
		case *dns.RR_PTR:
			memberMap[rr.Ptr] = struct{}{}
		}
	}
	var reply []string
	for member := range memberMap {
		reply = append(reply, member)
	}

	return reply
}

// ServiceDiscovery returns all current instances of a service (i.e. with a SRV record).
// We assume the user has already subscribed to the service to get systems on
// the network to multicast their entries.
func (s *MDNS) ServiceDiscovery(service string) []ServiceInstance {
	// Get the current set of members.
	members := s.ServiceMemberDiscovery(service)

	// Loop trying to fulfill the request.
	resolved := make([]ServiceInstance, 0)
	for i := 0; i < 3; i++ {
		if i != 0 {
			// Don't sleep the first time around.
			time.Sleep(50 * time.Millisecond)
		}
		var q []dns.Question
		var unresolved []string
		// First get what the is in the cache.
		for _, member := range members {
			var txtRRs []*dns.RR_TXT
			srvmap := make(map[string]*dns.RR_SRV, 0)
			req := lookupRequest{member, dns.TypeALL, make(chan dns.RR, 10)}
			s.lookup <- req
			for rr := <-req.rc; rr != nil; rr = <-req.rc {
				switch rr := rr.(type) {
				case *dns.RR_SRV:
					// It is a mistake to have two srv rrs with the
					// same target so we just remember the last seen.
					srvmap[rr.Target] = rr
				case *dns.RR_TXT:
					// We may get the same text record from multiple networks so
					// we need to suppress dups.
					found := false
					for i := range txtRRs {
						if reflect.DeepEqual(rr.Txt, txtRRs[i].Txt) {
							found = true
							break
						}
					}
					if !found {
						txtRRs = append(txtRRs, rr)
					}
				}
			}
			// We need at least one of each flavor or we'll ask the net for more records.
			if len(srvmap) == 0 || txtRRs == nil {
				unresolved = append(unresolved, member)
				if len(srvmap) == 0 {
					q = append(q, dns.Question{member, dns.TypeSRV, dns.ClassINET})
				}
				if txtRRs == nil {
					q = append(q, dns.Question{member, dns.TypeTXT, dns.ClassINET})
				}
			} else {
				var srvRRs []*dns.RR_SRV
				for _, rr := range srvmap {
					srvRRs = append(srvRRs, rr)
				}
				resolved = append(resolved, ServiceInstance{Name: instanceUnqualify(member, service), SrvRRs: srvRRs, TxtRRs: txtRRs})
			}
		}
		if q == nil {
			// Nothing left to ask for.
			break
		}

		// Ask the net for everything that's missing in a single request.  We are assuming that it
		// will fit in one request.  If not, the message will not be sent and we'll have to wait for
		// the systems to refresh.
		//
		// Note that we may ask but not wait around for the answer (should the loopterminate).
		// That is purposeful, i.e., priming the pump should the caller retry.
		members = unresolved
		for _, mifc := range s.mifcs {
			mifc.sendQuestion(q)
		}
	}
	return resolved
}

// changedRR is called after we add a new record to the cache.  Check to see if a watched service
// has changed and wake up the corresponding watcher routines.
func (s *MDNS) changedRR(rr dns.RR) {
	dn := rr.Header().Name
	switch rr.(type) {
	case *dns.RR_PTR:
		// Nothing to do here but we don't want to hit the default.
	case *dns.RR_TXT:
		dn = serviceFQDNFromInstanceFQDN(dn)
		if len(dn) == 0 {
			return
		}
	case *dns.RR_SRV:
		dn = serviceFQDNFromInstanceFQDN(dn)
		if len(dn) == 0 {
			return
		}
	default:
		return
	}
	if s.debug {
		log.Printf("%s: changed %v\n", s.hostName, rr)
	}
	s.watchedLock.RLock()
	if w, ok := s.watched[dn]; ok {
		w.c.L.Lock()
		w.gen++
		w.c.L.Unlock()
		w.c.Broadcast()
	}
	s.watchedLock.RUnlock()
}

// deepEqual returns true of both ServiceInstance's are equivalent except for TTLs.  If it
// wasn't for TTL we'ld be able to use reflect.DeepEqual.
//
// Normally they'll each only contain one each of a TXT and SRV RR so it normally goes pretty fast.
func deepEqual(a, b *ServiceInstance) bool {
	if len(a.TxtRRs) != len(b.TxtRRs) {
		return false
	}
	for _, arr := range a.TxtRRs {
		found := false
		for _, brr := range b.TxtRRs {
			if reflect.DeepEqual(arr.Txt, brr.Txt) {
				found = true
				break
			}
		}
		if found == false {
			return false
		}
	}
	if len(a.SrvRRs) != len(b.SrvRRs) {
		return false
	}
	for _, arr := range a.SrvRRs {
		found := false
		for _, brr := range b.SrvRRs {
			if arr.Target == brr.Target && arr.Port == brr.Port && arr.Priority == brr.Priority && arr.Weight == brr.Weight {
				found = true
				break
			}
		}
		if found == false {
			return false
		}
	}
	return true
}

// serviceMemberWatcher gets signalled each time membership might have changed.
func (s *MDNS) serviceMemberWatcher(service string, w *watchedService, reply chan ServiceInstance) {
	// A client signals its intent to stop by closing the channel.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("serviceMemberWatcher recover: %s", r)
		}
	}()

	var old map[string]ServiceInstance

	// Loop waiting for changes and tell any to client.
	gen := 0
	for {
		// Get current membership.
		current := make(map[string]ServiceInstance, 0)
		for _, x := range s.ServiceDiscovery(service) {
			current[x.Name] = x
		}

		for okey, oval := range old {
			if cval, ok := current[okey]; !ok {
				// Entry disappeared.  A message with nil pointers is the signal.
				oval.SrvRRs = nil
				oval.TxtRRs = nil
				reply <- oval
			} else {
				// See if anything changed other than TTLs.
				if !deepEqual(&oval, &cval) {
					reply <- cval
				}
			}
		}
		for ckey, cval := range current {
			if _, ok := old[ckey]; !ok {
				// A new instance.
				reply <- cval
			}
		}
		old = current

		// Wait for the next change.
		w.c.L.Lock()
		for gen == w.gen {
			w.c.Wait()
		}
		gen = w.gen
		w.c.L.Unlock()
	}
}

// ServiceMemberWatch returns a reply channel over which membership changes are announced.
// A zero TTL means that the instance is no longer a member.
func (s *MDNS) ServiceMemberWatch(service string) chan ServiceInstance {
	serviceDN := serviceFQDN(service)

	// Make sure we have a broadcast condition
	c := make(chan ServiceInstance, 20)
	s.watchedLock.Lock()
	if _, ok := s.watched[serviceDN]; !ok {
		s.watched[serviceDN] = &watchedService{c: sync.NewCond(new(sync.Mutex))}
	}
	s.watchedLock.Unlock()

	// Fire off a go routine to do the actual watching.  This lives forever.
	go s.serviceMemberWatcher(service, s.watched[serviceDN], c)
	return c
}

// Hostname return our chosen host name.
func (s *MDNS) Hostname() string {
	return s.hostName
}
