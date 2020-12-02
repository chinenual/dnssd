package dnssd

import (
	"context"
	"fmt"
	"github.com/brutella/dnssd/log"
	"github.com/miekg/dns"
	"math/rand"
	"net"
	"strings"
	"time"
)

// ProbeService probes for the hostname and service instance name of srv.
// If err == nil, the returned service is verified to be unique on the local network.
func ProbeService(ctx context.Context, srv Service) (Service, error) {
	conn, err := newMDNSConn()

	if err != nil {
		return srv, err
	}

	defer conn.close()

	// After one minute of probing, if the Multicast DNS responder has been
	// unable to find any unused name, it should log an error (RFC6762 9)
	probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	// When ready to send its Multicast DNS probe packet(s) the host should
	// first wait for a short random delay time, uniformly distributed in
	// the range 0-250 ms. (RFC6762 8.1)
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	delay := time.Duration(r.Intn(250)) * time.Millisecond
	log.Debug.Println("Probing delay", delay)
	time.Sleep(delay)

	return probeService(probeCtx, conn, srv, 1*time.Millisecond, false)
}

func ReprobeService(ctx context.Context, srv Service) (Service, error) {
	conn, err := newMDNSConn()

	if err != nil {
		return srv, err
	}

	defer conn.close()
	return probeService(ctx, conn, srv, 1*time.Millisecond, true)
}

func probeService(ctx context.Context, conn MDNSConn, srv Service, delay time.Duration, probeOnce bool) (s Service, e error) {
	candidate := srv.Copy()
	prevConflict := probeConflict{}

	// Keep track of the number of conflicts
	numHostConflicts := 0
	numNameConflicts := 0

	for i := 1; i <= 100; i++ {
		conflict, err := probe(ctx, conn, *candidate)
		if err != nil {
			e = err
			return
		}

		if conflict.hasNone() {
			s = *candidate
			return
		}

		candidate = candidate.Copy()

		if conflict.hostname && (prevConflict.hostname || probeOnce) {
			numHostConflicts++
			candidate.Host = fmt.Sprintf("%s-%d", srv.Host, numHostConflicts+1)
			conflict.hostname = false
		}

		if conflict.serviceName && (prevConflict.serviceName || probeOnce) {
			numNameConflicts++
			candidate.Name = fmt.Sprintf("%s-%d", srv.Name, numNameConflicts+1)
			conflict.serviceName = false
		}

		prevConflict = conflict

		if conflict.hasAny() {
			// If the host finds that its own data is lexicographically earlier,
			// then it defers to the winning host by waiting one second,
			// and then begins probing for this record again. (RFC6762 8.2)
			log.Debug.Println("Increase wait time after receiving conflicting data")
			delay = 1 * time.Second
		} else {
			delay = 250 * time.Millisecond
		}

		log.Debug.Println("Probing wait", delay)
		time.Sleep(delay)
	}

	return
}

func probe(ctx context.Context, conn MDNSConn, service Service) (conflict probeConflict, err error) {
	for ifname, ips := range service.IfaceIPs {
		iface, err := net.InterfaceByName(ifname)
		if err != nil {
			log.Debug.Printf("error getting interface with name %s: %v\n", ifname, err)
			continue
		}
		log.Debug.Printf("Probing with %v at %s\n", ips, iface.Name)

		conflict, err := probeAtInterface(ctx, conn, service, iface)
		if conflict.hasAny() {
			return conflict, err
		}
	}

	return probeConflict{}, nil
}

