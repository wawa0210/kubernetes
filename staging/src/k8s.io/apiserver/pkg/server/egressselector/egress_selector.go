/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package egressselector

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"google.golang.org/grpc"
	"io/ioutil"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apiserver/pkg/apis/apiserver"
	"k8s.io/klog"
	"net"
	"net/http"
	"net/url"
	client "sigs.k8s.io/apiserver-network-proxy/konnectivity-client/pkg/client"
	"strings"
)

var directDialer utilnet.DialFunc = http.DefaultTransport.(*http.Transport).DialContext

// EgressSelector is the map of network context type to context dialer, for network egress.
type EgressSelector struct {
	egressToDialer map[EgressType]utilnet.DialFunc
}

// EgressType is an indicator of which egress selection should be used for sending traffic.
// See https://github.com/kubernetes/enhancements/blob/master/keps/sig-api-machinery/20190226-network-proxy.md#network-context
type EgressType int

const (
	// Master is the EgressType for traffic intended to go to the control plane.
	Master EgressType = iota
	// Etcd is the EgressType for traffic intended to go to Kubernetes persistence store.
	Etcd
	// Cluster is the EgressType for traffic intended to go to the system being managed by Kubernetes.
	Cluster
)

// NetworkContext is the struct used by Kubernetes API Server to indicate where it intends traffic to be sent.
type NetworkContext struct {
	// EgressSelectionName is the unique name of the
	// EgressSelectorConfiguration which determines
	// the network we route the traffic to.
	EgressSelectionName EgressType
}

// Lookup is the interface to get the dialer function for the network context.
type Lookup func(networkContext NetworkContext) (utilnet.DialFunc, error)

// String returns the canonical string representation of the egress type
func (s EgressType) String() string {
	switch s {
	case Master:
		return "master"
	case Etcd:
		return "etcd"
	case Cluster:
		return "cluster"
	default:
		return "invalid"
	}
}

// AsNetworkContext is a helper function to make it easy to get the basic NetworkContext objects.
func (s EgressType) AsNetworkContext() NetworkContext {
	return NetworkContext{EgressSelectionName: s}
}

func lookupServiceName(name string) (EgressType, error) {
	switch strings.ToLower(name) {
	case "master":
		return Master, nil
	case "etcd":
		return Etcd, nil
	case "cluster":
		return Cluster, nil
	}
	return -1, fmt.Errorf("unrecognized service name %s", name)
}

func tunnelHTTPConnect(proxyConn net.Conn, proxyAddress, addr string) (net.Conn, error) {
	fmt.Fprintf(proxyConn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, "127.0.0.1")
	br := bufio.NewReader(proxyConn)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		proxyConn.Close()
		return nil, fmt.Errorf("reading HTTP response from CONNECT to %s via proxy %s failed: %v",
			addr, proxyAddress, err)
	}
	if res.StatusCode != 200 {
		proxyConn.Close()
		return nil, fmt.Errorf("proxy error from %s while dialing %s, code %d: %v",
			proxyAddress, addr, res.StatusCode, res.Status)
	}

	// It's safe to discard the bufio.Reader here and return the
	// original TCP conn directly because we only use this for
	// TLS, and in TLS the client speaks first, so we know there's
	// no unbuffered data. But we can double-check.
	if br.Buffered() > 0 {
		proxyConn.Close()
		return nil, fmt.Errorf("unexpected %d bytes of buffered data from CONNECT proxy %q",
			br.Buffered(), proxyAddress)
	}
	return proxyConn, nil
}

func createConnectTCPDialer(tcpTransport *apiserver.TCPTransport) (utilnet.DialFunc, error) {
	clientCert := tcpTransport.TLSConfig.ClientCert
	clientKey := tcpTransport.TLSConfig.ClientKey
	caCert := tcpTransport.TLSConfig.CABundle
	proxyURL, err := url.Parse(tcpTransport.URL)
	if err != nil {
		return nil, fmt.Errorf("invalid proxy server url %q: %v", tcpTransport.URL, err)
	}
	proxyAddress := proxyURL.Host

	clientCerts, err := tls.LoadX509KeyPair(clientCert, clientKey)
	if err != nil {
		return nil, fmt.Errorf("failed to read key pair %s & %s, got %v", clientCert, clientKey, err)
	}
	certPool := x509.NewCertPool()
	if caCert != "" {
		certBytes, err := ioutil.ReadFile(caCert)
		if err != nil {
			return nil, fmt.Errorf("failed to read cert file %s, got %v", caCert, err)
		}
		ok := certPool.AppendCertsFromPEM(certBytes)
		if !ok {
			return nil, fmt.Errorf("failed to append CA cert to the cert pool")
		}
	} else {
		// Use host's root CA set instead of providing our own
		certPool = nil
	}
	contextDialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		klog.V(4).Infof("Sending request to %q.", addr)
		proxyConn, err := tls.Dial("tcp", proxyAddress,
			&tls.Config{
				Certificates: []tls.Certificate{clientCerts},
				RootCAs:      certPool,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("dialing proxy %q failed: %v", proxyAddress, err)
		}
		return tunnelHTTPConnect(proxyConn, proxyAddress, addr)
	}
	return contextDialer, nil
}

