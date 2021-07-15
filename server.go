package socks5

import (
	"errors"
	"io"
	"log"
	"net"
	"strconv"
	"sync"
	"time"
)

// Server defines parameters for running socks server.
// The zero value for Server is a valid configuration(tcp listen on :1080).
type Server struct {
	// Addr optionally specifies the TCP address for the server to listen on,
	// in the form "host:port". If empty, ":1080" (port 1080) is used.
	Addr string

	// ReadTimeout is the maximum duration for reading from socks client.
	// it's only effective to socks server handshake process.
	//
	// If zero, there is no timeout.
	ReadTimeout time.Duration

	// WriteTimeout is the maximum duration for writing to socks client.
	// it's only effective to socks server handshake process.
	//
	// If zero, there is no timeout.
	WriteTimeout time.Duration

	// method mapping to the authenticator
	// if nil server provide NO_AUTHENTICATION_REQUIRED method by default
	Authenticators map[METHOD]Authenticator

	// The server select method to use policy
	//MethodSelector

	// Server transmit data between client and dest server.
	// if nil, DefaultTransport is used.
	Transporter

	// ErrorLog specifics an options logger for errors accepting
	// connections, unexpected socks protocol handshake process,
	// and server to remote connection errors.
	// If nil, logging is done via log package's standard logger.
	ErrorLog *log.Logger

	// DisableSocks4, disable socks4 server, default enable socks4 compatible.
	DisableSocks4 bool

	// Generate by Server.Addr field. For Server internal use only.
	bindAddr *Address

	isShutdown bool

	mu         sync.Mutex
	listeners  map[*net.Listener]struct{}
	activeConn map[*net.Conn]struct{}
	doneCh     chan struct{}
}

func (srv *Server) Close() error {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	// close all listeners
	err := srv.closeListenerLocked()
	if err != nil {
		return err
	}

	// todo close active conn
	return nil
}

func (srv *Server) Shutdown() error {
	// todo first close all listeners.
	srv.mu.Lock()
	defer srv.mu.Unlock()
	// todo second
	return nil
}

func (srv *Server) closeListenerLocked() error {
	var err error
	for ln := range srv.listeners {
		if cerr := (*ln).Close(); cerr != nil {
			err = cerr
		}
	}
	return err
}

func (srv *Server) trackListener(l *net.Listener, add bool) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.listeners == nil {
		srv.listeners = make(map[*net.Listener]struct{})
	}

	if add {
		srv.listeners[l] = struct{}{}
	} else {
		delete(srv.listeners, l)
	}
}

// ListenAndServe listens on the TCP network address srv.Addr and then
// calls serve to handle requests on incoming connections.
//
// If srv.Addr is blank, ":1080" is used.
func (srv *Server) ListenAndServe() error {
	if srv.bindAddr == nil {
		srv.bindAddr = new(Address)
	}
	addr := srv.Addr
	if addr == "" {
		addr = "0.0.0.0:1080"
	}

	address, err := ParseAddress(addr)
	if err != nil {
		return err
	}
	srv.bindAddr = address

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return srv.Serve(ln)
}

// Serve accepts incoming connections on the Listener l, creating a
// new service goroutine for each. The service goroutine select client
// list methods and reply client. Then process authentication and reply
// to them. At then end of handshake, read socks request from client and
// establish a connection to the target.
func (srv *Server) Serve(l net.Listener) error {
	srv.trackListener(&l, true)
	defer srv.trackListener(&l, false)
	for {
		client, err := l.Accept()
		if err != nil {
			select {}
			return err
		}
		go srv.serveconn(client)
	}
}

func (srv *Server) serveconn(client net.Conn) {
	if srv.ReadTimeout != 0 {
		client.SetReadDeadline(time.Now().Add(srv.ReadTimeout))
	}
	if srv.WriteTimeout != 0 {
		client.SetWriteDeadline(time.Now().Add(srv.WriteTimeout))
	}

	// handshake
	request, err := srv.handShake(client)
	if err != nil {
		srv.logf()(err.Error())
		client.Close()
		return
	}

	// establish connection to remote
	remote, err := srv.establish(client, request)
	if err != nil {
		srv.logf()(err.Error())
		client.Close()
		return
	}

	// establish over, reset deadline.
	client.SetReadDeadline(time.Time{})
	client.SetWriteDeadline(time.Time{})

	// transport data
	switch request.CMD {
	case CONNECT:
		srv.transport().TransportTCP(client, remote)
	case UDP_ASSOCIATE:
		udpServer := remote.(*net.UDPConn)
		srv.transport().TransportUDP(udpServer, request)
	case BIND:
		srv.logf()("not support bind command")
	}
}

