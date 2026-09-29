package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cloudsoda/go-smb2"
	"golang.org/x/net/dns/dnsmessage"
)

// Server is an SMB server found on the local network.
type Server struct {
	Name    string   `json:"name"`
	Address string   `json:"address"`
	Via     []string `json:"via"`
}

// ShareInfo is a disk share offered by a server.
type ShareInfo struct {
	Name    string `json:"name"`
	Comment string `json:"comment"`
}

// discoverServers finds SMB servers with Bonjour, WS-Discovery (Windows) and a scan of the local /24 for port 445.
func discoverServers(ctx context.Context) []Server {
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		found = map[string]*Server{}
	)
	add := func(ip net.IP, name, via string) {
		mu.Lock()
		defer mu.Unlock()
		key := ip.String()
		s := found[key]
		if s == nil {
			s = &Server{Address: key}
			found[key] = s
		}
		if name != "" && s.Name == "" {
			s.Name = name
		}
		for _, v := range s.Via {
			if v == via {
				return
			}
		}
		s.Via = append(s.Via, via)
	}
	wg.Add(3)
	go func() { defer wg.Done(); browseMDNS(ctx, 2500*time.Millisecond, add) }()
	go func() { defer wg.Done(); probeWSD(ctx, 2500*time.Millisecond, add) }()
	go func() { defer wg.Done(); sweepSMB(ctx, add) }()
	wg.Wait()

	var servers []Server
	var names sync.WaitGroup
	for _, s := range found {
		if !hasVia(s, "port") && !portOpen(s.Address, 445, 600*time.Millisecond) {
			continue
		}
		servers = append(servers, *s)
	}
	for i := range servers {
		if servers[i].Name != "" {
			continue
		}
		names.Add(1)
		go func(s *Server) {
			defer names.Done()
			if name := netbiosName(net.ParseIP(s.Address), 700*time.Millisecond); name != "" {
				s.Name = name
			} else if hosts, err := net.DefaultResolver.LookupAddr(ctx, s.Address); err == nil && len(hosts) > 0 {
				s.Name = strings.TrimSuffix(strings.TrimSuffix(hosts[0], "."), ".local")
			}
		}(&servers[i])
	}
	names.Wait()
	sort.Slice(servers, func(i, j int) bool {
		if (servers[i].Name == "") != (servers[j].Name == "") {
			return servers[i].Name != ""
		}
		return strings.ToLower(servers[i].Name+servers[i].Address) < strings.ToLower(servers[j].Name+servers[j].Address)
	})
	return servers
}

// hasVia reports whether a server was found by the given method.
func hasVia(s *Server, via string) bool {
	for _, v := range s.Via {
		if v == via {
			return true
		}
	}
	return false
}

// portOpen reports whether a TCP port accepts connections.
func portOpen(host string, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// mdnsAnswers gathers what Bonjour responders said about _smb._tcp services.
type mdnsAnswers struct {
	names     map[string]string
	targets   map[string]string
	hosts     map[string]net.IP
	responder map[string]net.IP
}

// browseMDNS asks Bonjour responders (NAS boxes, Macs, Samba with Avahi) for _smb._tcp services.
func browseMDNS(ctx context.Context, wait time.Duration, add func(net.IP, string, string)) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return
	}
	defer conn.Close()
	query := func(name string, qtype dnsmessage.Type) {
		b := dnsmessage.NewBuilder(nil, dnsmessage.Header{})
		_ = b.StartQuestions()
		// The top class bit asks responders to answer this port directly instead of the multicast group.
		_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: qtype, Class: dnsmessage.ClassINET | 1<<15})
		if msg, err := b.Finish(); err == nil {
			_, _ = conn.WriteToUDP(msg, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353})
		}
	}
	query("_smb._tcp.local.", dnsmessage.TypePTR)
	answers := &mdnsAnswers{names: map[string]string{}, targets: map[string]string{}, hosts: map[string]net.IP{}, responder: map[string]net.IP{}}
	deadline := time.Now().Add(wait)
	followedUp := false
	buf := make([]byte, 9000)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if !followedUp && time.Until(deadline) < wait/2 {
			followedUp = true
			for instance := range answers.names {
				if _, ok := answers.targets[instance]; !ok {
					query(instance, dnsmessage.TypeSRV)
				}
			}
		}
		_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		var p dnsmessage.Parser
		if _, err := p.Start(buf[:n]); err != nil || p.SkipAllQuestions() != nil {
			continue
		}
		answers.read(&p, from.IP)
	}
	for instance, name := range answers.names {
		target := answers.targets[instance]
		ip := answers.hosts[strings.ToLower(target)]
		if ip == nil {
			ip = answers.responder[instance]
		}
		if ip == nil && target != "" {
			if addrs, err := net.DefaultResolver.LookupIP(ctx, "ip4", strings.TrimSuffix(target, ".")); err == nil && len(addrs) > 0 {
				ip = addrs[0]
			}
		}
		if ip != nil {
			add(ip, name, "bonjour")
		}
	}
}

