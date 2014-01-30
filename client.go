package mdns

import (
	"fmt"
	"github.com/miekg/dns"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// ServiceEntry is returned after we query for a service
type ServiceEntry struct {
	Name string
	Addr net.IP
	Port int
	Info string

	hasTXT bool
	sent   bool
}

// complete is used to check if we have all the info we need
func (s *ServiceEntry) complete() bool {
	return s.Addr != nil && s.Port != 0 && s.hasTXT
}

// LookupDomain looks up a given service, in a domain, waiting at most
// for a timeout before finishing the query. The results are streamed
// to a channel. Sends will not block, so clients should make sure to
// either read or buffer.
func LookupDomain(service, domain string, timeout time.Duration, entries chan<- *ServiceEntry) error {
	// Create a new client
	client, err := newClient()
	if err != nil {
		return err
	}
	defer client.Close()

	// Create the query name
	serviceAddr := fmt.Sprintf("%s.%s.", trimDot(service), trimDot(domain))

	// Run the query
	return client.query(serviceAddr, timeout, entries)
}

// Lookup is the same as LookupDomain, however it only searches in the "local"
// domain, and uses a one second lookup timeout.
func Lookup(service string, entries chan<- *ServiceEntry) error {
	return LookupDomain(service, "local", time.Second, entries)
}

// Client provides a query interface that can be used to
// search for service providers using mDNS
type client struct {
	ipv4List *net.UDPConn
	ipv6List *net.UDPConn

	closed    bool
	closedCh  chan struct{}
	closeLock sync.Mutex
}

// NewClient creates a new mdns Client that can be used to query
// for records
func newClient() (*client, error) {
	// Create a IPv4 listener
	ipv4, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		log.Printf("[ERR] mdns: Failed to bind to udp4 port: %v", err)
	}
	ipv6, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6zero, Port: 0})
	if err != nil {
		log.Printf("[ERR] mdns: Failed to bind to udp6 port: %v", err)
	}

	if ipv4 == nil && ipv6 == nil {
		return nil, fmt.Errorf("Failed to bind to any udp port!")
	}

	c := &client{
		ipv4List: ipv4,
		ipv6List: ipv6,
		closedCh: make(chan struct{}),
	}
	return c, nil
}

// Close is used to cleanup the client
func (c *client) Close() error {
	c.closeLock.Lock()
	defer c.closeLock.Unlock()

	if c.closed {
		return nil
	}
	c.closed = true
	close(c.closedCh)

	if c.ipv4List != nil {
		c.ipv4List.Close()
	}
	if c.ipv6List != nil {
		c.ipv6List.Close()
	}
	return nil
}

// query is used to perform a lookup and stream results
func (c *client) query(service string, timeout time.Duration, entries chan<- *ServiceEntry) error {
	// Start listening for response packets
	msgCh := make(chan *dns.Msg, 32)
	go c.recv(c.ipv4List, msgCh)
	go c.recv(c.ipv6List, msgCh)

	// Send the query
	m := new(dns.Msg)
	m.SetQuestion(service, dns.TypeANY)
	if err := c.sendQuery(m); err != nil {
		return nil
	}

	// Map the in-progress responses
	inprogress := make(map[string]*ServiceEntry)

	// Listen until we reach the timeout
	finish := time.After(timeout)
	for {
		select {
		case resp := <-msgCh:
			var inp *ServiceEntry
			for _, answer := range resp.Answer {
				switch rr := answer.(type) {
				case *dns.PTR:
					// Create new entry for this
					inp = ensureName(inprogress, rr.Ptr)

				case *dns.SRV:
					// Get the port
					inp = ensureName(inprogress, rr.Target)
					inp.Port = int(rr.Port)

				case *dns.TXT:
					// Pull out the txt
					inp = ensureName(inprogress, rr.Hdr.Name)
					inp.Info = strings.Join(rr.Txt, "|")
					inp.hasTXT = true

				case *dns.A:
					// Pull out the IP
					inp = ensureName(inprogress, rr.Hdr.Name)
					inp.Addr = rr.A

				case *dns.AAAA:
					// Pull out the IP
					inp = ensureName(inprogress, rr.Hdr.Name)
					inp.Addr = rr.AAAA
				}
			}

			// Check if this entry is complete
			if inp.complete() && !inp.sent {
				inp.sent = true
				select {
				case entries <- inp:
				default:
				}
			} else {
				// Fire off a node specific query
				m := new(dns.Msg)
				m.SetQuestion(inp.Name, dns.TypeANY)
				if err := c.sendQuery(m); err != nil {
					log.Printf("[ERR] mdns: Failed to query instance %s: %v", inp.Name, err)
				}
			}
		case <-finish:
			return nil
		}
	}
	return nil
}

// sendQuery is used to multicast a query out
func (c *client) sendQuery(q *dns.Msg) error {
	buf, err := q.Pack()
	if err != nil {
		return err
	}
	if c.ipv4List != nil {
		c.ipv4List.WriteTo(buf, ipv4Addr)
	}
	if c.ipv6List != nil {
		c.ipv6List.WriteTo(buf, ipv6Addr)
	}
	return nil
}

// recv is used to receive until we get a shutdown
func (c *client) recv(l *net.UDPConn, msgCh chan *dns.Msg) {
	if l == nil {
		return
	}
	buf := make([]byte, 65536)
	for !c.closed {
		n, err := l.Read(buf)
		if err != nil {
			continue
		}
		msg := new(dns.Msg)
		if err := msg.Unpack(buf[:n]); err != nil {
			log.Printf("[ERR] mdns: Failed to unpack packet: %v", err)
			continue
		}
		select {
		case msgCh <- msg:
		case <-c.closedCh:
			return
		}
	}
}

// ensureName is used to ensure the named node is in progress
func ensureName(inprogress map[string]*ServiceEntry, name string) *ServiceEntry {
	if inp, ok := inprogress[name]; ok {
		return inp
	}
	inp := &ServiceEntry{
		Name: name,
	}
	inprogress[name] = inp
	return inp
}