func (srv *Server) transport() Transporter {
	if srv.Transporter == nil {
		return DefaultTransporter
	}
	return srv.Transporter
}

var errDisableSocks4 = errors.New("socks4 server has been disabled")

// handShake socks protocol handshake process
func (srv *Server) handShake(client net.Conn) (*Request, error) {
	//validate socks version message
	version, err := checkVersion(client)
	if err != nil {
		return nil, &OpError{Version5, "read", client.RemoteAddr(), "\"check version\"", err}
	}

	//socks4 protocol process
	if version == Version4 {
		if srv.DisableSocks4 {
			//send server reject reply
			address := &Address{net.IPv4zero, IPV4_ADDRESS, 0}
			addr, err := address.Bytes(Version4)
			if err != nil {
				return nil, &OpError{Version4, "", client.RemoteAddr(), "\"authentication\"", err}
			}
			_, err = client.Write(append([]byte{0, Rejected}, addr...))
			if err != nil {
				return nil, &OpError{Version4, "write", client.RemoteAddr(), "\"authentication\"", err}
			}
			return nil, errDisableSocks4
		}

		//handle socks4 request
		return srv.readSocks4Request(client)
	}

	//socks5 protocol authentication
	err = srv.authentication(client)
	if err != nil {
		return nil, err
	}

	//handle socks5 request
	return srv.readSocks5Request(client)
}

// authentication socks5 authentication process
func (srv *Server) authentication(client net.Conn) error {
	//get nMethods
	nMethods, err := ReadNBytes(client, 1)
	if err != nil {
		return err
	}

	//Get methods
	methods, err := ReadNBytes(client, int(nMethods[0]))
	if err != nil {
		return err
	}

	return srv.MethodSelect(methods, client)
}

// readSocks4Request receive socks4 protocol client request.
func (srv *Server) readSocks4Request(client net.Conn) (*Request, error) {
	reply := &Reply{
		VER:     Version4,
		Address: &Address{net.IPv4zero, IPV4_ADDRESS, 0},
	}
	req := &Request{
		VER: Version4,
	}
	// CMD
	cmd, err := ReadNBytes(client, 1)
	if err != nil {
		return nil, &OpError{req.VER, "read", client.RemoteAddr(), "\"process request command\"", err}
	}
	req.CMD = cmd[0]
	// DST.PORT, DST.IP
	addr, rep, err := readAddress(client, req.VER)
	if err != nil {
		reply.REP = rep
		err := srv.sendReply(client, reply)
		if err != nil {
			return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request address type\"", err}
		}
	}
	req.Address = addr
	return req, nil
}

// readSocks5Request read socks5 protocol client request.
func (srv *Server) readSocks5Request(client net.Conn) (*Request, error) {
	reply := &Reply{
		VER:     Version5,
		Address: &Address{net.IPv4zero, IPV4_ADDRESS, 0},
	}
	req := &Request{}
	//VER, CMD, RSV
	cmd, err := ReadNBytes(client, 3)
	if err != nil {
		return nil, &OpError{req.VER, "read", client.RemoteAddr(), "\"process request ver,cmd,rsv\"", err}
	}
	req.VER = cmd[0]
	req.CMD = cmd[1]
	req.RSV = cmd[2]
	// ATYPE, DST.IP, DST.PORT
	addr, rep, err := readAddress(client, req.VER)
	if err != nil {
		reply.REP = rep
		err := srv.sendReply(client, reply)
		if err != nil {
			return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request address\"", err}
		}
	}
	req.Address = addr

	return req, nil
}

// IsAllowNoAuthRequired  return true if server enable NO_AUTHENTICATION_REQUIRED.
// Or the server doesn't has no Authenticator return true. Otherwise return false.
func (srv *Server) IsAllowNoAuthRequired() bool {
	if len(srv.Authenticators) == 0 {
		return true
	}
	for method := range srv.Authenticators {
		if method == NO_AUTHENTICATION_REQUIRED {
			return true
		}
	}
	return false
}

