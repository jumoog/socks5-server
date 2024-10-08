package socks5

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/netip"

	"github.com/sirupsen/logrus"
)

const (
	socks5Version = uint8(5)
)

// Config is used to setup and configure a Server
type Config struct {
	// AuthMethods can be provided to implement custom authentication
	// By default, "auth-less" mode is enabled.
	// For password-based auth use UserPassAuthenticator.
	AuthMethods []Authenticator

	// If provided, username/password authentication is enabled,
	// by appending a UserPassAuthenticator to AuthMethods. If not provided,
	// and AUthMethods is nil, then "auth-less" mode is enabled.
	Credentials CredentialStore

	// Resolver can be provided to do custom name resolution.
	// Defaults to DNSResolver if not provided.
	Resolver NameResolver

	// Rules is provided to enable custom logic around permitting
	// various commands. If not provided, PermitAll is used.
	Rules RuleSet

	// Rewriter can be used to transparently rewrite addresses.
	// This is invoked before the RuleSet is invoked.
	// Defaults to NoRewrite.
	Rewriter AddressRewriter

	// BindIP is used for bind or udp associate
	BindIP netip.Addr

	// Logger can be used to provide a custom log target.
	// Defaults to stdout.
	Logger *logrus.Logger

	// Optional function for dialing out
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Server is reponsible for accepting connections and handling
// the details of the SOCKS5 protocol
type Server struct {
	config      *Config
	authMethods map[uint8]Authenticator
	isIPAllowed func(netip.Addr) bool
}

// New creates a new Server and potentially returns an error
func New(conf *Config) (*Server, error) {
	// Ensure we have at least one authentication method enabled
	if len(conf.AuthMethods) == 0 {
		if conf.Credentials != nil {
			conf.AuthMethods = []Authenticator{&UserPassAuthenticator{conf.Credentials}}
		} else {
			conf.AuthMethods = []Authenticator{&NoAuthAuthenticator{}}
		}
	}

	// Ensure we have a DNS resolver
	if conf.Resolver == nil {
		conf.Resolver = DNSResolver{}
	}

	// Ensure we have a rule set
	if conf.Rules == nil {
		conf.Rules = PermitAll()
	}

	// Ensure we have a log target
	if conf.Logger == nil {
		conf.Logger = logrus.StandardLogger()
	}

	server := &Server{
		config: conf,
	}

	server.authMethods = make(map[uint8]Authenticator)

	for _, a := range conf.AuthMethods {
		server.authMethods[a.GetCode()] = a
	}

	// Set default IP whitelist function
	server.isIPAllowed = func(ip netip.Addr) bool {
		return false // default block all IPs
	}

	return server, nil
}

// ListenAndServe is used to create a listener and serve on it
func (s *Server) ListenAndServe(network, addr string) error {
	l, err := net.Listen(network, addr)
	if err != nil {
		return err
	}
	return s.Serve(l)
}

// Serve is used to serve connections from a listener
func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		go s.ServeConn(conn)
	}
}

// SetIPWhitelist sets the function to check if a given IP is allowed
func (s *Server) SetIPWhitelist(allowedIPs []netip.Addr) {
	s.isIPAllowed = func(ip netip.Addr) bool {
		for _, allowedIP := range allowedIPs {
			if ip.Compare(allowedIP) == 0 {
				return true
			}
		}
		return false
	}
}

// ServeConn is used to serve a single connection.
func (s *Server) ServeConn(conn net.Conn) error {
	defer conn.Close()
	bufConn := bufio.NewReader(conn)

	// Check client IP against whitelist
	clientIP, _, err := net.SplitHostPort(conn.RemoteAddr().String())
	if err != nil {
		s.config.Logger.Errorf("failed to get client IP address: %v", err)
		return err
	}
	ip, _ := netip.ParseAddr(string(clientIP))
	if s.IsDockerNetwork(ip) {
		s.config.Logger.Infof("connection from Docker IP address: %s", clientIP)
	} else if s.IsTailScale(ip) {
		s.config.Logger.Infof("connection from Tailscale IP address: %s", clientIP)
	} else if s.isIPAllowed(ip) {
		s.config.Logger.Infof("connection from allowed address: %s", clientIP)
	} else {
		s.config.Logger.Warnf("connection from not allowed IP address: %s", clientIP)
		return fmt.Errorf("connection from not allowed IP address")
	}

	// Read the version byte
	version := []byte{0}
	if _, err := bufConn.Read(version); err != nil {
		s.config.Logger.Errorf("failed to get version byte: %v", err)
		return err
	}

	// Ensure we are compatible
	if version[0] != socks5Version {
		err := fmt.Errorf("unsupported SOCKS version: %v", version)
		s.config.Logger.Errorf("socks: %v", err)
		return err
	}

	// Authenticate the connection
	authContext, err := s.authenticate(conn, bufConn)
	if err != nil {
		err = fmt.Errorf("failed to authenticate: %v", err)
		s.config.Logger.Errorf("socks: %v", err)
		return err
	}

	request, err := NewRequest(bufConn)
	if err != nil {
		if err == ErrUnrecognizedAddrType {
			if err := sendReply(conn, addrTypeNotSupported, nil); err != nil {
				return fmt.Errorf("failed to send reply: %v", err)
			}
		}
		return fmt.Errorf("failed to read destination address: %v", err)
	}
	request.AuthContext = authContext
	if client, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		addr, _ := netip.ParseAddr(string(client.IP))
		request.RemoteAddr = &AddrSpec{IP: addr, Port: client.Port}
	}

	// Process the client request
	if err := s.handleRequest(request, conn); err != nil {
		err = fmt.Errorf("failed to handle request: %v", err)
		s.config.Logger.Errorf("socks: %v", err)
		return err
	}

	return nil
}

func (s *Server) IsDockerNetwork(ip netip.Addr) bool {
	if !ip.IsValid() || !ip.Is4() {
		return false
	}

	// Class B private range in CIDR notation: 172.16.0.0/12
	classBCIDR := netip.MustParsePrefix("172.16.0.0/12")

	return classBCIDR.Contains(ip)
}

func (s *Server) IsTailScale(ip netip.Addr) bool {
	if !ip.IsValid() || !ip.Is4() {
		return false
	}

	// CGNAT range in CIDR notation: 100.64.0.0/10
	cgnatCIDR := netip.MustParsePrefix("100.64.0.0/10")

	return cgnatCIDR.Contains(ip)
}
