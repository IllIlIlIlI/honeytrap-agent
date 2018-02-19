/*
* Honeytrap Agent
* Copyright (C) 2016-2017 DutchSec (https://dutchsec.com/)
*
* This program is free software; you can redistribute it and/or modify it under
* the terms of the GNU Affero General Public License version 3 as published by the
* Free Software Foundation.
*
* This program is distributed in the hope that it will be useful, but WITHOUT
* ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS
* FOR A PARTICULAR PURPOSE.  See the GNU Affero General Public License for more
* details.
*
* You should have received a copy of the GNU Affero General Public License
* version 3 along with this program in the file "LICENSE".  If not, see
* <http://www.gnu.org/licenses/agpl-3.0.txt>.
*
* See https://honeytrap.io/ for more details. All requests should be sent to
* licensing@honeytrap.io
*
* The interactive user interfaces in modified source and object code versions
* of this program must display Appropriate Legal Notices, as required under
* Section 5 of the GNU Affero General Public License version 3.
*
* In accordance with Section 7(b) of the GNU Affero General Public License version 3,
* these Appropriate Legal Notices must retain the display of the "Powered by
* Honeytrap" logo and retain the original copyright notice. If the display of the
* logo is not reasonably feasible for technical reasons, the Appropriate Legal Notices
* must display the words "Powered by Honeytrap" and retain the original copyright notice.
 */
package server

import (
	"context"
	"encoding"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"github.com/fatih/color"
	"github.com/mimoo/disco/libdisco"

	logging "github.com/op/go-logging"
)

var log = logging.MustGetLogger("agent")

type Agent struct {
	in chan encoding.BinaryMarshaler

	conns Connections

	token string

	count uint32

	Server    string
	RemoteKey []byte
}

func New(options ...OptionFn) (*Agent, error) {
	h := &Agent{}

	for _, fn := range options {
		if err := fn(h); err != nil {
			return nil, err
		}
	}

	return h, nil
}

func (a *Agent) newConn(rw net.Conn) (c *conn) {
	defer atomic.AddUint32(&a.count, 1)

	c = &conn{
		Conn:  rw,
		host:  "",
		agent: a,
		id:    atomic.LoadUint32(&a.count),
		out:   make(chan []byte),
		close: make(chan struct{}),
	}

	a.conns.Add(c)
	return c
}

func (a *Agent) serv(l net.Listener) error {
	defer func() {
		defer l.Close()

		log.Error("Listener closed")
	}()

	for {
		rw, err := l.Accept()
		if err != nil {
			log.Errorf("Error while accepting connection: %s", err.Error())
			break
		}

		log.Info(color.YellowString("Accepting connection from %s => %s", rw.RemoteAddr().String(), rw.LocalAddr().String()))

		c := a.newConn(rw)

		go c.serve()
	}

	return nil
}

func localIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, address := range addrs {
		if ipnet, ok := address.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
			if ipnet.IP.To4() == nil {
				continue
			}

			return ipnet.IP.String()
		}
	}
	return ""
}

func (a *Agent) Run(ctx context.Context) {
	fmt.Println(color.YellowString("Honeytrap Agent starting (%s)...", a.token))
	fmt.Println(color.YellowString("Version: %s (%s)", Version, ShortCommitID))

	defer fmt.Println("Honeytrap Agent stopped.")

	go func() {
		for {
			a.in = make(chan encoding.BinaryMarshaler)

			func() {
				log.Info(color.YellowString("Connecting to Honeytrap... "))

				// configure the Disco connection
				clientConfig := libdisco.Config{
					HandshakePattern: libdisco.Noise_NK,
					RemoteKey:        a.RemoteKey,
				}

				dc, err := libdisco.Dial("tcp", a.Server, &clientConfig)
				if err != nil {
					log.Errorf("Error connecting to server: %s: %s", a.Server, err.Error())
					return
				}

				cc := &agentConnection{dc}

				log.Info(color.YellowString("Connected to Honeytrap."))

				defer func() {
					cc.Close()

					log.Info(color.YellowString("Honeytrap disconnected."))
				}()

				cc.send(Handshake{
					Version: Version,
				})

				o, err := cc.receive()
				if err != nil {
					log.Errorf("Invalid handshake response: %s", err.Error())
					return
				}

				hr, ok := o.(*HandshakeResponse)
				if !ok {
					log.Errorf("Invalid handshake response: %s", err.Error())
					return
				}

				listeners := []net.Listener{}

				// we know what ports to listen to
				for _, address := range hr.Addresses {
					if _, ok := address.(*net.TCPAddr); ok {
						l, err := net.Listen(address.Network(), address.String())
						if err != nil {
							log.Errorf(color.RedString("Error starting listener: %s", err.Error()))
							continue
						}

						log.Infof("Listener started: %s", address)

						listeners = append(listeners, l)

						go a.serv(l)
					} else if ua, ok := address.(*net.UDPAddr); ok {
						_ = ua
						log.Errorf("Not implemented yet")
					}
				}

				rwctx, rwcancel := context.WithCancel(context.Background())
				defer func() {
					rwcancel()

					go func() {
						for _ = range a.in {
							// drain
						}
					}()

					a.conns.Each(func(ac *conn) {
						// non blocking
						select {
						case ac.close <- struct{}{}:
						default:
						}
					})

					for _, l := range listeners {
						l.Close()
					}

					close(a.in)
				}()

				go func() {
					defer cc.Close()

					counter := 0

					for {
						select {
						case <-rwctx.Done():
							return
						case <-time.After(time.Second * 5):
							if err := cc.send(Ping{}); err != nil {
								return
							}
						case data, ok := <-a.in:
							if !ok {
								return
							}

							if err := cc.send(data); err != nil {
								return
							}
						}

						counter++
					}
				}()

				for {
					o, err := cc.receive()
					if err == io.EOF {
						return
					} else if err != nil {
						log.Errorf(color.RedString("Error receiving data from server: %s", err.Error()))
						return
					}

					switch v := o.(type) {
					case *ReadWrite:
						conn := a.conns.Get(v.Laddr, v.Raddr)
						if conn == nil {
							continue
						}

						if conn.closed {
							continue
						}

						conn.out <- v.Payload
					case *EOF:
						conn := a.conns.Get(v.Laddr, v.Raddr)
						if conn == nil {
							continue
						}

						select {
						case conn.close <- struct{}{}:
						default:
						}

						a.conns.Delete(conn)
					default:
						// unknown
					}
				}

			}()

			time.Sleep(time.Second * 2)
		}

	}()

	<-ctx.Done()
}