// establish tcp connection to remote host if command is CONNECT or
// start listen on udp socket when command is UDP_ASSOCIATE. Listen
// and accept host connection when command is BIND. Finally, send
// corresponding reply to client.
func (srv *Server) establish(client net.Conn, req *Request) (dest net.Conn, err error) {
	reply := &Reply{
		VER:     req.VER,
		Address: &Address{net.IPv4zero, IPV4_ADDRESS, 0},
	}

	// version4
	if req.VER == Version4 {
		switch req.CMD {
		case CONNECT:
			// dial to dest host.
			dest, err = net.Dial("tcp", req.Address.String())
			if err != nil {
				reply.REP = Rejected
				err = srv.sendReply(client, reply)
				if err != nil {
					return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
				}
				return nil, err
			}

			// parse remote host address.
			remoteAddr, err := ParseAddress(dest.RemoteAddr().String())
			if err != nil {
				reply.REP = Rejected
				err = srv.sendReply(client, reply)
				if err != nil {
					return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
				}
				return nil, err
			}
			reply.Address = remoteAddr

			// success
			reply.REP = Granted
			err = srv.sendReply(client, reply)
			if err != nil {
				return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
			}
		case BIND:
			bindAddr, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(srv.bindAddr.Addr.String(), "0"))
			if err != nil {
				return nil, err
			}

			// start listening on random port.
			bindServer, err := net.ListenTCP("tcp", bindAddr)
			if err != nil {
				reply.REP = Rejected
				err = srv.sendReply(client, reply)
				if err != nil {
					return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
				}
				return nil, err
			}
			defer bindServer.Close()
			reply.REP = Granted
			reply.Address, err = ParseAddress(bindServer.Addr().String())
			if err != nil {
				return nil, err
			}

			// send first reply to client.
			err = srv.sendReply(client, reply)
			if err != nil {
				return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
			}
			// waiting target host connect.
			dest, err = bindServer.Accept()
			if err != nil {
				reply.REP = Rejected
				err = srv.sendReply(client, reply)
				if err != nil {
					return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
				}
				return nil, err
			}

			// send second reply to client.
			if req.Address.String() == dest.RemoteAddr().String() {
				err = srv.sendReply(client, reply)
				if err != nil {
					return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
				}
			} else {
				reply.REP = Rejected
				err = srv.sendReply(client, reply)
				if err != nil {
					return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
				}
			}
		default:
			reply.REP = Rejected
			err = srv.sendReply(client, reply)
			if err != nil {
				return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
			}
			return nil, &OpError{req.VER, "", client.RemoteAddr(), "\"process request\"", &CMDError{req.CMD}}
		}
	} else if req.VER == Version5 { // version5
		switch req.CMD {
		case CONNECT:
			// dial dest host.
			dest, err = net.Dial("tcp", req.Address.String())
			if err != nil {
				reply.REP = HOST_UNREACHABLE
				err = srv.sendReply(client, reply)
				return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
				return nil, err
			}

			// parse remote host address.
			remoteAddr, err := ParseAddress(dest.RemoteAddr().String())
			if err != nil {
				reply.REP = GENERAL_SOCKS_SERVER_FAILURE
				err = srv.sendReply(client, reply)
				if err != nil {
					return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
				}
				return nil, err
			}
			reply.Address = remoteAddr

			// success
			reply.REP = SUCCESSED
			err = srv.sendReply(client, reply)
			if err != nil {
				return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
			}
		case UDP_ASSOCIATE:
			addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(srv.bindAddr.Addr.String(), "0"))
			if err != nil {
				return nil, err
			}

			// start udp listening on random port.
			dest, err = net.ListenUDP("udp", addr)
			if err != nil {
				reply.REP = GENERAL_SOCKS_SERVER_FAILURE
				err = srv.sendReply(client, reply)
				if err != nil {
					return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
				}
				return nil, err
			}

			// success
			reply.REP = SUCCESSED
			_, p, err := net.SplitHostPort(dest.LocalAddr().String())
			port, err := strconv.Atoi(p)
			reply.Address.Port = uint16(port)
			err = srv.sendReply(client, reply)
			if err != nil {
				return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
			}
		case BIND:
			bindAddr, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(srv.bindAddr.Addr.String(), "0"))
			if err != nil {
				return nil, err
			}

			// start tcp listening on random port.
			bindServer, err := net.ListenTCP("tcp", bindAddr)
			if err != nil {
				reply.REP = GENERAL_SOCKS_SERVER_FAILURE
				err = srv.sendReply(client, reply)
				if err != nil {
					return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
				}
				return nil, err
			}
			defer bindServer.Close()
			reply.REP = SUCCESSED
			reply.Address, err = ParseAddress(bindServer.Addr().String())
			if err != nil {
				return nil, err
			}

			// send first reply to client.
			err = srv.sendReply(client, reply)
			if err != nil {
				return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
			}
			dest, err = bindServer.Accept()
			if err != nil {
				reply.REP = GENERAL_SOCKS_SERVER_FAILURE
				err = srv.sendReply(client, reply)
				if err != nil {
					return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
				}
				return nil, err
			}

			// send second reply to client.
			if req.Address.String() == dest.RemoteAddr().String() {
				reply.Address, err = ParseAddress(dest.RemoteAddr().String())
				if err != nil {
					return nil, err
				}
				err = srv.sendReply(client, reply)
				if err != nil {
					return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
				}
			} else {
				reply.REP = GENERAL_SOCKS_SERVER_FAILURE
				err = srv.sendReply(client, reply)
				if err != nil {
					return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
				}
			}
		default:
			reply.REP = COMMAND_NOT_SUPPORTED
			err = srv.sendReply(client, reply)
			if err != nil {
				return nil, &OpError{req.VER, "write", client.RemoteAddr(), "\"process request\"", err}
			}

			return nil, &OpError{Version5, "", client.RemoteAddr(), "\"process request\"", &CMDError{req.CMD}}
		}
	} else { // unknown version
		return nil, &VersionError{req.VER}
	}
	return
}

