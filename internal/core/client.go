// Copyright (c) 2019-2020 IrineSistiana
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS
// FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR
// COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER LIABILITY, WHETHER
// IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT OF OR IN
// CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

package core

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"
	"github.com/xtaci/smux"
)

const (
	muxCheckIdleInterval = time.Second * 2
	muxSessIdleTimeout   = time.Second * 10
)

//Client represents a client instance
type Client struct {
	conf *ClientConfig

	tlsConf   *tls.Config
	tcpConfig *tcpConfig

	wssURL   string
	wsDialer *websocket.Dialer

	netDialer *net.Dialer

	smuxSessPool smuxSessPool
	smuxConfig   *smux.Config

	listenerLocker sync.Mutex
	listener       net.Listener

	log *logrus.Logger

	//test only
	testDialServerRaw func() (net.Conn, error)
}

// NewClient inits a client instance
func NewClient(c *ClientConfig) (*Client, error) {
	client := new(Client)

	if len(c.BindAddr) == 0 {
		return nil, errors.New("need bind address")
	}

	if len(c.RemoteAddr) == 0 {
		return nil, errors.New("need remote server address")
	}

	if c.Timeout <= 0 {
		return nil, errors.New("timeout value must at least 1 sec")
	}

	if len(c.ServerName) == 0 { //set ServerName from RemoteAddr
		host, _, err := net.SplitHostPort(c.RemoteAddr)
		if err != nil {
			return nil, errors.New("cannot get the host address from the remote server address")
		}
		c.ServerName = host
	}

	if c.MuxMaxStream < 1 || c.MuxMaxStream > defaultSmuxMaxStream {
		return nil, fmt.Errorf("mux max stream should between 1 - 16")
	}

	//init

	//logger
	client.log = logrus.New()
	if c.Verbose {
		client.log.SetLevel(logrus.DebugLevel)
	} else {
		client.log.SetLevel(logrus.ErrorLevel)
	}

	//config
	client.tcpConfig = &tcpConfig{tfo: c.EnableTFO, vpnMode: c.VpnMode}
	client.tlsConf = &tls.Config{
		InsecureSkipVerify: c.InsecureSkipVerify,
		ServerName:         c.ServerName,
		ClientSessionCache: tls.NewLRUClientSessionCache(16),
	}

	//net dialer
	client.netDialer = &net.Dialer{
		Control: getControlFunc(client.tcpConfig),
		Timeout: defaultHandShakeTimeout,
	}

	//ws
	if !strings.HasPrefix(c.WSSPath, "/") {
		c.WSSPath = "/" + c.WSSPath
	}
	client.wssURL = "wss://" + c.ServerName + c.WSSPath
	internelDial := func(network, addr string) (net.Conn, error) {
		// overwrite url host addr
		return client.dialServerRaw()
	}
	client.wsDialer = &websocket.Dialer{
		TLSClientConfig: client.tlsConf,
		NetDial:         internelDial,

		ReadBufferSize:   defaultWSIOBufferSize,
		WriteBufferSize:  defaultWSIOBufferSize,
		WriteBufferPool:  &sync.Pool{},
		HandshakeTimeout: defaultHandShakeTimeout,
	}
	if c.EnableMux {
		client.wsDialer.Subprotocols = []string{websocketSubprotocolSmuxON}
	} else {
		client.wsDialer.Subprotocols = []string{websocketSubprotocolSmuxOFF}
	}

	// fallback dns
	if len(c.FallbackDNS) != 0 {
		//set fallback dns server
		if net.ParseIP(c.FallbackDNS) == nil { //it's not a IP addr
			return nil, fmt.Errorf("fallback dns server must be an IP addr, got %s", c.FallbackDNS)
		}

		//just overwrite net.DefaultResolver
		net.DefaultResolver.PreferGo = true
		net.DefaultResolver.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{}
			d.Control = getControlFunc(client.tcpConfig)
			return d.DialContext(ctx, "tcp", c.FallbackDNS)
		}
	}

	//smux pool
	client.smuxSessPool = smuxSessPool{}

	client.smuxConfig = defaultSmuxConfig()
	client.conf = c
	return client, nil
}

