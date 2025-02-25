// Copyright (c) 2017-2018 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package syncclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/gob"
	"errors"
	"io/ioutil"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/unai-ttxu/libcalico-go/lib/backend/api"
	"github.com/unai-ttxu/typha/pkg/syncproto"
	"github.com/unai-ttxu/typha/pkg/tlsutils"
)

var nextID uint64

const (
	defaultReadtimeout  = 30 * time.Second
	defaultWriteTimeout = 10 * time.Second
)

type Options struct {
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	KeyFile      string
	CertFile     string
	CAFile       string
	ServerCN     string
	ServerURISAN string
	SyncerType   syncproto.SyncerType
}

func (o *Options) readTimeout() time.Duration {
	if o == nil || o.ReadTimeout <= 0 {
		return defaultReadtimeout
	}
	return o.ReadTimeout
}

func (o *Options) writeTimeout() time.Duration {
	if o == nil || o.WriteTimeout <= 0 {
		return defaultWriteTimeout
	}
	return o.WriteTimeout
}

func (o *Options) requiringTLS() bool {
	// True if any of the TLS parameters are set.
	requiringTLS := o != nil && o.KeyFile+o.CertFile+o.CAFile+o.ServerCN+o.ServerURISAN != ""
	log.WithField("requiringTLS", requiringTLS).Info("")
	return requiringTLS
}

func (o *Options) validate() (err error) {
	// If any client-side TLS options are specified, they _all_ must be - except that either
	// ServerCN or ServerURISAN may be left unset.
	if o.requiringTLS() {
		// Some TLS options specified.
		if o.KeyFile == "" ||
			o.CertFile == "" ||
			o.CAFile == "" ||
			(o.ServerCN == "" && o.ServerURISAN == "") {
			err = errors.New("If any Felix-Typha TLS options are specified," +
				" they _all_ must be" +
				" - except that either ServerCN or ServerURISAN may be left unset.")
		}
	}
	return
}

func New(
	addr string,
	myVersion, myHostname, myInfo string,
	cbs api.SyncerCallbacks,
	options *Options,
) *SyncerClient {
	if err := options.validate(); err != nil {
		log.WithField("options", options).WithError(err).Fatal("Invalid options")
	}
	id := nextID
	nextID++
	if options == nil {
		options = &Options{}
	}
	return &SyncerClient{
		ID: id,
		logCxt: log.WithFields(log.Fields{
			"connID": id,
			"type":   options.SyncerType,
		}),
		callbacks: cbs,
		addr:      addr,

		myVersion:  myVersion,
		myHostname: myHostname,
		myInfo:     myInfo,

		options: options,
	}
}

type SyncerClient struct {
	ID                            uint64
	logCxt                        *log.Entry
	addr                          string
	myHostname, myVersion, myInfo string
	options                       *Options

	connection net.Conn
	encoder    *gob.Encoder
	decoder    *gob.Decoder

	callbacks api.SyncerCallbacks
	Finished  sync.WaitGroup
}

func (s *SyncerClient) Start(cxt context.Context) error {
	// Connect synchronously.
	err := s.connect(cxt)
	if err != nil {
		return err
	}

	// Then start our background goroutines.  We start the main loop and a second goroutine to
	// manage shutdown.
	cxt, cancelFn := context.WithCancel(cxt)
	s.Finished.Add(1)
	go s.loop(cxt, cancelFn)

	s.Finished.Add(1)
	go func() {
		// Wait for the context to finish, either due to external cancel or our own loop
		// exiting.
		<-cxt.Done()
		s.logCxt.Info("Typha client Context asked us to exit")
		// Close the connection.  This will trigger the main loop to exit if it hasn't
		// already.
		err := s.connection.Close()
		if err != nil {
			log.WithError(err).Warn("Ignoring error from Close during shut-down of client.")
		}
		// Broadcast that we're finished.
		s.Finished.Done()
	}()
	return nil
}