var errErrorATPE = errors.New("socks4 server bind address type should be ipv4")

// sendReply The server send socks protocol reply to client
func (srv *Server) sendReply(out io.Writer, r *Reply) error {
	var reply []byte
	var err error

	if r.VER == Version4 {
		if r.Address.ATYPE != IPV4_ADDRESS {
			return errErrorATPE
		}
		addr, err := r.Address.Bytes(r.VER)
		if err != nil {
			return err
		}
		reply = append(reply, 0, r.REP)
		// Remove NULL at the end. Please see Address.Bytes() Method.
		reply = append(reply, addr[:len(addr)-1]...)
	} else if r.VER == Version5 {
		addr, err := r.Address.Bytes(r.VER)
		if err != nil {
			return err
		}
		reply = append(reply, r.VER, r.REP, r.RSV)
		reply = append(reply, addr...)
	} else {
		return &VersionError{r.VER}
	}

	_, err = out.Write(reply)
	return err
}

// MethodSelect select authentication method and reply to client.
func (srv *Server) MethodSelect(methods []CMD, client net.Conn) error {
	// Select method to authenticate, then send selected method to client.
	for _, method := range methods {
		//Preferred to use NO_AUTHENTICATION_REQUIRED method
		if method == NO_AUTHENTICATION_REQUIRED && srv.IsAllowNoAuthRequired() {
			reply := []byte{Version5, NO_AUTHENTICATION_REQUIRED}
			_, err := client.Write(reply)
			if err != nil {
				return &OpError{Version5, "write", client.RemoteAddr(), "authentication", err}
			}
			return nil
		}
		for m := range srv.Authenticators {
			//Select the first matched method to authenticate
			if m == method {
				reply := []byte{Version5, method}
				_, err := client.Write(reply)
				if err != nil {
					return &OpError{Version5, "write", client.RemoteAddr(), "authentication", err}
				}

				err = srv.Authenticators[m].Authenticate(client, client)
				if err != nil {
					return &OpError{Version5, "", client.RemoteAddr(), "authentication", err}
				}
				return nil
			}
		}
	}

	// There are no Methods can use
	reply := []byte{Version5, NO_ACCEPTABLE_METHODS}
	_, err := client.Write(reply)
	if err != nil {
		return &OpError{Version5, "write", client.RemoteAddr(), "authentication", err}
	}
	return &OpError{Version5, "", client.RemoteAddr(), "authentication", &MethodError{methods[0]}}
}

func (srv *Server) logf() func(format string, args ...interface{}) {
	if srv.ErrorLog == nil {
		return log.Printf
	}
	return srv.ErrorLog.Printf
}

// checkVersion check version is 4 or 5.
func checkVersion(in io.Reader) (VER, error) {
	version, err := ReadNBytes(in, 1)
	if err != nil {
		return 0, err
	}

	if (version[0] != Version5) && (version[0] != Version4) {
		return 0, &VersionError{version[0]}
	}
	return version[0], nil
}

// OpError is the error type usually returned by functions in the socks5
// package. It describes the socks version, operation, client address,
// and address of an error.
type OpError struct {
	// VER describe the socks server version on process.
	VER

	// Op is the operation which caused the error, such as
	// "read", "write".
	Op string

	// Addr define client's address which caused the error.
	Addr net.Addr

	// Step is the client's current connection stage, such as
	// "check version", "authentication", "process request",
	Step string

	// Err is the error that occurred during the operation.
	// The Error method panics if the error is nil.
	Err error
}

func (o *OpError) Error() string {
	str := "socks" + strconv.Itoa(int(o.VER))
	str += " " + o.Op
	if o.Addr == nil {
		str += " "
	} else {
		str += " " + o.Addr.String()
	}
	str += " " + o.Step
	str += ":" + o.Err.Error()
	return str
}
