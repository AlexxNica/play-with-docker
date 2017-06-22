package router

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	vhost "github.com/inconshreveable/go-vhost"
	"github.com/miekg/dns"
)

type Director func(host string) (*net.TCPAddr, error)

type proxyRouter struct {
	sync.Mutex

	keyPath      string
	director     Director
	closed       bool
	httpListener net.Listener
	udpDnsServer *dns.Server
	tcpDnsServer *dns.Server
	sshListener  net.Listener
	sshConfig    *ssh.ServerConfig
}

func (r *proxyRouter) Listen(httpAddr, dnsAddr, sshAddr string) {
	l, err := net.Listen("tcp", httpAddr)
	if err != nil {
		log.Fatal(err)
	}
	r.httpListener = l
	go func() {
		for !r.closed {
			conn, err := r.httpListener.Accept()
			if err != nil {
				continue
			}
			go r.handleConnection(conn)
		}
	}()

	dnsMux := dns.NewServeMux()
	dnsMux.HandleFunc(".", r.dnsRequest)
	r.udpDnsServer = &dns.Server{Addr: dnsAddr, Net: "udp", Handler: dnsMux}
	r.tcpDnsServer = &dns.Server{Addr: dnsAddr, Net: "tcp", Handler: dnsMux}

	wg := sync.WaitGroup{}
	wg.Add(2)

	r.udpDnsServer.NotifyStartedFunc = func() {
		wg.Done()
	}
	r.tcpDnsServer.NotifyStartedFunc = func() {
		wg.Done()
	}
	go r.udpDnsServer.ListenAndServe()
	go r.tcpDnsServer.ListenAndServe()
	wg.Wait()

	lssh, err := net.Listen("tcp", sshAddr)
	if err != nil {
		log.Fatal("failed to listen for connection: ", err)
	}
	r.sshListener = lssh
	go func() {
		for {
			nConn, err := lssh.Accept()
			if err != nil {
				log.Fatal("failed to accept incoming connection: ", err)
			}

			go r.sshHandle(nConn)
		}
	}()
}

func (r *proxyRouter) sshHandle(nConn net.Conn) {
	sshCon, chans, reqs, err := ssh.NewServerConn(nConn, r.sshConfig)
	if err != nil {
		nConn.Close()
		return
	}

	dstHost, err := r.director(sshCon.User())
	if err != nil {
		nConn.Close()
		return
	}

	// The incoming Request channel must be serviced.
	go ssh.DiscardRequests(reqs)

	newChannel := <-chans
	if newChannel == nil {
		sshCon.Close()
		return
	}

	if newChannel.ChannelType() != "session" {
		newChannel.Reject(ssh.UnknownChannelType, "unknown channel type")
		return
	}

	channel, requests, err := newChannel.Accept()
	if err != nil {
		log.Fatalf("Could not accept channel: %v", err)
	}

	stderr := channel.Stderr()

	fmt.Fprintf(stderr, "Connecting to %s\r\n", dstHost.String())

	clientConfig := &ssh.ClientConfig{
		User: "root",
		Auth: []ssh.AuthMethod{
			ssh.Password("root"),
		},
		HostKeyCallback: func(hostname string, remote net.Addr, key ssh.PublicKey) error {
			return nil
		},
	}

	client, err := ssh.Dial("tcp", dstHost.String(), clientConfig)
	if err != nil {
		fmt.Fprintf(stderr, "Connect failed: %v\r\n", err)
		channel.Close()
		return
	}

	go func() {
		for newChannel = range chans {
			if newChannel == nil {
				return
			}

			channel2, reqs2, err := client.OpenChannel(newChannel.ChannelType(), newChannel.ExtraData())
			if err != nil {
				x, ok := err.(*ssh.OpenChannelError)
				if ok {
					newChannel.Reject(x.Reason, x.Message)
				} else {
					newChannel.Reject(ssh.Prohibited, "remote server denied channel request")
				}
				continue
			}

			channel, reqs, err := newChannel.Accept()
			if err != nil {
				channel2.Close()
				continue
			}
			go proxySsh(reqs, reqs2, channel, channel2)
		}
	}()

	// Forward the session channel
	channel2, reqs2, err := client.OpenChannel("session", []byte{})
	if err != nil {
		fmt.Fprintf(stderr, "Remote session setup failed: %v\r\n", err)
		channel.Close()
		return
	}

	maskedReqs := make(chan *ssh.Request, 1)
	go func() {
		for req := range requests {
			if req.Type == "auth-agent-req@openssh.com" {
				continue
			}
			maskedReqs <- req
		}
	}()
	proxySsh(maskedReqs, reqs2, channel, channel2)
}

