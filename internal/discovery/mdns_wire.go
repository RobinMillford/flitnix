package discovery

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	// MDNSServiceName is the standard DNS-SD service type for Flitnix peers.
	MDNSServiceName = "_flitnix._tcp.local."

	dnsTypeA    uint16 = 1
	dnsTypePTR  uint16 = 12
	dnsTypeTXT  uint16 = 16
	dnsTypeAAAA uint16 = 28
	dnsTypeSRV  uint16 = 33
	dnsClassIN  uint16 = 1

	maxDNSPacketBytes = 2048
	maxDNSNameJumps   = 16
)

var errMalformedDNS = errors.New("discovery: malformed mDNS packet")

// mdnsAnnouncement holds sanitized service metadata carried inside a DNS-SD packet.
type mdnsAnnouncement struct {
	HostID     string
	PublicKey  string
	DeviceName string
	OS         string
	Version    string
	Port       uint16
	IPs        []net.IP
	Sig        string
	Timestamp  int64
}

func encodeDNSName(name string) ([]byte, error) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(name), ".")
	if trimmed == "" {
		return []byte{0}, nil
	}
	labels := strings.Split(trimmed, ".")
	out := make([]byte, 0, len(trimmed)+2)
	for _, lbl := range labels {
		if len(lbl) == 0 || len(lbl) > 63 {
			return nil, fmt.Errorf("%w: invalid label %q", errMalformedDNS, lbl)
		}
		out = append(out, byte(len(lbl)))
		out = append(out, lbl...)
	}
	out = append(out, 0)
	return out, nil
}

func decodeDNSName(pkt []byte, offset int) (string, int, error) {
	if offset < 0 || offset >= len(pkt) {
		return "", 0, errMalformedDNS
	}
	var labels []string
	cur := offset
	nextOffset := -1
	jumped := false
	jumps := 0

	for {
		if cur < 0 || cur >= len(pkt) {
			return "", 0, errMalformedDNS
		}
		length := int(pkt[cur])
		if length == 0 {
			if !jumped {
				nextOffset = cur + 1
			}
			break
		}
		if (length & 0xC0) == 0xC0 {
			if cur+1 >= len(pkt) {
				return "", 0, errMalformedDNS
			}
			ptr := int(binary.BigEndian.Uint16(pkt[cur:cur+2]) & 0x3FFF)
			if ptr >= len(pkt) {
				return "", 0, errMalformedDNS
			}
			if !jumped {
				nextOffset = cur + 2
				jumped = true
			}
			jumps++
			if jumps > maxDNSNameJumps {
				return "", 0, errMalformedDNS
			}
			cur = ptr
			continue
		}
		if (length & 0xC0) != 0 {
			return "", 0, errMalformedDNS
		}
		cur++
		if cur+length > len(pkt) {
			return "", 0, errMalformedDNS
		}
		labels = append(labels, string(pkt[cur:cur+length]))
		cur += length
	}
	if nextOffset < 0 {
		return "", 0, errMalformedDNS
	}
	if len(labels) == 0 {
		return ".", nextOffset, nil
	}
	return strings.Join(labels, ".") + ".", nextOffset, nil
}

func appendDNSRR(buf []byte, name string, rrType uint16, ttl uint32, rdata []byte) ([]byte, error) {
	encName, err := encodeDNSName(name)
	if err != nil {
		return nil, err
	}
	buf = append(buf, encName...)
	var hdr [10]byte
	binary.BigEndian.PutUint16(hdr[0:2], rrType)
	binary.BigEndian.PutUint16(hdr[2:4], dnsClassIN)
	binary.BigEndian.PutUint32(hdr[4:8], ttl)
	binary.BigEndian.PutUint16(hdr[8:10], uint16(len(rdata)))
	buf = append(buf, hdr[:]...)
	buf = append(buf, rdata...)
	return buf, nil
}

// buildMDNSQuery builds a standard mDNS PTR query for _flitnix._tcp.local.
func buildMDNSQuery() ([]byte, error) {
	var buf []byte
	var hdr [12]byte
	// Standard DNS query header: QDCOUNT = 1
	binary.BigEndian.PutUint16(hdr[4:6], 1)
	buf = append(buf, hdr[:]...)

	qname, err := encodeDNSName(MDNSServiceName)
	if err != nil {
		return nil, err
	}
	buf = append(buf, qname...)
	var qtail [4]byte
	binary.BigEndian.PutUint16(qtail[0:2], dnsTypePTR)
	binary.BigEndian.PutUint16(qtail[2:4], dnsClassIN)
	buf = append(buf, qtail[:]...)
	return buf, nil
}