func createConnectUDSDialer(udsConfig *apiserver.UDSTransport) (utilnet.DialFunc, error) {
	contextDialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		proxyConn, err := net.Dial("unix", udsConfig.UDSName)
		if err != nil {
			return nil, fmt.Errorf("dialing proxy %q failed: %v", udsConfig.UDSName, err)
		}
		return tunnelHTTPConnect(proxyConn, udsConfig.UDSName, addr)
	}
	return contextDialer, nil
}

func createGRPCUDSDialer(udsName string) (utilnet.DialFunc, error) {
	contextDialer := func(ctx context.Context, network, addr string) (net.Conn, error) {

		dialOption := grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			c, err := net.Dial("unix", udsName)
			if err != nil {
				klog.Errorf("failed to create connection to uds name %s, error: %v", udsName, err)
			}
			return c, err
		})

		tunnel, err := client.CreateGrpcTunnel(udsName, dialOption, grpc.WithInsecure())
		if err != nil {
			return nil, err
		}

		proxyConn, err := tunnel.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		return proxyConn, nil
	}
	return contextDialer, nil
}

// NewEgressSelector configures lookup mechanism for Lookup.
// It does so based on a EgressSelectorConfiguration which was read at startup.
func NewEgressSelector(config *apiserver.EgressSelectorConfiguration) (*EgressSelector, error) {
	if config == nil || config.EgressSelections == nil {
		// No Connection Services configured, leaving the serviceMap empty, will return default dialer.
		return nil, nil
	}
	cs := &EgressSelector{
		egressToDialer: make(map[EgressType]utilnet.DialFunc),
	}
	for _, service := range config.EgressSelections {
		name, err := lookupServiceName(service.Name)
		if err != nil {
			return nil, err
		}
		switch service.Connection.ProxyProtocol {

		case apiserver.ProtocolHTTPConnect:
			if service.Connection.Transport.UDS != nil {
				contextDialer, err := createConnectUDSDialer(service.Connection.Transport.UDS)
				if err != nil {
					return nil, fmt.Errorf("failed to create HTTPConnect uds dialer: %v", err)
				}
				cs.egressToDialer[name] = contextDialer
			} else if service.Connection.Transport.TCP != nil {
				contextDialer, err := createConnectTCPDialer(service.Connection.Transport.TCP)
				if err != nil {
					return nil, fmt.Errorf("failed to create HTTPConnect dialer: %v", err)
				}
				cs.egressToDialer[name] = contextDialer
			} else {
				return nil, fmt.Errorf("Either a TCP or UDS transport must be specified")
			}
		case apiserver.ProtocolGRPC:
			if service.Connection.Transport.UDS != nil {
				grpcContextDialer, err := createGRPCUDSDialer(service.Connection.Transport.UDS.UDSName)
				if err != nil {
					return nil, fmt.Errorf("failed to create grpc dialer: %v", err)
				}
				cs.egressToDialer[name] = grpcContextDialer

			} else {
				return nil, fmt.Errorf("UDS transport must be specified for GRPC")
			}
		case apiserver.ProtocolDirect:
			cs.egressToDialer[name] = directDialer
		default:
			return nil, fmt.Errorf("unrecognized service connection protocol %q", service.Connection.ProxyProtocol)
		}
	}
	return cs, nil
}

// Lookup gets the dialer function for the network context.
// This is configured for the Kubernetes API Server at startup.
func (cs *EgressSelector) Lookup(networkContext NetworkContext) (utilnet.DialFunc, error) {
	if cs.egressToDialer == nil {
		// The round trip wrapper will over-ride the dialContext method appropriately
		return nil, nil
	}
	return cs.egressToDialer[networkContext.EgressSelectionName], nil
}