func (r *proxyRouter) dnsRequest(w dns.ResponseWriter, req *dns.Msg) {
	if len(req.Question) > 0 {
		question := req.Question[0].Name

		if question == "localhost." {
			log.Printf("Asked for [localhost.] returning automatically [127.0.0.1]\n")
			m := new(dns.Msg)
			m.SetReply(req)
			m.Authoritative = true
			m.RecursionAvailable = true
			a, err := dns.NewRR(fmt.Sprintf("%s 60 IN A 127.0.0.1", question))
			if err != nil {
				log.Fatal(err)
			}
			m.Answer = append(m.Answer, a)
			w.WriteMsg(m)
			return
		}

		dstHost, err := r.director(strings.TrimSuffix(question, "."))
		if err != nil {
			// Director couldn't resolve it, try to lookup in the system's DNS
			ips, err := net.LookupIP(question)
			if err != nil {
				// we have no information about this and we are not a recursive dns server, so we just fail so the client can fallback to the next dns server it has configured
				w.Close()
				// dns.HandleFailed(w, r)
				return
			}
			m := new(dns.Msg)
			m.SetReply(req)
			m.Authoritative = true
			m.RecursionAvailable = true
			for _, ip := range ips {
				ipv4 := ip.To4()
				if ipv4 == nil {
					a, err := dns.NewRR(fmt.Sprintf("%s 60 IN AAAA %s", question, ip.String()))
					if err != nil {
						log.Fatal(err)
					}
					m.Answer = append(m.Answer, a)
				} else {
					a, err := dns.NewRR(fmt.Sprintf("%s 60 IN A %s", question, ipv4.String()))
					if err != nil {
						log.Fatal(err)
					}
					m.Answer = append(m.Answer, a)
				}
			}
			w.WriteMsg(m)
			return
		}

		m := new(dns.Msg)
		m.SetReply(req)
		m.Authoritative = true
		m.RecursionAvailable = true
		a, err := dns.NewRR(fmt.Sprintf("%s 60 IN A %s", question, dstHost.IP))
		if err != nil {
			log.Println(err)
			w.Close()
			// dns.HandleFailed(w, r)
			return
		}
		m.Answer = append(m.Answer, a)
		w.WriteMsg(m)
		return
	}
}

func (r *proxyRouter) Close() {
	r.Lock()
	defer r.Unlock()

	if r.httpListener != nil {
		r.httpListener.Close()
	}
	if r.udpDnsServer != nil {
		r.udpDnsServer.Shutdown()
	}
	r.closed = true
}

func (r *proxyRouter) ListenHttpAddress() string {
	if r.httpListener != nil {
		return r.httpListener.Addr().String()
	}
	return ""
}

func (r *proxyRouter) ListenDnsUdpAddress() string {
	if r.udpDnsServer != nil && r.udpDnsServer.PacketConn != nil {
		return r.udpDnsServer.PacketConn.LocalAddr().String()
	}

	return ""
}
func (r *proxyRouter) ListenDnsTcpAddress() string {
	if r.tcpDnsServer != nil && r.tcpDnsServer.Listener != nil {
		return r.tcpDnsServer.Listener.Addr().String()
	}

	return ""
}

func (r *proxyRouter) ListenSshAddress() string {
	if r.sshListener != nil {
		return r.sshListener.Addr().String()
	}

	return ""
}

func (r *proxyRouter) handleConnection(c net.Conn) {
	defer c.Close()
	// first try tls
	vhostConn, err := vhost.TLS(c)

	if err == nil {
		// It is a TLS connection
		defer vhostConn.Close()
		host := vhostConn.ClientHelloMsg.ServerName
		dstHost, err := r.director(host)
		if err != nil {
			log.Printf("Error directing request: %v\n", err)
			return
		}
		d, err := net.Dial("tcp", dstHost.String())
		if err != nil {
			log.Printf("Error dialing backend %s: %v\n", dstHost.String(), err)
			return
		}

		proxyConn(vhostConn, d)
	} else {
		// it is not TLS
		// treat it as an http connection

		req, err := http.ReadRequest(bufio.NewReader(vhostConn))
		if err != nil {
			// It is not http neither. So just close the connection.
			return
		}
		dstHost, err := r.director(req.Host)
		if err != nil {
			log.Printf("Error directing request: %v\n", err)
			return
		}
		d, err := net.Dial("tcp", dstHost.String())
		if err != nil {
			log.Printf("Error dialing backend %s: %v\n", dstHost.String(), err)
			return
		}
		err = req.Write(d)
		if err != nil {
			log.Printf("Error requesting backend %s: %v\n", dstHost.String(), err)
			return
		}
		proxyConn(c, d)
	}
}

func proxySsh(reqs1, reqs2 <-chan *ssh.Request, channel1, channel2 ssh.Channel) {
	var closer sync.Once
	closeFunc := func() {
		channel1.Close()
		channel2.Close()
	}

	defer closer.Do(closeFunc)

	closerChan := make(chan bool, 1)

	go func() {
		io.Copy(channel1, channel2)
		closerChan <- true
	}()

	go func() {
		io.Copy(channel2, channel1)
		closerChan <- true
	}()

	for {
		select {
		case req := <-reqs1:
			if req == nil {
				return
			}
			b, err := channel2.SendRequest(req.Type, req.WantReply, req.Payload)
			if err != nil {
				return
			}
			req.Reply(b, nil)

		case req := <-reqs2:
			if req == nil {
				return
			}
			b, err := channel1.SendRequest(req.Type, req.WantReply, req.Payload)
			if err != nil {
				return
			}
			req.Reply(b, nil)
		case <-closerChan:
			return
		}
	}
}

func proxyConn(src, dst net.Conn) {
	errc := make(chan error, 2)
	cp := func(dst io.Writer, src io.Reader) {
		_, err := io.Copy(dst, src)
		errc <- err
	}
	go cp(src, dst)
	go cp(dst, src)
	<-errc
}

func NewRouter(director Director, keyPath string) *proxyRouter {
	var sshConfig = &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, pubKey ssh.PublicKey) (*ssh.Permissions, error) {
			return nil, nil
		},
	}
	privateBytes, err := ioutil.ReadFile(keyPath)
	if err != nil {
		log.Fatal("Failed to load private key: ", err)
	}

	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		log.Fatal("Failed to parse private key: ", err)
	}

	sshConfig.AddHostKey(private)

	return &proxyRouter{director: director, sshConfig: sshConfig}
}