// read collects PTR, SRV and A records from every section of one mDNS answer.
func (a *mdnsAnswers) read(p *dnsmessage.Parser, from net.IP) {
	sections := []func() (dnsmessage.ResourceHeader, error){p.AnswerHeader, p.AuthorityHeader, p.AdditionalHeader}
	for _, next := range sections {
		for {
			h, err := next()
			if err != nil {
				break
			}
			owner := h.Name.String()
			switch h.Type {
			case dnsmessage.TypePTR:
				r, err := p.PTRResource()
				if err == nil && strings.HasSuffix(strings.ToLower(owner), "_smb._tcp.local.") {
					instance := r.PTR.String()
					a.names[instance] = unescapeDNS(strings.TrimSuffix(instance, "._smb._tcp.local."))
					a.responder[instance] = from
				}
			case dnsmessage.TypeSRV:
				if r, err := p.SRVResource(); err == nil && strings.HasSuffix(strings.ToLower(owner), "_smb._tcp.local.") {
					a.targets[owner] = r.Target.String()
					a.responder[owner] = from
					if _, ok := a.names[owner]; !ok {
						a.names[owner] = unescapeDNS(strings.TrimSuffix(owner, "._smb._tcp.local."))
					}
				}
			case dnsmessage.TypeA:
				if r, err := p.AResource(); err == nil {
					a.hosts[strings.ToLower(owner)] = net.IP(r.A[:])
				}
			default:
				if err := p.SkipAnswer(); err != nil {
					return
				}
			}
		}
	}
}

// unescapeDNS turns DNS-SD escapes such as "\032" back into characters.
func unescapeDNS(label string) string {
	var out strings.Builder
	for i := 0; i < len(label); i++ {
		if label[i] != '\\' || i+1 >= len(label) {
			out.WriteByte(label[i])
			continue
		}
		if i+3 < len(label) && isDigit(label[i+1]) && isDigit(label[i+2]) && isDigit(label[i+3]) {
			out.WriteByte(byte(int(label[i+1]-'0')*100 + int(label[i+2]-'0')*10 + int(label[i+3]-'0')))
			i += 3
		} else {
			out.WriteByte(label[i+1])
			i++
		}
	}
	return out.String()
}

// isDigit reports whether a byte is an ASCII digit.
func isDigit(c byte) bool {
	return c >= '0' && c <= '9'
}

// probeWSD sends a WS-Discovery probe, which Windows PCs with network discovery on answer.
func probeWSD(ctx context.Context, wait time.Duration, add func(net.IP, string, string)) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return
	}
	defer conn.Close()
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	uuid := hex.EncodeToString(id)
	uuid = uuid[:8] + "-" + uuid[8:12] + "-" + uuid[12:16] + "-" + uuid[16:20] + "-" + uuid[20:]
	probe := `<?xml version="1.0" encoding="utf-8"?>` +
		`<soap:Envelope xmlns:soap="http://www.w3.org/2003/05/soap-envelope" xmlns:wsa="http://schemas.xmlsoap.org/ws/2004/08/addressing" ` +
		`xmlns:wsd="http://schemas.xmlsoap.org/ws/2005/04/discovery" xmlns:wsdp="http://schemas.xmlsoap.org/ws/2006/02/devprof">` +
		`<soap:Header><wsa:To>urn:schemas-xmlsoap-org:ws:2005:04:discovery</wsa:To>` +
		`<wsa:Action>http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</wsa:Action>` +
		`<wsa:MessageID>urn:uuid:` + uuid + `</wsa:MessageID></soap:Header>` +
		`<soap:Body><wsd:Probe><wsd:Types>wsdp:Device</wsd:Types></wsd:Probe></soap:Body></soap:Envelope>`
	target := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 3702}
	_, _ = conn.WriteToUDP([]byte(probe), target)
	deadline := time.Now().Add(wait)
	buf := make([]byte, 16384)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		if strings.Contains(string(buf[:n]), "ProbeMatch") {
			add(from.IP, "", "windows")
		}
	}
}

