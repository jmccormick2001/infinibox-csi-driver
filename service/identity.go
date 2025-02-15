/*Copyright 2022 Infinidat
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.*/
package service

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/golang/protobuf/ptypes/wrappers"
	"k8s.io/klog"
)

func (s *service) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	return &csi.GetPluginInfoResponse{
		Name:          s.driverName,
		VendorVersion: s.driverVersion,
	}, nil
}

func (s *service) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
			// To be reported when topology contstraints are supported
			// {
			// 	Type: &csi.PluginCapability_Service_{
			// 		Service: &csi.PluginCapability_Service{
			// 			Type: csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS,
			// 		},
			// 	},
			// },
		},
	}, nil
}

func (s *service) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	ready := new(wrappers.BoolValue)
	ready.Value = true
	proberes := new(csi.ProbeResponse)
	proberes.Ready = ready
	klog.V(4).Infof("Probe returning: %v", proberes.Ready.GetValue())
	return proberes, nil
}