func (s *SyncerClient) connect(cxt context.Context) error {
	log.Info("Starting Typha client")
	var err error
	logCxt := s.logCxt.WithField("address", s.addr)

	var connFunc func() (net.Conn, error)
	if s.options.requiringTLS() {
		cert, err := tls.LoadX509KeyPair(s.options.CertFile, s.options.KeyFile)
		if err != nil {
			log.WithError(err).Error("Failed to load certificate and key")
			return err
		}
		tlsConfig := tls.Config{Certificates: []tls.Certificate{cert}}

		// Set InsecureSkipVerify true, because when it's false crypto/tls insists on
		// verifying the server's hostname or IP address against tlsConfig.ServerName, and
		// we don't always want that.  We will do certificate chain verification ourselves
		// inside CertificateVerifier.
		tlsConfig.InsecureSkipVerify = true
		caPEMBlock, err := ioutil.ReadFile(s.options.CAFile)
		if err != nil {
			log.WithError(err).Error("Failed to read CA data")
			return err
		}
		tlsConfig.RootCAs = x509.NewCertPool()
		ok := tlsConfig.RootCAs.AppendCertsFromPEM(caPEMBlock)
		if !ok {
			log.Error("Failed to add CA data to pool")
			return errors.New("Failed to add CA data to pool")
		}
		tlsConfig.VerifyPeerCertificate = tlsutils.CertificateVerifier(
			logCxt,
			tlsConfig.RootCAs,
			s.options.ServerCN,
			s.options.ServerURISAN,
		)

		connFunc = func() (net.Conn, error) {
			return tls.DialWithDialer(
				&net.Dialer{Timeout: 10 * time.Second},
				"tcp",
				s.addr,
				&tlsConfig)
		}
	} else {
		connFunc = func() (net.Conn, error) {
			return net.DialTimeout("tcp", s.addr, 10*time.Second)
		}
	}
	if cxt.Err() == nil {
		logCxt.Info("Connecting to Typha.")
		s.connection, err = connFunc()
		if err != nil {
			return err
		}
	}
	if cxt.Err() != nil {
		if s.connection != nil {
			err := s.connection.Close()
			if err != nil {
				log.WithError(err).Warn("Ignoring error from Close during shut-down of client.")
			}
		}
		return cxt.Err()
	}
	logCxt.Info("Connected to Typha.")

	// Log TLS connection details.
	tlsConn, ok := s.connection.(*tls.Conn)
	log.WithField("ok", ok).Debug("TLS conn?")
	if ok {
		state := tlsConn.ConnectionState()
		for _, v := range state.PeerCertificates {
			bytes, _ := x509.MarshalPKIXPublicKey(v.PublicKey)
			logCxt.Debugf("%#v", bytes)
			logCxt.Debugf("%#v", v.Subject)
			logCxt.Debugf("%#v", v.URIs)
		}
		logCxt.WithFields(log.Fields{
			"handshake": state.HandshakeComplete,
			"mutual":    state.NegotiatedProtocolIsMutual,
		}).Debug("TLS negotiation")
	}

	return nil
}

func (s *SyncerClient) logConnectionFailure(cxt context.Context, logCxt *log.Entry, err error, operation string) {
	if cxt.Err() != nil {
		logCxt.WithError(err).Warn("Connection failed while being shut down by context.")
		return
	}
	logCxt.WithError(err).Errorf("Failed to %s", operation)
}

