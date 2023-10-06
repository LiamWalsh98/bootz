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

// Package service receives bootstrap requests and responds with the relevant data.
package service

import (
	"context"
	"crypto/tls"
	"fmt"
	"sync"
	"time"

	"github.com/openconfig/gnmi/errlist"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	log "github.com/golang/glog"
	bpb "github.com/openconfig/bootz/proto/bootz"
)

// OVList is a mapping of control card serial number to ownership voucher.
type OVList map[string]string

// KeyPair is a struct containing PEM-encoded certificates and private keys.
type KeyPair struct {
	Cert string
	Key  string
}

// SecurityArtifacts contains all KeyPairs and OVs needed for the Bootz Server.
// Currently, RSA is the only encryption standard supported by these artifacts.
type SecurityArtifacts struct {
	// The Ownership Certificate is an x509 certificate/private key pair signed by the PDC.
	// The certificate is presented to the device during bootstrapping and is used to validate the Ownership Voucher.
	OC *KeyPair
	// The Pinned Domain Certificate is an x509 certificate/private key pair which acts as a certificate authority on the owner's side.
	// This certificate is included in OVs and is also used to generate a server TLS Cert in this implementation.
	PDC *KeyPair
	// The Vendor CA represents a certificate authority on the vendor side. This CA signs Ownership Vouchers which are verified by the device.
	VendorCA *KeyPair
	// Ownership Vouchers are a list of PKCS7 messages signed by the Vendor CA. There is one per control card.
	OV OVList
	// The TLSKeypair is a TLS certificate used to secure connections between device and server. It is derived from the Pinned Domain Cert.
	TLSKeypair *tls.Certificate
}

// EntityLookup provides a way to resolve chassis and control cards
// in the EntityManager.
type EntityLookup struct {
	Manufacturer string
	SerialNumber string
}

// ChassisEntity provides the mode that the system is currently
// configured.
type ChassisEntity struct {
	BootMode bpb.BootMode
}

// an struct to record the boot logs for connected chassis
type bootLog struct {
	BootMode       bpb.BootMode
	StartTimeStamp uint64
	EndTimeStamp   uint64
	Status         []bpb.ControlCardState_ControlCardStatus
	LastStatus     bpb.ControlCardState_ControlCardStatus
	BootResponse   *bpb.BootstrapDataResponse
	BootRequest    *bpb.GetBootstrapDataRequest
	Err            error
}

// EntityManager maintains the entities and their states.
type EntityManager interface {
	ResolveChassis(*EntityLookup, string) (*ChassisEntity, error)
	GetBootstrapData(*EntityLookup, *bpb.ControlCard) (*bpb.BootstrapDataResponse, error)
	SetStatus(*bpb.ReportStatusRequest) error
	Sign(*bpb.GetBootstrapDataResponse, *EntityLookup, string) error
}

// Service represents the server and entity manager.
type Service struct {
	bpb.UnimplementedBootstrapServer
	em               EntityManager
	mu               sync.Mutex
	connectedChassis map[EntityLookup]bool
	activeBoots      map[string]*bootLog
	failedRequest    map[*bpb.GetBootstrapDataRequest]error
}