// buildMDNSResponse builds an unsolicited/solicited mDNS DNS-SD response with
// PTR, SRV, TXT, and A/AAAA records containing strictly sanitized metadata.
func buildMDNSResponse(a mdnsAnnouncement, ttlSeconds uint32) ([]byte, error) {
	if a.HostID == "" || a.Port == 0 {
		return nil, errors.New("discovery: incomplete announcement")
	}
	prefix := a.HostID
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	instanceName := "flitnix-" + prefix + "." + MDNSServiceName
	targetHost := "flitnix-" + prefix + ".local."

	var answers []byte
	var err error

	// 1. PTR: _flitnix._tcp.local. -> instanceName
	ptrData, err := encodeDNSName(instanceName)
	if err != nil {
		return nil, err
	}
	answers, err = appendDNSRR(answers, MDNSServiceName, dnsTypePTR, ttlSeconds, ptrData)
	if err != nil {
		return nil, err
	}

	// 2. SRV: instanceName -> port, targetHost
	targetData, err := encodeDNSName(targetHost)
	if err != nil {
		return nil, err
	}
	srvData := make([]byte, 6+len(targetData))
	binary.BigEndian.PutUint16(srvData[4:6], a.Port)
	copy(srvData[6:], targetData)
	answers, err = appendDNSRR(answers, instanceName, dnsTypeSRV, ttlSeconds, srvData)
	if err != nil {
		return nil, err
	}

	// 3. TXT: strictly sanitized metadata
	ipStrs := make([]string, 0, len(a.IPs))
	for _, ip := range a.IPs {
		if ip != nil {
			ipStrs = append(ipStrs, ip.String())
		}
	}
	txtPairs := []string{
		"hid=" + a.HostID,
		"pk=" + a.PublicKey,
		"name=" + a.DeviceName,
		"os=" + a.OS,
		"ver=" + a.Version,
		"ips=" + strings.Join(ipStrs, ","),
		"ts=" + strconv.FormatInt(a.Timestamp, 10),
		"sig=" + a.Sig,
	}
	var txtData []byte
	for _, entry := range txtPairs {
		if len(entry) > 255 {
			entry = entry[:255]
		}
		txtData = append(txtData, byte(len(entry)))
		txtData = append(txtData, entry...)
	}
	answers, err = appendDNSRR(answers, instanceName, dnsTypeTXT, ttlSeconds, txtData)
	if err != nil {
		return nil, err
	}

	// 4. A / AAAA records for bound local interface IPs
	rrCount := uint16(3)
	for _, ip := range a.IPs {
		if ip4 := ip.To4(); ip4 != nil {
			answers, err = appendDNSRR(answers, targetHost, dnsTypeA, ttlSeconds, ip4)
			if err != nil {
				return nil, err
			}
			rrCount++
		} else if ip16 := ip.To16(); ip16 != nil {
			answers, err = appendDNSRR(answers, targetHost, dnsTypeAAAA, ttlSeconds, ip16)
			if err != nil {
				return nil, err
			}
			rrCount++
		}
	}

	var hdr [12]byte
	binary.BigEndian.PutUint16(hdr[2:4], 0x8400) // standard response, authoritative
	binary.BigEndian.PutUint16(hdr[6:8], rrCount)
	pkt := make([]byte, 0, len(hdr)+len(answers))
	pkt = append(pkt, hdr[:]...)
	pkt = append(pkt, answers...)
	return pkt, nil
}

// isMDNSFlitnixQuery checks whether pkt is an mDNS query asking for _flitnix._tcp.local.
func isMDNSFlitnixQuery(pkt []byte) bool {
	if len(pkt) < 12 || len(pkt) > maxDNSPacketBytes {
		return false
	}
	flags := binary.BigEndian.Uint16(pkt[2:4])
	if (flags & 0x8000) != 0 {
		return false
	}
	qdCount := int(binary.BigEndian.Uint16(pkt[4:6]))
	if qdCount <= 0 || qdCount > 16 {
		return false
	}
	off := 12
	for i := 0; i < qdCount; i++ {
		name, next, err := decodeDNSName(pkt, off)
		if err != nil || next+4 > len(pkt) {
			return false
		}
		qType := binary.BigEndian.Uint16(pkt[next : next+2])
		if strings.EqualFold(name, MDNSServiceName) && (qType == dnsTypePTR || qType == 255) {
			return true
		}
		off = next + 4
	}
	return false
}