func probeAtInterface(ctx context.Context, conn MDNSConn, service Service, iface *net.Interface) (conflict probeConflict, err error) {

	msg := new(dns.Msg)

	instanceQ := dns.Question{
		Name:   service.ServiceInstanceName(),
		Qtype:  dns.TypeANY,
		Qclass: dns.ClassINET,
	}

	hostQ := dns.Question{
		Name:   service.Hostname(),
		Qtype:  dns.TypeANY,
		Qclass: dns.ClassINET,
	}

// Match fix for https://github.com/brutella/dnssd/issues/15 
//	// Responses to probe should be unicast
//	setQuestionUnicast(&instanceQ)
//	setQuestionUnicast(&hostQ)

	msg.Question = []dns.Question{instanceQ, hostQ}

	srv := SRV(service)
	as := A(service, iface)
	aaaas := AAAA(service, iface)

	var authority = []dns.RR{srv}
	for _, a := range as {
		authority = append(authority, a)
	}
	for _, aaaa := range aaaas {
		authority = append(authority, aaaa)
	}
	msg.Ns = authority

	readCtx, readCancel := context.WithCancel(ctx)
	defer readCancel()

	// Multicast DNS responses received *before* the first probe packet is sent
	// MUST be silently ignored. (RFC6762 8.1)
	conn.Drain(readCtx)
	ch := conn.Read(readCtx)

	queryTime := time.After(1 * time.Millisecond)
	queriesCount := 1

	for {
		select {
		case req := <-ch:
			answers := allRecords(req.msg)
			for _, answer := range answers {
				switch rr := answer.(type) {
				case *dns.A:
					for _, a := range as {
						if isDenyingA(rr, a) {
							/*
							fmt.Printf("DENIES A req: %#v\n",req)
							if req.from == nil {
								fmt.Printf("DENIES A req.from NIL\n")
							}
							if req.iface == nil {
								fmt.Printf("DENIES A req.iface NIL\n")
							}
							log.Debug.Printf("%v:%d@%s denies A\n", req.from.IP, req.from.Port, req.iface.Name)
							*/
							conflict.hostname = true
							break
						}
					}

				case *dns.AAAA:
					for _, aaaa := range aaaas {
						if isDenyingAAAA(rr, aaaa) {
							/*
							fmt.Printf("DENIES AAAA req: %#v\n",req)
							if req.from == nil {
								fmt.Printf("DENIES AAAA req.from NIL\n")
							}
							if req.iface == nil {
								fmt.Printf("DENIES AAAA req.iface NIL\n")
							}
							log.Debug.Printf("%v:%d@%s denies AAAA\n", req.from.IP, req.from.Port, req.iface.Name)
							*/
							conflict.hostname = true
							break
						}
					}

				case *dns.SRV:
					if isDenyingSRV(rr, srv) {
						conflict.serviceName = true
					}

				default:
					break
				}
			}

		case <-ctx.Done():
			err = ctx.Err()
			return

		case <-queryTime:
			// Stop on conflict
			if conflict.hasAny() {
				return
			}

			// Stop after 3 probe queries
			if queriesCount > 3 {
				return
			}

			queriesCount++
			log.Debug.Println("Sending probe", msg)
			q := &Query{msg: msg, iface: iface}
			conn.SendQuery(q)

			delay := 250 * time.Millisecond
			log.Debug.Println("Waiting for conflicting data", delay)
			queryTime = time.After(delay)
		}
	}

	return
}

type probeConflict struct {
	hostname    bool
	serviceName bool
}

func (pr probeConflict) hasNone() bool {
	return !pr.hostname && !pr.serviceName
}

func (pr probeConflict) hasAny() bool {
	return pr.hostname || pr.serviceName
}

func isDenyingA(this *dns.A, that *dns.A) bool {
	if strings.EqualFold(this.Hdr.Name, that.Hdr.Name) {
		log.Debug.Println("Conflicting hosts")

		if !isValidRR(this) {
			log.Debug.Println("Invalid record produces conflict")
			return true
		}

		switch compareIP(this.A.To4(), that.A.To4()) {
		case -1:
			log.Debug.Println("Lexicographical earlier")
			break
		case 1:
			log.Debug.Println("Lexicographical later")
			return true
		default:
			log.Debug.Println("Tiebreak")
			break
		}
	}

	return false
}

// isDenyingAAAA returns true if this denies that.
func isDenyingAAAA(this *dns.AAAA, that *dns.AAAA) bool {
	if strings.EqualFold(this.Hdr.Name, that.Hdr.Name) {
		log.Debug.Println("Conflicting hosts")
		if !isValidRR(this) {
			log.Debug.Println("Invalid record produces conflict")
			return true
		}

		switch compareIP(this.AAAA.To16(), that.AAAA.To16()) {
		case -1:
			log.Debug.Println("Lexicographical earlier")
			break
		case 1:
			log.Debug.Println("Lexicographical later")
			return true
		default:
			log.Debug.Println("Tiebreak")
			break
		}
	}

	return false
}

// isDenyingSRV returns true if this denies that.
func isDenyingSRV(this *dns.SRV, that *dns.SRV) bool {
	if strings.EqualFold(this.Hdr.Name, that.Hdr.Name) {
		log.Debug.Println("Conflicting SRV")
		if !isValidRR(this) {
			log.Debug.Println("Invalid record produces conflict")
			return true
		}

		switch compareSRV(this, that) {
		case -1:
			log.Debug.Println("Lexicographical earlier")
			break
		case 1:
			log.Debug.Println("Lexicographical later")
			return true
		default:
			log.Debug.Println("Tiebreak")
			break
		}
	}

	return false
}

func isValidRR(rr dns.RR) bool {
	switch r := rr.(type) {
	case *dns.A:
		return !net.IPv4zero.Equal(r.A)
	case *dns.AAAA:
		return !net.IPv6zero.Equal(r.AAAA)
	case *dns.SRV:
		return len(r.Target) > 0 && r.Port != 0
	default:
		break
	}

	return true
}

func compareIP(this net.IP, that net.IP) int {
	count := len(this)
	if count > len(that) {
		count = len(that)
	}

	for i := 0; i < count; i++ {
		if this[i] < that[i] {
			return -1
		} else if this[i] > that[i] {
			return 1
		}
	}

	if len(this) < len(that) {
		return -1
	} else if len(this) > len(that) {
		return 1
	}
	return 0
}

func compareSRV(this *dns.SRV, that *dns.SRV) int {
	if this.Priority < that.Priority {
		return -1
	} else if this.Priority > that.Priority {
		return 1
	}

	if this.Weight < that.Weight {
		return -1
	} else if this.Weight > that.Weight {
		return 1
	}

	if this.Port < that.Port {
		return -1
	} else if this.Port > that.Port {
		return 1
	}

	return strings.Compare(this.Target, that.Target)
}