// sweepSMB tries port 445 on every address of the local /24 networks this machine is on.
func sweepSMB(ctx context.Context, add func(net.IP, string, string)) {
	var targets []net.IP
	seen := map[string]bool{}
	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil || !ipnet.IP.IsPrivate() {
				continue
			}
			base := ipnet.IP.To4().Mask(net.CIDRMask(24, 32))
			if seen[base.String()] {
				continue
			}
			seen[base.String()] = true
			for i := 1; i < 255; i++ {
				targets = append(targets, net.IPv4(base[0], base[1], base[2], byte(i)))
			}
		}
	}
	gate := make(chan struct{}, 128)
	var wg sync.WaitGroup
	for _, ip := range targets {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		gate <- struct{}{}
		go func(ip net.IP) {
			defer wg.Done()
			defer func() { <-gate }()
			if portOpen(ip.String(), 445, 400*time.Millisecond) {
				add(ip, "", "port")
			}
		}(ip)
	}
	wg.Wait()
}

// netbiosName asks a host for its NetBIOS name, which Windows and Samba servers answer with.
func netbiosName(ip net.IP, timeout time.Duration) string {
	if ip == nil {
		return ""
	}
	conn, err := net.DialTimeout("udp4", net.JoinHostPort(ip.String(), "137"), timeout)
	if err != nil {
		return ""
	}
	defer conn.Close()
	packet := []byte{0x13, 0x37, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0x20}
	name := append([]byte("*"), make([]byte, 15)...)
	for _, c := range name {
		packet = append(packet, 'A'+c>>4, 'A'+c&0x0f)
	}
	packet = append(packet, 0, 0, 0x21, 0, 1)
	_ = conn.SetDeadline(time.Now().Add(timeout))
	if _, err := conn.Write(packet); err != nil {
		return ""
	}
	buf := make([]byte, 2048)
	n, err := conn.Read(buf)
	if err != nil || n < 57 {
		return ""
	}
	count := int(buf[56])
	fallback := ""
	for i := 0; i < count; i++ {
		off := 57 + i*18
		if off+18 > n {
			break
		}
		entry := strings.TrimRight(string(buf[off:off+15]), " \x00")
		suffix, group := buf[off+15], buf[off+16]&0x80 != 0
		if group || entry == "" {
			continue
		}
		if suffix == 0x20 {
			return entry
		}
		if suffix == 0x00 && fallback == "" {
			fallback = entry
		}
	}
	return fallback
}

// listShares logs in to a server and lists the disk shares a user can pick.
func listShares(host, user, password, domain string) ([]ShareInfo, error) {
	session, err := dialSMB(host, user, password, domain)
	if err != nil {
		return nil, err
	}
	defer closeSMB(session)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	infos, err := session.WithContext(ctx).ListShares()
	if err != nil {
		return nil, err
	}
	var shares []ShareInfo
	for _, info := range infos {
		if info.Type() != smb2.ShareTypeDiskTree || info.IsSpecial() || strings.HasSuffix(info.Name, "$") {
			continue
		}
		shares = append(shares, ShareInfo{Name: info.Name, Comment: info.Comment})
	}
	sort.Slice(shares, func(i, j int) bool { return strings.ToLower(shares[i].Name) < strings.ToLower(shares[j].Name) })
	return shares, nil
}

// listShareDirs lists the folders inside one folder of a share, for picking where videos live.
func listShareDirs(host, user, password, domain, share, dir string) ([]string, error) {
	session, err := dialSMB(host, user, password, domain)
	if err != nil {
		return nil, err
	}
	defer closeSMB(session)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fs, err := session.WithContext(ctx).Mount(share)
	if err != nil {
		return nil, err
	}
	entries, err := fs.WithContext(ctx).ReadDir(strings.ReplaceAll(strings.Trim(dir, `/\`), "/", `\`))
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() && !skipName(entry.Name()) {
			dirs = append(dirs, entry.Name())
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i]) < strings.ToLower(dirs[j]) })
	return dirs, nil
}
