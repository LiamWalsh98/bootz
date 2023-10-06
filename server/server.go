// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Bootz server reference implementation.
//
// The bootz server will provide a simple file based bootstrap
// implementation for devices. The service can be extended by
// provding your own implementation of the entity manager.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	log "github.com/golang/glog"
	"github.com/openconfig/bootz/dhcp"
	"github.com/openconfig/bootz/server/entitymanager"
	"github.com/openconfig/bootz/server/service"
	// "golang.org/x/tools/cmd/guru/serial"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	bpb "github.com/openconfig/bootz/proto/bootz"
)

var (
	bootzAddress      = flag.String("address", "8008", "The [ip:]port to listen for the bootz request. when ip is not given, the server will listen on localhost.")
	dhcpIntf          = flag.String("dhcp_intf", "", "Network interface to use for dhcp server.")
	artifactDirectory = flag.String("artifact_dir", "../testdata/", "The relative directory to look into for certificates, private keys and OVs.")
	inventoryConfig   = flag.String("inv_config", "../testdata/inventory_local.prototxt", "Devices' config files to be loaded by inventory manager")
)

type server struct {
	serv *grpc.Server
	lis  net.Listener
    status string
    lock sync.Mutex
    config ServerConfig
}

type ServerConfig struct {
    // port              string
	DhcpIntf          string
	ArtifactDirectory string
	InventoryConfig   string
}

// var lock = &sync.Mutex{}

// Convert address to localhost when no ip is specified.
func convertAddress(addr string) string {
	items := strings.Split(addr, ":")
	listenAddr := addr
	if len(items) == 1 {
		listenAddr = fmt.Sprintf("localhost:%v", addr)
	}
	return listenAddr
}

// readKeyPair reads the cert/key pair from the specified artifacts directory.
// Certs must have the format {name}_pub.pem and keys must have the format {name}_priv.pem.
func readKeypair(name string) (*service.KeyPair, error) {
	cert, err := os.ReadFile(filepath.Join(*artifactDirectory, fmt.Sprintf("%v_pub.pem", name)))
	if err != nil {
		return nil, fmt.Errorf("unable to read %v cert: %v", name, err)
	}
	key, err := os.ReadFile(filepath.Join(*artifactDirectory, fmt.Sprintf("%v_priv.pem", name)))
	if err != nil {
		return nil, fmt.Errorf("unable to read %v key: %v", name, err)
	}
	return &service.KeyPair{
		Cert: string(cert),
		Key:  string(key),
	}, nil
}

// readOVs discovers and reads all available OVs in the artifacts directory.
func readOVs() (service.OVList, error) {
	ovs := make(service.OVList)
	files, err := os.ReadDir(*artifactDirectory)
	if err != nil {
		return nil, fmt.Errorf("unable to list files in artifact directory: %v", err)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "ov") {
			bytes, err := os.ReadFile(filepath.Join(*artifactDirectory, f.Name()))
			if err != nil {
				return nil, err
			}
			trimmed := strings.TrimPrefix(f.Name(), "ov_")
			trimmed = strings.TrimSuffix(trimmed, ".txt")
			ovs[trimmed] = string(bytes)
		}
	}
	if len(ovs) == 0 {
		return nil, fmt.Errorf("found no OVs in artifacts directory")
	}
	return ovs, err
}

// generateServerTLSCert creates a new TLS keypair from the PDC.
func generateServerTLSCert(pdc *service.KeyPair) (*tls.Certificate, error) {
	tlsCert, err := tls.X509KeyPair([]byte(pdc.Cert), []byte(pdc.Key))
	if err != nil {
		return nil, fmt.Errorf("unable to generate Server TLS Certificate from PDC %v", err)
	}
	return &tlsCert, err
}

// parseSecurityArtifacts reads from the specified directory to find the required keypairs and ownership vouchers.
func parseSecurityArtifacts() (*service.SecurityArtifacts, error) {
	oc, err := readKeypair("oc")
	if err != nil {
		return nil, err
	}
	pdc, err := readKeypair("pdc")
	if err != nil {
		return nil, err
	}
	vendorCA, err := readKeypair("vendorca")
	if err != nil {
		return nil, err
	}
	ovs, err := readOVs()
	if err != nil {
		return nil, err
	}
	tlsCert, err := generateServerTLSCert(pdc)
	if err != nil {
		return nil, err
	}
	return &service.SecurityArtifacts{
		OC:         oc,
		PDC:        pdc,
		VendorCA:   vendorCA,
		OV:         ovs,
		TLSKeypair: tlsCert,
	}, nil
}