// parseMDNSResponse parses an mDNS response packet and returns sanitized announcement data if valid.
func parseMDNSResponse(pkt []byte) (mdnsAnnouncement, bool) {
	if len(pkt) < 12 || len(pkt) > maxDNSPacketBytes {
		return mdnsAnnouncement{}, false
	}
	flags := binary.BigEndian.Uint16(pkt[2:4])
	if (flags & 0x8000) == 0 {
		return mdnsAnnouncement{}, false
	}
	qdCount := int(binary.BigEndian.Uint16(pkt[4:6]))
	anCount := int(binary.BigEndian.Uint16(pkt[6:8]))
	nsCount := int(binary.BigEndian.Uint16(pkt[8:10]))
	arCount := int(binary.BigEndian.Uint16(pkt[10:12]))
	totalRR := anCount + nsCount + arCount
	if totalRR == 0 || totalRR > 64 || qdCount > 16 {
		return mdnsAnnouncement{}, false
	}

	off := 12
	for i := 0; i < qdCount; i++ {
		_, next, err := decodeDNSName(pkt, off)
		if err != nil || next+4 > len(pkt) {
			return mdnsAnnouncement{}, false
		}
		off = next + 4
	}

	var out mdnsAnnouncement
	var signedIPs []net.IP
	hasFlitnix := false

	for i := 0; i < totalRR; i++ {
		name, next, err := decodeDNSName(pkt, off)
		if err != nil || next+10 > len(pkt) {
			return mdnsAnnouncement{}, false
		}
		rrType := binary.BigEndian.Uint16(pkt[next : next+2])
		rdLen := int(binary.BigEndian.Uint16(pkt[next+8 : next+10]))
		rdataOff := next + 10
		if rdataOff+rdLen > len(pkt) {
			return mdnsAnnouncement{}, false
		}
		rdata := pkt[rdataOff : rdataOff+rdLen]

		switch rrType {
		case dnsTypePTR:
			if strings.EqualFold(name, MDNSServiceName) {
				target, _, ptrErr := decodeDNSName(pkt, rdataOff)
				if ptrErr == nil && strings.HasSuffix(strings.ToLower(target), "."+MDNSServiceName) {
					hasFlitnix = true
				}
			}
		case dnsTypeSRV:
			if strings.HasSuffix(strings.ToLower(name), "."+MDNSServiceName) && len(rdata) >= 6 {
				out.Port = binary.BigEndian.Uint16(rdata[4:6])
				hasFlitnix = true
			}
		case dnsTypeTXT:
			if strings.HasSuffix(strings.ToLower(name), "."+MDNSServiceName) {
				hasFlitnix = true
				pos := 0
				for pos < len(rdata) {
					slen := int(rdata[pos])
					pos++
					if pos+slen > len(rdata) {
						return mdnsAnnouncement{}, false
					}
					kv := string(rdata[pos : pos+slen])
					pos += slen
					if eq := strings.IndexByte(kv, '='); eq > 0 {
						k, v := kv[:eq], kv[eq+1:]
						switch k {
						case "hid":
							out.HostID = v
						case "pk":
							out.PublicKey = v
						case "name":
							out.DeviceName = v
						case "os":
							out.OS = v
						case "ver":
							out.Version = v
						case "ips":
							for _, rawIP := range strings.Split(v, ",") {
								if parsed := net.ParseIP(strings.TrimSpace(rawIP)); parsed != nil {
									signedIPs = append(signedIPs, parsed)
								}
							}
						case "ts":
							if parsed, err := strconv.ParseInt(v, 10, 64); err == nil {
								out.Timestamp = parsed
							}
						case "sig":
							out.Sig = v
						}
					}
				}
			}
		case dnsTypeA:
			if len(rdata) == 4 {
				ip := make(net.IP, 4)
				copy(ip, rdata)
				out.IPs = append(out.IPs, ip)
			}
		case dnsTypeAAAA:
			if len(rdata) == 16 {
				ip := make(net.IP, 16)
				copy(ip, rdata)
				out.IPs = append(out.IPs, ip)
			}
		}
		off = rdataOff + rdLen
	}

	if !hasFlitnix || out.HostID == "" || out.PublicKey == "" || out.Port == 0 {
		return mdnsAnnouncement{}, false
	}
	if len(signedIPs) > 0 {
		out.IPs = signedIPs
	}
	return out, true
}