func (s *SyncerClient) loop(cxt context.Context, cancelFn context.CancelFunc) {
	defer s.Finished.Done()
	defer cancelFn()

	logCxt := s.logCxt.WithField("address", s.addr)
	logCxt.Info("Started Typha client main loop")

	s.encoder = gob.NewEncoder(s.connection)
	s.decoder = gob.NewDecoder(s.connection)

	ourSyncerType := s.options.SyncerType
	if ourSyncerType == "" {
		ourSyncerType = syncproto.SyncerTypeFelix
	}
	err := s.sendMessageToServer(cxt, logCxt, "send hello to server",
		syncproto.MsgClientHello{
			Hostname:   s.myHostname,
			Version:    s.myVersion,
			Info:       s.myInfo,
			SyncerType: ourSyncerType,
		},
	)
	if err != nil {
		return // (Failure already logged.)
	}

	for cxt.Err() == nil {
		msg, err := s.readMessageFromServer(cxt, logCxt)
		if err != nil {
			return
		}
		switch msg := msg.(type) {
		case syncproto.MsgSyncStatus:
			logCxt.WithField("newStatus", msg.SyncStatus).Info("Status update from Typha.")
			s.callbacks.OnStatusUpdated(msg.SyncStatus)
		case syncproto.MsgPing:
			logCxt.Debug("Ping received from Typha")
			err := s.sendMessageToServer(cxt, logCxt, "write pong to server",
				syncproto.MsgPong{
					PingTimestamp: msg.Timestamp,
				},
			)
			if err != nil {
				return // (Failure already logged.)
			}
			logCxt.Debug("Pong sent to Typha")
		case syncproto.MsgKVs:
			updates := make([]api.Update, 0, len(msg.KVs))
			for _, kv := range msg.KVs {
				update, err := kv.ToUpdate()
				if err != nil {
					logCxt.WithError(err).Error("Failed to deserialize update, skipping.")
					continue
				}
				logCxt.WithFields(log.Fields{
					"serialized":   kv,
					"deserialized": update,
				}).Debug("Decoded update from Typha")
				updates = append(updates, update)
			}
			s.callbacks.OnUpdates(updates)
		case syncproto.MsgServerHello:
			logCxt.WithField("serverVersion", msg.Version).Info("Server hello message received")

			// Check the SyncerType reported by the server.  If the server is too old to support SyncerType then
			// the message will have an empty string in place of the SyncerType.  In that case we only proceed if
			// the client wants the felix syncer.
			serverSyncerType := msg.SyncerType
			if serverSyncerType == "" {
				logCxt.Info("Server responded without SyncerType, assuming an old Typha version that only " +
					"supports SyncerTypeFelix.")
				serverSyncerType = syncproto.SyncerTypeFelix
			}
			if ourSyncerType != serverSyncerType {
				logCxt.Error("We require SyncerType %s but Typha server doesn't support it.", ourSyncerType)
				return
			}
		}
	}
}

// sendMessageToServer sends a single value-type MsgXYZ object to the server.  It updates the connection's
// write deadline to ensure we don't block forever.  Logs errors via logConnectionFailure.
func (s *SyncerClient) sendMessageToServer(cxt context.Context, logCxt *log.Entry, op string, message interface{}) error {
	err := s.connection.SetWriteDeadline(time.Now().Add(s.options.writeTimeout()))
	if err != nil {
		s.logConnectionFailure(cxt, logCxt, err, "set timeout before "+op)
		return err
	}
	err = s.encoder.Encode(syncproto.Envelope{
		Message: message,
	})
	if err != nil {
		s.logConnectionFailure(cxt, logCxt, err, op)
		return err
	}
	return nil
}

// readMessageFromServer reads a single value-type MsgXYZ object from the server.  It updates the connection's
// read deadline to ensure we don't block forever.  Logs errors via logConnectionFailure.
func (s *SyncerClient) readMessageFromServer(cxt context.Context, logCxt *log.Entry) (interface{}, error) {
	var envelope syncproto.Envelope
	// Update the read deadline before we try to read, otherwise we could block for a very long time if the
	// TCP connection was severed without being cleanly shut down.  Typha sends regular pings so we should receive
	// something even if there are no datamodel updates.
	err := s.connection.SetReadDeadline(time.Now().Add(s.options.readTimeout()))
	if err != nil {
		s.logConnectionFailure(cxt, logCxt, err, "set read timeout")
		return nil, err
	}
	err = s.decoder.Decode(&envelope)
	if err != nil {
		s.logConnectionFailure(cxt, logCxt, err, "read from server")
		return nil, err
	}
	logCxt.WithField("envelope", envelope).Debug("New message from Typha.")
	return envelope.Message, nil
}