func (s *server) Start(bootzAddress string, config ServerConfig) (string, error) {
    s.lock.Lock()
    defer s.lock.Unlock()
    
    s.status = "Failure"
    
	if config.ArtifactDirectory == "" {
		return s.status, fmt.Errorf("no artifact directory selected. specify with the --artifact_dir flag")
	}

	if bootzAddress == "" {
		log.Exitf("no port selected. specify with the -port flag")
	}
    
	log.Infof("Setting up server security artifacts: OC, OVs, PDC, VendorCA")
	sa, err := parseSecurityArtifacts()
	if err != nil {
		return s.status, err
	}
    
	log.Infof("Setting up entities")
	em, err := entitymanager.New(config.InventoryConfig)
	if err != nil {
		return s.status, fmt.Errorf("unable to initiate inventory manager %v", err)
	}

	c := service.New(em)
    
	trustBundle := x509.NewCertPool()
	if !trustBundle.AppendCertsFromPEM([]byte(sa.PDC.Cert)) {
		return s.status, fmt.Errorf("unable to add PDC cert to trust pool")
	}
	tls := &tls.Config{
		Certificates: []tls.Certificate{*sa.TLSKeypair},
		RootCAs:      trustBundle,
	}
	log.Infof("Creating server...")
	newServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(tls)))
	bpb.RegisterBootstrapServer(newServer, c)

	lis, err := net.Listen("tcp", convertAddress(bootzAddress))
	if err != nil {
		return s.status, fmt.Errorf("error listening on port: %v", err)
	}
	log.Infof("Server ready and listening on %s", lis.Addr())
	log.Infof("=============================================================================")
    
    s.status = "Running"
    s.serv = newServer 
    s.lis = lis
    
	return s.status, nil
    
}

func (s *server) Stop() (string, error){
    s.lock.Lock()
    defer s.lock.Unlock()
	s.serv.GracefulStop()
    s.status = "Exited"
    return s.status, nil
}

func (s *server) Reload() (string, error) {
    addr := s.lis.Addr().String()
    s.Stop()
    _, err :=  s.Start(addr, s.config)
    return s.status, err 
}

func (s *server) Status() (string, error) {
    return s.status, nil
}

func (s *server) BootLogs() (error) {
    return nil
}

// newServer creates a new Bootz gRPC server from flags.
func newServer() (*server, error) {
	if *artifactDirectory == "" {
		return nil, fmt.Errorf("no artifact directory selected. specify with the --artifact_dir flag")
	}

	log.Infof("Setting up server security artifacts: OC, OVs, PDC, VendorCA")
	sa, err := parseSecurityArtifacts()
	if err != nil {
		return nil, err
	}

	log.Infof("Setting up entities")
	em, err := entitymanager.New(*inventoryConfig)
	if err != nil {
		return nil, fmt.Errorf("unable to initiate inventory manager %v", err)
	}

	if *dhcpIntf != "" {
		if err := startDhcpServer(em); err != nil {
			return nil, fmt.Errorf("unable to start dhcp server %v", err)
		}
	}

	c := service.New(em)

	trustBundle := x509.NewCertPool()
	if !trustBundle.AppendCertsFromPEM([]byte(sa.PDC.Cert)) {
		return nil, fmt.Errorf("unable to add PDC cert to trust pool")
	}
	tls := &tls.Config{
		Certificates: []tls.Certificate{*sa.TLSKeypair},
		RootCAs:      trustBundle,
	}
	log.Infof("Creating server...")
	s := grpc.NewServer(grpc.Creds(credentials.NewTLS(tls)))
	bpb.RegisterBootstrapServer(s, c)

	lis, err := net.Listen("tcp", convertAddress(*bootzAddress))
	if err != nil {
		return nil, fmt.Errorf("error listening on port: %v", err)
	}
	log.Infof("Server ready and listening on %s", lis.Addr())
	log.Infof("=============================================================================")
	return &server{serv: s, lis: lis}, nil
}

func main() {
	flag.Parse()

	log.Infof("=============================================================================")
	log.Infof("=========================== BootZ Server Emulator ===========================")
	log.Infof("=============================================================================")

	// s, err := newServer()
	// if err != nil {
	// 	log.Exit(err)
	// }

    s := server{}

    config := ServerConfig{
        DhcpIntf          : "",
        ArtifactDirectory : "../testdata/",
        InventoryConfig   : "../testdata/inventory_local.prototxt",
    }

	if _,err := s.Start("127.0.0.1", config); err != nil {
		log.Exit(err)
	}
}

func startDhcpServer(em *entitymanager.InMemoryEntityManager) error {
	conf := &dhcp.Config{
		Interface:  *dhcpIntf,
		AddressMap: make(map[string]*dhcp.Entry),
	}

	for _, c := range em.GetChassisInventory() {
		if dhcpConf := c.GetDhcpConfig(); dhcpConf != nil {
			key := dhcpConf.GetHardwareAddress()
			if key == "" {
				key = c.GetSerialNumber()
			}
			conf.AddressMap[key] = &dhcp.Entry{
				IP: dhcpConf.GetIpAddress(),
				Gw: dhcpConf.GetGateway(),
			}
		}
	}

	return dhcp.Start(conf)
}