func (s *Service) GetBootstrapData(ctx context.Context, req *bpb.GetBootstrapDataRequest) (*bpb.GetBootstrapDataResponse, error) {
	log.Infof("=============================================================================")
	log.Infof("==================== Received request for bootstrap data ====================")
	log.Infof("=============================================================================")
	s.mu.Lock()
	defer s.mu.Unlock()
	fixedChasis := true
	ccSerial := ""
	if len(req.ChassisDescriptor.ControlCards) == 0 {
		s.failedRequest[req] = status.Errorf(codes.InvalidArgument, "request must include at least one control card")
		return nil, status.Errorf(codes.InvalidArgument, "request must include at least one control card")
	}
	if len(req.ChassisDescriptor.ControlCards) >= 1 {
		fixedChasis = false
		ccSerial = req.ChassisDescriptor.GetControlCards()[0].GetSerialNumber()
    }
	log.Infof("Requesting for %v chassis %v", req.ChassisDescriptor.Manufacturer, req.ChassisDescriptor.SerialNumber)
	lookup := &EntityLookup{
		Manufacturer: req.ChassisDescriptor.Manufacturer,
		SerialNumber: req.ChassisDescriptor.SerialNumber,
	}
	// Validate the chassis can be serviced
	chassis, err := s.em.ResolveChassis(lookup, ccSerial)
	if err != nil {
s.failedRequest[req] = status.Errorf(codes.InvalidArgument, "failed to resolve chassis to inventory %+v, err: %v", req.ChassisDescriptor, err)
		return nil, status.Errorf(codes.InvalidArgument, "failed to resolve chassis to inventory %+v, err: %v", req.ChassisDescriptor, err)
	}
	log.Infof("Verified server can resolve chassis")
	s.connectedChassis[*lookup] = true

	// If chassis can only be booted into secure mode then return error
	if chassis.BootMode == bpb.BootMode_BOOT_MODE_SECURE && req.Nonce == "" {
		return nil, status.Errorf(codes.InvalidArgument, "chassis requires secure boot only")
	}

	// Iterate over the control cards and fetch data for each card.
	var errs errlist.List

	log.Infof("=============================================================================")
	log.Infof("==================== Fetching data for each control card ====================")
	log.Infof("=============================================================================")
	var responses []*bpb.BootstrapDataResponse
	for _, v := range req.ChassisDescriptor.ControlCards {
		s.activeBoots[v.GetSerialNumber()] = &bootLog{
			BootMode:       chassis.BootMode,
			StartTimeStamp: uint64(time.Now().UnixMilli()),
			BootRequest:    req,
			LastStatus:     bpb.ControlCardState_CONTROL_CARD_STATUS_UNSPECIFIED,
		}
		bootdata, err := s.em.GetBootstrapData(lookup, v)
		if err != nil {
			s.activeBoots[v.GetSerialNumber()].Err = err
			errs.Add(err)
			log.Infof("Error occurred while retrieving data for Serial Number %v", v.SerialNumber)
		}
		s.activeBoots[v.GetSerialNumber()].BootResponse = bootdata
		responses = append(responses, bootdata)
	}
	if fixedChasis {
		bootdata, err := s.em.GetBootstrapData(lookup, nil)
		if err != nil {
			errs.Add(err)
			log.Infof("Error occurred while retrieving data for fixed chassis with serail number %v", lookup.SerialNumber)
		}
		responses = append(responses, bootdata)
	}

	if errs.Err() != nil {
		return nil, errs.Err()
	}
	log.Infof("Successfully fetched data for each control card")
	log.Infof("=============================================================================")

	resp := &bpb.GetBootstrapDataResponse{
		SignedResponse: &bpb.BootstrapDataSigned{
			Responses: responses,
		},
	}
	log.Infof("Response set")

	// Sign the response if Nonce is provided.
	if req.Nonce != "" {
		log.Infof("=============================================================================")
		log.Infof("====================== Signing the response with nonce ======================")
		log.Infof("=============================================================================")
		resp.SignedResponse.Nonce = req.Nonce
		if err := s.em.Sign(resp, lookup, req.GetControlCardState().GetSerialNumber()); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to sign bootz response")
		}
		log.Infof("Signed with nonce")
	}
	log.Infof("Returning response")
	return resp, nil
}

func (s *Service) ReportStatus(ctx context.Context, req *bpb.ReportStatusRequest) (*bpb.EmptyResponse, error) {
	log.Infof("=============================================================================")
	log.Infof("========================== Status report received ===========================")
	log.Infof("=============================================================================")
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.em.SetStatus(req)
	if err != nil {
		for _, stat := range req.GetStates() {
			s.activeBoots[stat.GetSerialNumber()].LastStatus = stat.GetStatus()
			s.activeBoots[stat.SerialNumber].Status = append(s.activeBoots[stat.SerialNumber].Status, stat.GetStatus())
			if stat.GetStatus() == bpb.ControlCardState_CONTROL_CARD_STATUS_INITIALIZED {
				s.activeBoots[stat.SerialNumber].EndTimeStamp = uint64(time.Now().UnixMilli())
			}
		}
		return &bpb.EmptyResponse{}, nil
	}
	return nil, err

}

// IsChassisConnected checks if a device is connected to Bootz Server
func (s *Service) IsChassisConnected(chassis EntityLookup) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connectedChassis[chassis]
}

// ResetStatus clears boot log for devices.
// This is intended to use for testing and can be used to clear logs without restarting servive.
func (s *Service) ResetStatus(chassis EntityLookup) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connectedChassis = map[EntityLookup]bool{}
	s.failedRequest = map[*bpb.GetBootstrapDataRequest]error{}
	s.activeBoots = map[string]*bootLog{}
}

// GetBootStatus return boot log for a controller card. This is intended to use for testing.
func (s *Service) GetBootStatus(serial string) (bootLog, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.activeBoots[serial]
	if !ok {
		return bootLog{}, fmt.Errorf("no boot log found for controller card %s", serial)
	}
	return *b, nil
}

// SetDeviceConfiguration is a public API for allowing the device configuration to be set for each device the
// will be responsible for configuring.  This will be only available for testing.
func (s *Service) SetDeviceConfiguration(ctx context.Context) error {
	return status.Errorf(codes.Unimplemented, "Unimplemented")
}

// New creates a new service.
func New(em EntityManager) *Service {
	return &Service{
		em:               em,
		connectedChassis: map[EntityLookup]bool{},
		failedRequest:    map[*bpb.GetBootstrapDataRequest]error{},
		activeBoots:      map[string]*bootLog{},
	}
}