//Start starts the client, it block
func (client *Client) Start() error {
	listenConfig := net.ListenConfig{Control: getControlFunc(client.tcpConfig)}
	listener, err := listenConfig.Listen(context.Background(), "tcp", client.conf.BindAddr)
	if err != nil {
		return fmt.Errorf("net.Listen: %v", err)
	}
	defer listener.Close()
	client.listenerLocker.Lock()
	client.listener = listener
	client.listenerLocker.Unlock()
	client.log.Printf("plugin listen at %s", listener.Addr())

	for {
		leftConn, err := listener.Accept()
		if err != nil {
			return fmt.Errorf("listener.Accept: %v", err)
		}

		go func() {
			client.log.Debugf("client connection from %s accepted", leftConn.RemoteAddr())
			err := client.ForwardConn(leftConn)
			if err != nil {
				client.log.Errorf("forward client connection from %s: %v", leftConn.RemoteAddr(), err)
			}
		}()
	}
}

//ForwardConn forwards this connection to server.
//It will block until server-side connection is closed
//or c is closed
func (client *Client) ForwardConn(c net.Conn) error {
	var rightConn net.Conn
	var err error

	if client.conf.EnableMux {
		rightConn, err = client.getMuxStream()
		if err != nil {
			return fmt.Errorf("mux getStream: %v", err)
		}
	} else {
		rightConn, err = client.dialServer()
		if err != nil {
			return fmt.Errorf("connect to remote: %v", err)
		}
	}
	defer rightConn.Close()

	err = openTunnel(c, rightConn, client.conf.Timeout)
	if err != nil {
		return fmt.Errorf("openTunnel: %v", err)
	}
	return nil
}

//Close shutdown client
func (client *Client) Close() error {
	client.listenerLocker.Lock()
	defer client.listenerLocker.Unlock()
	if client.listener != nil {
		return client.listener.Close()
	}
	return nil
}

func (client *Client) dialWSS() (net.Conn, error) {
	return dialWebsocketConn(client.wsDialer, client.wssURL)
}

func (client *Client) dialTLS() (net.Conn, error) {
	raw, err := client.dialServerRaw()
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, client.tlsConf)
	if err := conn.Handshake(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func (client *Client) dialServer() (net.Conn, error) {
	if client.conf.EnableWSS {
		return client.dialWSS()
	}
	return client.dialTLS()
}

func (client *Client) dialServerRaw() (net.Conn, error) {
	if client.testDialServerRaw == nil {
		return client.netDialer.Dial("tcp", client.conf.RemoteAddr)
	}
	return client.testDialServerRaw()
}

type smuxSessPool struct {
	sync.Map
}

func (client *Client) dialNewSmuxSess() (*smux.Session, error) {
	rightConn, err := client.dialServer()
	if err != nil {
		return nil, err
	}
	sess, err := smux.Client(rightConn, client.smuxConfig)
	if err != nil {
		rightConn.Close()
		return nil, err
	}

	client.log.Debugf("new sess %p opend", sess)
	return sess, nil
}

func (client *Client) getMuxStream() (*muxStream, error) {
	var stream *muxStream

	try := func(key, value interface{}) bool {
		sess := key.(*muxSession)
		if sess.IsClosed() {
			client.smuxSessPool.Delete(sess)
			client.log.Debugf("deleted closed sess %p", sess)
			return true
		}

		// try
		var er error
		stream, er = sess.openStream(client.conf.MuxMaxStream)
		if er == ErrTooManyStreams {
			return true // this session opened too many streams, try next
		}
		if er != nil {
			client.smuxSessPool.Delete(sess)
			client.log.Warnf("deleted err sess %p: open stream: %v", sess, er)
			return true
		}
		return false
	}

	client.smuxSessPool.Range(try)

	if stream == nil {
		sess, err := client.dialNewSmuxSess()
		if err != nil {
			return nil, err
		}
		muxSess := newMuxSession(sess)
		client.smuxSessPool.Store(muxSess, nil)
		return muxSess.openStream(client.conf.MuxMaxStream)
	}
	return stream, nil
}
