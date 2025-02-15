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
package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"infinibox-csi-driver/helper"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/containerd/containerd/snapshots/devmapper/dmsetup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/kubernetes/pkg/volume/util"
	utilexec "k8s.io/utils/exec"
	"k8s.io/utils/mount"
)

const (
	devMapperDir     string = dmsetup.DevMapperDir // ie /dev/mapper/
	mpathDeviceCount int    = 6
)

type iscsiDiskUnmounter struct {
	*iscsiDisk
	mounter mount.Interface
	exec    utilexec.Interface // mount.Exec
}

type iscsiDiskMounter struct {
	*iscsiDisk
	readOnly     bool
	fsType       string
	mountOptions []string
	mounter      *mount.SafeFormatAndMount
	exec         utilexec.Interface
	deviceUtil   util.DeviceUtil
	targetPath   string
	stagePath    string
}

type iscsiDisk struct {
	Portals        []string
	Iqn            string
	lun            string
	Iface          string
	chap_discovery bool
	chap_session   bool
	secret         map[string]string
	InitiatorName  string
	VolName        string
	isBlock        bool
	MpathDevice    string
}

var (
	// chap_st = []string{
	// 	"discovery.sendtargets.auth.username",
	// 	"discovery.sendtargets.auth.password",
	// 	"discovery.sendtargets.auth.username_in",
	// 	"discovery.sendtargets.auth.password_in",
	// }
	chap_sess = []string{
		"node.session.auth.username",
		"node.session.auth.password",
		"node.session.auth.username_in",
		"node.session.auth.password_in",
	}
	ifaceTransportNameRe = regexp.MustCompile(`iface.transport_name = (.*)\n`)
)

// Global resouce contains a sync.Mutex. Used to serialize iSCSI resource accesses.
var execScsi helper.ExecScsi

// StatFunc stat a path, if not exists, retry maxRetries times
// when iscsi transports other than default are used
type StatFunc func(string) (os.FileInfo, error)

// GlobFunc  use glob instead as pci id of device is unknown
type GlobFunc func(string) ([]string, error)

func (iscsi *iscsistorage) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	var err error
	err = nil
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = fmt.Errorf("Recovered from NodeStageVolume: %+v", res)
		}
		if err == nil {
			klog.V(4).Infof("NodeStageVolume completed")
		} else {
			klog.V(4).Infof("NodeStageVolume failed for request %+v", req)
		}
	}()
	klog.V(2).Infof("NodeStageVolume called with publish context: %s", req.GetPublishContext())
	helper.CheckMultipath()

	hostIDString := req.GetPublishContext()["hostID"]
	hostID, err := strconv.Atoi(hostIDString)
	if err != nil {
		err := fmt.Errorf("hostID string '%s' is not valid host ID: %s", hostIDString, err)
		klog.Error(err.Error())
		return nil, status.Error(codes.Internal, err.Error())
	}
	ports := req.GetPublishContext()["hostPorts"]
	hostSecurity := req.GetPublishContext()["securityMethod"]
	useChap := req.GetVolumeContext()["useCHAP"]
	klog.V(4).Infof("Publishing volume to host with hostID %d", hostID)

	// validate host exists
	if hostID < 1 {
		klog.Errorf("hostID %d is not valid host ID", hostID)
		return nil, status.Error(codes.Internal, "not a valid host")
	}
	initiatorName := getInitiatorName()
	if initiatorName == "" {
		msg := "Initiator name not found"
		klog.Errorf(msg)
		return nil, status.Error(codes.Internal, "iscsi: "+msg)
	}
	if !strings.Contains(ports, initiatorName) {
		klog.V(4).Infof("Host port is not created, creating one")
		err = iscsi.cs.AddPortForHost(hostID, "ISCSI", initiatorName)
		if err != nil {
			klog.Errorf("Error creating host port %v", err)
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	klog.V(4).Infof("Setup chap auth as '%s'", useChap)
	if strings.ToLower(hostSecurity) != useChap || !strings.Contains(ports, initiatorName) {
		secrets := req.GetSecrets()
		chapCreds := make(map[string]string)
		if useChap != "none" {
			if useChap == "chap" || useChap == "mutual_chap" {
				if secrets["node.session.auth.username"] != "" && secrets["node.session.auth.password"] != "" {
					chapCreds["security_chap_inbound_username"] = secrets["node.session.auth.username"]
					chapCreds["security_chap_inbound_secret"] = secrets["node.session.auth.password"]
					chapCreds["security_method"] = "CHAP"
				} else {
					msg := "Mutual chap credentials not provided"
					klog.V(4).Infof(msg)
					return nil, status.Error(codes.Internal, "iscsi: "+msg)
				}
			}
			if useChap == "mutual_chap" {
				if secrets["node.session.auth.username_in"] != "" && secrets["node.session.auth.password_in"] != "" && chapCreds["security_method"] == "CHAP" {
					chapCreds["security_chap_outbound_username"] = secrets["node.session.auth.username_in"]
					chapCreds["security_chap_outbound_secret"] = secrets["node.session.auth.password_in"]
					chapCreds["security_method"] = "MUTUAL_CHAP"
				} else {
					msg := "Mutual chap credentials not provided"
					klog.V(4).Infof(msg)
					return nil, status.Error(codes.Internal, "iscsi: "+msg)
				}
			}
			if len(chapCreds) > 1 {
				klog.V(4).Infof("Create chap authentication for host %d", hostID)
				err := iscsi.cs.AddChapSecurityForHost(hostID, chapCreds)
				if err != nil {
					return nil, status.Error(codes.Internal, err.Error())
				}
			}
		} else if hostSecurity != "NONE" {
			klog.V(4).Infof("Remove chap authentication for host %d", hostID)
			chapCreds["security_method"] = "NONE"
			err := iscsi.cs.AddChapSecurityForHost(hostID, chapCreds)
			if err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
		}
	}

	return &csi.NodeStageVolumeResponse{}, nil
}

func (iscsi *iscsistorage) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	var err error
	err = nil
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("iscsi: Recovered from ISCSI NodePublishVolume  " + fmt.Sprint(res))
		}
		if err == nil {
			klog.V(4).Infof("NodePublishVolume completed for volume ID %s", req.GetVolumeId())
		} else {
			klog.V(4).Infof("NodePublishVolume failed for volume ID %s", req.GetVolumeId())
		}
	}()

	klog.V(4).Infof("NodePublishVolume called with volume ID '%s'", req.GetVolumeId())
	klog.V(4).Infof("NodePublishVolume called with request '%+v'", req)
	helper.CheckMultipath()

	iscsiDisk, err := iscsi.getISCSIDisk(req)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	klog.V(4).Infof("iscsiDisk: %v", iscsiDisk)

	diskMounter, err := iscsi.getISCSIDiskMounter(iscsiDisk, req)
	if err != nil {
		return nil, err
	}
	_, err = iscsi.AttachDisk(*diskMounter)
	if err != nil {
		klog.Errorf("AttachDisk failed")
		return nil, status.Error(codes.Internal, err.Error())
	} else {
		klog.V(4).Infof("AttachDisk succeeded")
	}

	// Chown
	uid := req.GetVolumeContext()["uid"] // Returns an empty string if key not found
	gid := req.GetVolumeContext()["gid"]
	targetPath := req.GetTargetPath()
	err = iscsi.osHelper.ChownVolume(uid, gid, targetPath)
	if err != nil {
		msg := fmt.Sprintf("Failed to chown path '%s' : %s", targetPath, err.Error())
		klog.Errorf(msg)
		return nil, status.Error(codes.Internal, msg)
	}

	// Chmod
	unixPermissions := req.GetVolumeContext()["unix_permissions"] // Returns an empty string if key not found
	klog.V(4).Infof("unixPermissions: %s", unixPermissions)
	err = iscsi.osHelper.ChmodVolume(unixPermissions, targetPath)
	if err != nil {
		msg := fmt.Sprintf("Failed to chmod path '%s': %s", targetPath, err)
		klog.Errorf(msg)
		return nil, status.Errorf(codes.Internal, msg)
	}

	helper.PrettyKlogDebug("NodePublishVolume returning csi.NodePublishVolumeResponse:", csi.NodePublishVolumeResponse{})
	return &csi.NodePublishVolumeResponse{}, nil
}

func (iscsi *iscsistorage) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	volumeId := req.GetVolumeId()
	targetPath := req.GetTargetPath()
	var err error = nil

	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("iscsi: Recovered from ISCSI NodeUnpublishVolume  " + fmt.Sprint(res))
		}
		if err == nil {
			klog.V(4).Infof("NodeUnpublishVolume completed for volume ID %s", volumeId)
		} else {
			klog.Errorf("NodeUnpublishVolume failed for volume ID %s", volumeId)
		}
	}()

	klog.V(4).Infof("NodeUnpublishVolume called with volume ID %s and targetPath '%s'", volumeId, targetPath)
	klog.V(4).Infof("NodeUnpublishVolume called with request '%+v'", req)

	err = unmountAndCleanUp(targetPath)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (iscsi *iscsistorage) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (res *csi.NodeUnstageVolumeResponse, err error) {
	err = nil
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("iscsi: Recovered from NodeUnstageVolume  " + fmt.Sprint(res))
		}
		if err == nil {
			klog.V(4).Infof("NodeUnstageVolume completed for volume with ID %s", req.GetVolumeId())
		} else {
			klog.V(4).Infof("NodeUnstageVolume failed for volume with ID %s", req.GetVolumeId())
		}
	}()

	klog.V(4).Infof("NodeUnstageVolume called with volume ID %s", req.GetVolumeId())
	helper.CheckMultipath()

	diskUnmounter := iscsi.getISCSIDiskUnmounter(req.GetVolumeId())
	stagePath := req.GetStagingTargetPath()
	// var bkpPortal []string
	// var volName, iqn, iface, initiatorName, mpathDevice string
	var mpathDevice string

	klog.V(4).Infof("Staging target path: %s", stagePath)

	// Load iscsi disk config from json file
	// diskConfigFound := true
	if err := iscsi.loadDiskInfoFromFile(diskUnmounter.iscsiDisk, stagePath); err == nil {
		klog.V(4).Infof("Successfully loaded disk information from %s", stagePath)
		// bkpPortal = diskUnmounter.iscsiDisk.Portals
		// iqn = diskUnmounter.iscsiDisk.Iqn
		// iface = diskUnmounter.iscsiDisk.Iface
		// volName = diskUnmounter.iscsiDisk.VolName
		// initiatorName = diskUnmounter.iscsiDisk.InitiatorName
		mpathDevice = diskUnmounter.iscsiDisk.MpathDevice
	} else {
		confFile := path.Join("/host", stagePath, diskUnmounter.iscsiDisk.VolName+".json")
		klog.V(4).Infof("Check if config file exists")
		pathExist, pathErr := iscsi.cs.pathExists(confFile)
		if pathErr == nil {
			if !pathExist {
				klog.V(4).Infof("Config file does not exist")
				klog.V(4).Infof("Calling RemoveAll with stagePath %s", stagePath)

				_ = debugWalkDir(stagePath)

				// TODO - Review code
				if err := os.RemoveAll(stagePath); err != nil {
					klog.Warningf("Failed to RemoveAll stage path '%s': %v", stagePath, err)
				}
				klog.V(4).Infof("Removed stage path '%s'", stagePath)
				return &csi.NodeUnstageVolumeResponse{}, nil
			}
		}
		klog.Warningf("Failed to get iscsi config from stage path '%s': %v", stagePath, err)
		// diskConfigFound = false
	}
	err = nil

	//_ = diskConfigFound

	// remove multipath
	protocol := "iscsi"
	err = detachMpathDevice(mpathDevice, protocol)
	if err != nil {
		klog.Warningf("NodeUnstageVolume cannot detach volume with ID %s: %+v", req.GetVolumeId(), err)
	}

	removePath := path.Join("/host", stagePath)
	klog.V(4).Infof("Calling RemoveAll with removePath '%s'", removePath)

	_ = debugWalkDir(removePath)

	// Check if removePath is a directory or a file
	isADir, isADirError := IsDirectory(removePath)
	if isADirError != nil {
		err := fmt.Errorf("Failed to check if removePath '%s' is a directory: %v", removePath, isADirError)
		klog.Errorf(err.Error())
		return nil, err
	}

	// Remove directory contents
	if isADir {
		// removePath '/host/var/lib/kubelet/plugins/kubernetes.io/csi/pv/csi-6e48953803/globalmount'
		// Found path /host/var/lib/kubelet/plugins/kubernetes.io/csi/pv/csi-6e48953803/globalmount
		// Found path /host/var/lib/kubelet/plugins/kubernetes.io/csi/pv/csi-6e48953803/globalmount/93642552.json
		// 93642552.json: {"Portals":["172.31.32.145:3260","172.31.32.146:3260","172.31.32.147:3260","172.31.32.148:3260","172.31.32.149:3260","172.31.32.150:3260"],"Iqn":"iqn.2009-11.com.infinidat:storage:infinibox-sn-1521","Iface":"172.31.32.145:3260","InitiatorName":"iqn.1994-05.com.redhat:462c9b4cda1","VolName":"93642189","MpathDevice":"/dev/dm-8"}

		klog.V(4).Infof("removePath '%s' is a directory", removePath)
		volumeId := strings.Split(req.GetVolumeId(), "$$")[0]
		jsonPath := fmt.Sprintf("%s/%s.json", removePath, volumeId)
		klog.V(4).Infof("Removing json file '%s'", jsonPath)
		if err := os.Remove(jsonPath); err != nil {
			klog.Errorf("Failed to remove json file '%s': %v", jsonPath, err)
			return nil, err
		}
	} else {
		klog.V(4).Infof("removePath '%s' is not a directory", removePath)
	}

	// Remove directory or file
	klog.V(4).Infof("Removing removePath '%s'", removePath)
	if err := os.Remove(removePath); err != nil {
		klog.Errorf("Failed to remove path '%s': %v", removePath, err)
		return nil, err
	}

	// TODO: implement a mechanism for detecting unused iscsi sessions/targets
	// we can't just logout and delete targets because their sessions are shared by multiple volumes,
	// thus we just leave them dangling for now

	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (iscsi *iscsistorage) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_UNKNOWN,
					},
				},
			},
		},
	}, nil
}

func (iscsi *iscsistorage) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	return &csi.NodeGetInfoResponse{}, nil
}

func (iscsi *iscsistorage) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	return nil, status.Error(codes.Unimplemented, time.Now().String())
}

func (iscsi *iscsistorage) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, time.Now().String())
}

func (iscsi *iscsistorage) rescanDeviceMap(volumeId string, lun string) error {
	defer func() {
		klog.V(4).Infof("rescanDeviceMap() with volume %s and lun %s completed", volumeId, lun)
		klog.Flush()
		// deviceMu.Unlock()
		// May happen if unlocking a mutex that was not locked
		if r := recover(); r != nil {
			err := fmt.Errorf("%v", r)
			klog.V(4).Infof("rescanDeviceMap(), with volume ID '%s' and lun '%s', failed with run-time error: %+v", volumeId, lun, err)
		}
	}()

	// deviceMu.Lock()
	klog.V(4).Infof("Rescan hosts for volume %s and lun %s", volumeId, lun)

	// Find hosts. TODO - take heed of portals.
	hostIds, err := execScsi.Command("iscsiadm", fmt.Sprintf("-m session -P3 | awk '{ if (NF > 3 && $1 == \"Host\" && $2 == \"Number:\") printf(\"%%s \", $3) }'"))
	if err != nil {
		klog.Errorf("Finding hosts failed: %s", err)
		return err
	}

	// For each host, scan using lun

	hosts := strings.Fields(hostIds)
	for _, host := range hosts {
		scsiHostPath := fmt.Sprintf("/sys/class/scsi_host/host%s/scan", host)
		_, err = execScsi.Command("echo", fmt.Sprintf("'0 0 %s' > %s", lun, scsiHostPath))
		if err != nil {
			klog.Errorf("Rescan of host %s failed for volume ID %s and lun %s: %s", scsiHostPath, volumeId, lun, err)
			return err
		}
	}

	for _, host := range hosts {
		if err := waitForDeviceState(host, lun, "running"); err != nil {
			return err
		}
	}

	if err := waitForMultipath(hosts[0], lun); err != nil {
		klog.V(4).Infof("Rescan hosts failed for volume ID %s and lun %s", volumeId, lun)
		return err
	}

	klog.V(4).Infof("Rescan hosts complete for volume ID %s and lun %s", volumeId, lun)
	return err
}

func (iscsi *iscsistorage) AttachDisk(b iscsiDiskMounter) (mntPath string, err error) {
	defer func() {
		if res := recover(); res != nil && err == nil {
			msg := fmt.Sprintf("Recovered from AttachDisk: %v", res)
			klog.V(4).Info(msg)
			err = errors.New(msg)
		}
	}()
	var devicePath string
	var devicePaths []string
	var iscsiTransport string
	var lastErr error

	klog.V(2).Infof("Called AttachDisk, disk: %v fsType: %s readOnly: %v mountOpts: %v targetPath: %s stagePath: %s",
		b.iscsiDisk, b.fsType, b.readOnly, b.mountOptions, b.targetPath, b.stagePath)

	klog.V(4).Infof("Check that provided interface '%s' is available", b.Iface)
	isToLogOutput := false
	out, err := execScsi.Command("iscsiadm", fmt.Sprintf("--mode iface --interface %s --op show", b.Iface), isToLogOutput)
	if err != nil {
		klog.Errorf("Cannot read interface %s error: %s", b.Iface, string(out))
		return "", err
	}
	klog.V(4).Infof("Provided interface '%s': ", b.Iface) //, out)

	iscsiTransport = iscsi.extractTransportName(string(out))
	klog.V(4).Infof("iscsiTransport: %s", iscsiTransport)
	if iscsiTransport == "" {
		klog.Errorf("Could not find transport name in iface %s", b.Iface) // TODO - b.Iface here realy should be newIface...or does it matter?
		return "", fmt.Errorf("iscsi: Could not parse iface file for %s", b.Iface)
	}

	bkpPortal := b.Portals
	newIface := bkpPortal[0] // Do not append ':$volume_id'

	// If not found, create new iface and copy parameters from pre-configured (default) iface to the created iface
	// Use one interface per iSCSI network-space, i.e. usually one per IBox.
	// TODO what does a blank Initiator name mean?
	if b.InitiatorName != "" {
		// Look for existing interface named newIface. Clone default iface, if not found.
		klog.V(4).Infof("initiatorName: %s", b.InitiatorName)
		klog.V(4).Infof("Required iface name: '%s'", newIface)
		isToLogOutput := false
		_, err := execScsi.Command("iscsiadm", fmt.Sprintf("--mode iface --interface %s --op show", newIface), isToLogOutput)
		if err != nil {
			klog.V(4).Infof("Creating new iface (clone) and copying parameters from pre-configured iface to it")
			err = iscsi.cloneIface(b, newIface)
			if err != nil {
				klog.Errorf("Failed to clone iface: %s error: %v", b.Iface, err)
				return "", err
			}
			klog.V(4).Infof("New iface created '%s'", newIface)
		} else {
			klog.V(4).Infof("Required iface '%s' already exists", newIface)
		}
		// update iface name. TODO - Seems bad form to change a func param.
		b.Iface = newIface
	} else {
		klog.V(4).Infof("Using existing initiator name'%s'", b.InitiatorName)
	}

	klog.V(4).Infof("Discover targets at portal '%s'", bkpPortal[0])
	// Discover all targets associated with a portal.
	_, err = execScsi.Command("iscsiadm", fmt.Sprintf("--mode discoverydb --type sendtargets --portal %s --discover --op new --op delete", bkpPortal[0]))
	if err != nil {
		msg := fmt.Sprintf("Failed to discover targets at portal '%s': %s ", out, err)
		klog.Errorf(msg)
		err = errors.New(msg)
		return "", fmt.Errorf(msg)
	}

	if !b.chap_session {
		klog.V(4).Infof("target iqn: %s - Not using CHAP", b.Iqn)
	} else {
		// Loop over portals:
		// - Set CHAP usage and update discoverydb with CHAP secret
		for _, tp := range bkpPortal {
			klog.V(4).Infof("target iqn: %s - use CHAP at portal: %s", b.Iqn, tp)
			err = iscsi.updateISCSINode(b, tp)
			if err != nil {
				// failure to update node db is rare. But deleting record will likely impact those who already start using it.
				lastErr = fmt.Errorf("iscsi: Failed to update iscsi node for portal: %s error: %v", tp, err)
				continue
			}
		}
	}

	// Check for at least one session. If none, login.
	klog.V(4).Infof("List sessions to target iqn: %s", b.Iqn)
	sessionString, err := execScsi.Command("iscsiadm", fmt.Sprintf("--mode session | grep '%s'", b.Iqn))
	if err != nil {
		klog.Warningf("Session list failed, err: %s", err)
	}
	if !strings.Contains(sessionString, b.Iqn) {
		klog.V(4).Infof("Login to iscsi target iqn %s at all portals using interface %s", b.Iqn, b.Iface)
		_, err = execScsi.Command("iscsiadm", fmt.Sprintf("--mode node --targetname %s --interface %s --login", b.Iqn, b.Iface))
		if err != nil {
			if status.Code(err) != codes.AlreadyExists {
				msg := fmt.Sprintf("iscsi login failed to target iqn: %s, err: %s", b.Iqn, err)
				klog.Errorf(msg)
				return "", err
			} else {
				err = nil
				klog.Infof("already logged in to target iqn: %s", b.Iqn)
			}
		}
	} else {
		klog.V(4).Infof("Already Logged into iscsi target iqn %s using interface %s", b.Iqn, b.Iface)
	}

	klog.V(4).Infof("List sessions to target iqn: %s", b.Iqn)
	_, err = execScsi.Command("iscsiadm", fmt.Sprintf("--mode session | grep '%s'", b.Iqn))
	if err != nil {
		klog.Errorf("Session list failed, err: %s", err)
		return "", err
	}

	// Rescan for LUN b.lun
	if err := iscsi.rescanDeviceMap(b.VolName, b.lun); err != nil {
		klog.Errorf("rescanDeviceMap failed for volume ID %s and lun %s: %s", b.VolName, b.lun, err)
		return "", err
	}

	for _, tp := range bkpPortal {
		if iscsiTransport == "tcp" {
			devicePath = strings.Join([]string{"/host/dev/disk/by-path/ip", tp, "iscsi", b.Iqn, "lun", b.lun}, "-")
		} else {
			devicePath = strings.Join([]string{"/host/dev/disk/by-path/pci", "*", "ip", tp, "iscsi", b.Iqn, "lun", b.lun}, "-")
		}

		// klog.V(4).Infof("Wait for iscsi device path: %s to appear", devicePath)
		timeout := 10
		if devExists := iscsi.waitForPathToExist(&devicePath, timeout, iscsiTransport); devExists {
			klog.V(2).Infof("iscsi device path found: %s", devicePath)
			devicePaths = append(devicePaths, devicePath)
		} else {
			msg := fmt.Sprintf("failed to attach iqn: %s lun: %s at portal: %s - Timeout after %ds", b.Iqn, b.lun, tp, timeout)
			klog.Errorf(msg)
			// update last error
			lastErr = fmt.Errorf("iscsi: " + msg)
			continue
		}
	}

	if len(devicePaths) == 0 {
		msg := fmt.Sprintf("failed to get any path for iscsi disk, last err seen:\n%v", lastErr)
		klog.Errorf(msg)
		err = errors.New(msg)
		return "", fmt.Errorf(msg)
	}
	if lastErr != nil {
		klog.Errorf("Last error occurred during iscsi init:\n%v", lastErr)
	}

	// Make sure we use a valid devicepath to find mpio device.
	devicePath = devicePaths[0]
	mntPath = b.targetPath
	// Mount device
	notMnt, err := b.mounter.IsLikelyNotMountPoint(mntPath)
	if err == nil {
		if !notMnt {
			klog.V(2).Infof("%s already mounted", mntPath)
			return "", nil
		}
	} else if !os.IsNotExist(err) {
		return "", status.Errorf(codes.Internal, "%s exists but IsLikelyNotMountPoint failed: %v", mntPath, err)
	}

	for _, path := range devicePaths {
		if path == "" {
			continue
		}
		// check if the dev is using mpio and if so mount it via the dm-XX device
		if mappedDevicePath := iscsi.findMultipathDeviceForDevice(path); mappedDevicePath != "" {
			devicePath = mappedDevicePath
			b.iscsiDisk.MpathDevice = mappedDevicePath
			break
		}
	}

	if b.isBlock {
		// A block volume is a volume that will appear as a block device inside the container.
		klog.V(2).Infof("mounting raw block volume at given path %s", mntPath)
		if b.readOnly {
			// TODO: actually implement this - CSIC-343
			return "", status.Error(codes.Internal, "iscsi: Read-only is not supported for Block Volume")
		}

		klog.V(4).Infof("Mount point does not exist. Creating mount point.")
		klog.V(4).Infof("Run: mkdir --parents --mode 0750 '%s' ", filepath.Dir(mntPath))
		// Do not use os.MkdirAll(). This ignores the mount chroot defined in the Dockerfile.
		// MkdirAll() will cause hard-to-grok mount errors.
		cmd := exec.Command("mkdir", "--parents", "--mode", "0750", filepath.Dir(mntPath))
		err = cmd.Run()
		if err != nil {
			klog.Errorf("failed to mkdir '%s': %s", mntPath, err)
			return "", err
		}

		_, err = os.Create("/host/" + mntPath)
		if err != nil {
			klog.Errorf("Failed to create target file %q: %v", mntPath, err)
			return "", fmt.Errorf("iscsi: Failed to create target file for raw block bind mount: %v", err)
		}
		devicePath = strings.Replace(devicePath, "/host", "", 1)

		// TODO: validate this further, see CSIC-341
		options := []string{"bind"}
		options = append(options, "rw") // TODO address in CSIC-343
		if err := b.mounter.Mount(devicePath, mntPath, "", options); err != nil {
			klog.Errorf("Failed to bind mount iscsi block volume %s [%s] to %s, error %v", devicePath, b.fsType, mntPath, err)
			return "", err
		}
		if err := iscsi.createISCSIConfigFile(*(b.iscsiDisk), b.stagePath); err != nil {
			klog.Errorf("Failed to save iscsi config with error: %v", err)
			return "", err
		}
		klog.V(4).Infof("Block volume bind mounted successfully to %s", mntPath)
		return devicePath, err
	} else {
		// A mounted (file) volume is volume that will be mounted using a specified file system
		// and appear as a directory inside the container.
		mountPoint := mntPath
		klog.V(4).Infof("mounting volume %s with filesystem at given path %s", devicePath, mountPoint)

		// Attempt to find a mapper device to use rather than a bare devicePath.
		// If not found, use the devicePath.
		dmsetupInfo, dmsetupErr := dmsetup.Info(devicePath)
		if dmsetupErr != nil {
			klog.Errorf("Failed to get dmsetup info for: '%s', err: %s", devicePath, dmsetupErr)
		} else {
			for i, info := range dmsetupInfo {
				klog.V(4).Infof("dmsetupInfo[%d]: %+v", i, *info)
			}
			if len(dmsetupInfo) == 1 { // One and only one mapper should be in the slice since the devicePath was given.
				mapperPath := devMapperDir + dmsetupInfo[0].Name
				klog.V(2).Infof("Using mapper device: '%s' mapped to path: '%s'", devicePath, mapperPath)
				devicePath = mapperPath
			}
		}

		// Create mountPoint if it does not exist.
		_, err := os.Stat(mountPoint)
		if os.IsNotExist(err) {
			klog.V(4).Infof("Mount point does not exist. Creating mount point.")
			// Do not use os.MkdirAll(). This ignores the mount chroot defined in the Dockerfile.
			// MkdirAll() will cause hard-to-grok mount errors.
			_, err := execScsi.Command("mkdir", fmt.Sprintf("--parents --mode 0750 '%s'", mountPoint))
			if err != nil {
				klog.Errorf("Failed to mkdir '%s': %s", mountPoint, err)
				return "", err
			}
		} else {
			klog.V(4).Infof("mkdir of mountPoint not required. '%s' already exists", mountPoint)
		}

		var options []string
		if b.readOnly {
			klog.V(4).Infof("Volume is read-only")
			options = append(options, "ro") // BUG: what if user separately specified "rw" option? address in CSIC-343
		} else {
			klog.V(4).Infof("Volume is read-write")
			options = append(options, "rw") // BUG: what if user separately specified "ro" option? address in CSIC-343
		}
		options = append(options, b.mountOptions...)

		klog.V(4).Infof("Strip /host from %s", devicePath)
		devicePath = strings.Replace(devicePath, "/host", "", 1)

		helper.CheckMultipath()

		// Persist here so that even if mount fails, the globalmount metadata json
		// file will contain an mpath to use during clean up.
		klog.V(4).Infof("Persist iscsi disk config to json file for later use, when detaching the disk")
		if err = iscsi.createISCSIConfigFile(*(b.iscsiDisk), b.stagePath); err != nil {
			klog.Errorf("Failed to save iscsi config with error: %v", err)
			return "", err
		}

		if b.fsType == "xfs" {
			klog.V(4).Infof("Device %s is of type XFS. Mounting without regard to its XFS UUID.", devicePath)
			options = append(options, "nouuid")
		}

		klog.V(4).Infof("Format '%s' (if needed) and mount volume", devicePath)
		err = b.mounter.FormatAndMount(devicePath, mountPoint, b.fsType, options)
		klog.V(4).Infof("FormatAndMount returned: %+v", err)
		if err != nil {
			searchAlreadyMounted := fmt.Sprintf("already mounted on %s", mountPoint)
			klog.V(4).Infof("Search error for matches to handle: %+v", err)

			if isAlreadyMounted := strings.Contains(err.Error(), searchAlreadyMounted); isAlreadyMounted {
				klog.Errorf("Device %s is already mounted on %s", devicePath, mountPoint)
			} else {
				klog.Errorf("Failed to mount iscsi volume %s [%s] to %s, error %+v", devicePath, b.fsType, mountPoint, err)
				_, _ = mountPathExists(mountPoint)
				return "", err
			}
		} else {
			klog.V(4).Infof("FormatAndMount err is nil")
		}
	}
	klog.V(4).Infof("Mounted volume with device path %s successfully at '%s'", devicePath, mntPath)
	return devicePath, nil
}

func mountPathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		klog.V(4).Infof("mountPathExists: Path " + path + " exists")
		return true, nil
	}
	if os.IsNotExist(err) {
		klog.V(4).Infof("mountPathExists: Path " + path + " does not exist")
		return false, nil
	}
	return false, err
}

func portalMounter(portal string) string {
	if !strings.Contains(portal, ":") {
		portal = portal + ":3260"
	}
	return portal
}

func getInitiatorName() string {
	var err error
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("iscsi: Recovered from ISCSI getInitiatorName  " + fmt.Sprint(res))
		}
	}()
	cmd := "cat /etc/iscsi/initiatorname.iscsi | grep InitiatorName="
	out, err := exec.Command("bash", "-c", cmd).Output()
	if err != nil {
		klog.Errorf("Failed to get initiator name. Is iSCSI initiator installed? Error: %v", err)
		return ""
	}
	initiatorName := string(out)
	initiatorName = strings.TrimSuffix(initiatorName, "\n")
	klog.V(4).Infof("Host initiator name %s ", initiatorName)
	arr := strings.Split(initiatorName, "=")
	return arr[1]
}

func (iscsi *iscsistorage) getISCSIDisk(req *csi.NodePublishVolumeRequest) (*iscsiDisk, error) {
	var err error
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("iscsi: Recovered from ISCSI getISCSIDisk  " + fmt.Sprint(res))
		}
	}()
	klog.V(4).Infof("Called getISCSIDisk")
	initiatorName := getInitiatorName()

	volproto := strings.Split(req.GetVolumeId(), "$$")
	volName := volproto[0]

	volContext := req.GetVolumeContext()
	publishContext := req.GetPublishContext()
	klog.V(4).Infof("volume: %s context: %v publish context: %v", volName, volContext, publishContext)

	iqn := volContext["iqn"]
	if iqn == "" {
		return nil, fmt.Errorf("iscsi: target iqn is missing")
	}

	portals := volContext["portals"]
	portalList := strings.Split(portals, ",")
	if len(portalList) == 0 {
		return nil, fmt.Errorf("iscsi: target portals list is missing")
	}
	bkportal := []string{}
	for _, portal := range portalList {
		bkportal = append(bkportal, portalMounter(string(portal)))
	}

	lun := publishContext["lun"]
	if lun == "" {
		return nil, fmt.Errorf("iscsi: LUN is missing")
	}

	useChap := volContext["useCHAP"]
	chapSession := false
	if useChap != "none" {
		chapSession = true
	}
	chapDiscovery := false
	if volContext["discoveryCHAPAuth"] == "true" {
		chapDiscovery = true
	}
	secret := req.GetSecrets()
	if chapSession {
		secret, err = iscsi.parseSessionSecret(useChap, secret)
		// secret["node.session.auth.username"] = initiatorName
		if err != nil {
			klog.V(4).Infof("iscsi.parseSessionSecret failed")
			return nil, err
		}
	}

	return &iscsiDisk{
		VolName:        volName,
		Portals:        bkportal,
		Iqn:            iqn,
		lun:            lun,
		Iface:          "default",
		chap_discovery: chapDiscovery,
		chap_session:   chapSession,
		secret:         secret,
		InitiatorName:  initiatorName,
	}, nil
}

func (iscsi *iscsistorage) getISCSIDiskMounter(iscsiDisk *iscsiDisk, req *csi.NodePublishVolumeRequest) (*iscsiDiskMounter, error) {
	// handle volumeCapabilities, the standard place to define block/file etc
	reqVolCapability := req.GetVolumeCapability()

	// check accessMode - where we will eventually police R/W etc (CSIC-343)
	accessMode := reqVolCapability.GetAccessMode().GetMode() // GetAccessMode() guaranteed not nil from controller.go
	// TODO: set readonly flag for RO accessmodes, any other validations needed?

	// handle file (mount) and block parameters
	mountVolCapability := reqVolCapability.GetMount()
	fstype := ""
	mountOptions := []string{}
	blockVolCapability := reqVolCapability.GetBlock()

	// LEGACY MITIGATION: accept but warn about old opaque fstype parameter if present - remove in the future with CSIC-344
	fstype, oldFstypeParamProvided := req.GetVolumeContext()["fstype"]
	if oldFstypeParamProvided {
		klog.Warningf("Deprecated 'fstype' parameter %s provided, will NOT be supported in future releases - please move to 'csi.storage.k8s.io/fstype'", fstype)
	}

	// protocol-specific paths below
	if mountVolCapability != nil && blockVolCapability == nil {
		// option A. user wants file access to their iSCSI device
		iscsiDisk.isBlock = false

		// filesystem type and reconciliation with older nonstandard param
		// LEGACY MITIGATION: remove !oldFstypeParamProvided in the future with CSIC-344
		if mountVolCapability.GetFsType() != "" {
			fstype = mountVolCapability.GetFsType()
		} else if !oldFstypeParamProvided {
			errMsg := "No fstype in VolumeCapability for volume: " + req.GetVolumeId()
			klog.Errorf(errMsg)
			return nil, status.Error(codes.InvalidArgument, errMsg)
		}

		// mountOptions - could be nothing
		mountOptions = mountVolCapability.GetMountFlags()

		// TODO: other validations needed for file?
		// - something about read-only access?
		// - check that fstype is supported?
		// - check that mount options are valid for fstype provided

	} else if mountVolCapability == nil && blockVolCapability != nil {
		// option B. user wants block access to their iSCSI device
		iscsiDisk.isBlock = true

		if accessMode == csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER {
			klog.Warning("MULTI_NODE_MULTI_WRITER AccessMode requested for raw block volume, could be dangerous")
		}
		// TODO: something about SINGLE_NODE_MULTI_WRITER (alpha feature) as well?

		// don't need to look at FsType or MountFlags here, only relevant for mountVol
		// TODO: other validations needed for block?
		// - something about read-only access?
	} else {
		errMsg := "Bad VolumeCapability parameters: both block and mount modes, for volume: " + req.GetVolumeId()
		klog.Errorf(errMsg)
		return nil, status.Error(codes.InvalidArgument, errMsg)
	}

	return &iscsiDiskMounter{
		iscsiDisk:    iscsiDisk,
		fsType:       fstype,
		readOnly:     false, // TODO: not accurate, address in CSIC-343
		mountOptions: mountOptions,
		mounter:      &mount.SafeFormatAndMount{Interface: mount.New(""), Exec: utilexec.New()},
		exec:         utilexec.New(),
		targetPath:   req.GetTargetPath(),
		stagePath:    req.GetStagingTargetPath(),
		deviceUtil:   util.NewDeviceHandler(util.NewIOHandler()),
	}, nil
}

func (iscsi *iscsistorage) getISCSIDiskUnmounter(volumeID string) *iscsiDiskUnmounter {
	volproto := strings.Split(volumeID, "$$")
	volName := volproto[0]
	return &iscsiDiskUnmounter{
		iscsiDisk: &iscsiDisk{
			VolName: volName,
		},
		mounter: mount.New(""),
		exec:    utilexec.New(),
	}
}

func (iscsi *iscsistorage) parseSessionSecret(useChap string, secretParams map[string]string) (map[string]string, error) {
	var ok bool
	secret := make(map[string]string)

	if useChap == "chap" || useChap == "mutual_chap" {
		if len(secretParams) == 0 {
			return secret, errors.New("iscsi: required chap secrets not provided")
		}
		if secret["node.session.auth.username"], ok = secretParams["node.session.auth.username"]; !ok {
			return secret, fmt.Errorf("iscsi: node.session.auth.username not found in secret")
		}
		if secret["node.session.auth.password"], ok = secretParams["node.session.auth.password"]; !ok {
			return secret, fmt.Errorf("iscsi: node.session.auth.password not found in secret")
		}
		if useChap == "mutual_chap" {
			if secret["node.session.auth.username_in"], ok = secretParams["node.session.auth.username_in"]; !ok {
				return secret, fmt.Errorf("iscsi: node.session.auth.username_in not found in secret")
			}
			if secret["node.session.auth.password_in"], ok = secretParams["node.session.auth.password_in"]; !ok {
				return secret, fmt.Errorf("iscsi: node.session.auth.password_in not found in secret")
			}
		}
		secret["SecretsType"] = "chap"
	}
	return secret, nil
}

func (iscsi *iscsistorage) updateISCSINode(b iscsiDiskMounter, tp string) error {
	if !b.chap_session {
		return nil
	}

	klog.V(4).Infof("Update node with CHAP")
	out, err := execScsi.Command("iscsiadm", fmt.Sprintf("--mode node --portal %s --targetname %s --interface %s --op update --name node.session.auth.authmethod --value CHAP", tp, b.Iqn, b.Iface))
	if err != nil {
		return fmt.Errorf("iscsi: Failed to update node with CHAP, output: %v", string(out))
	}

	for _, k := range chap_sess {
		v := b.secret[k]
		if len(v) > 0 {
			klog.V(4).Infof("Update node session key/value")
			out, err := execScsi.Command("iscsiadm", fmt.Sprintf("--mode node --portal %s --targetname %s --interface %s --op update --name %q --value %q", tp, b.Iqn, b.Iface, k, v))
			if err != nil {
				return fmt.Errorf("iscsi: Failed to update node session key %q with value %q error: %v", k, v, string(out))
			}
		}
	}
	return nil
}

func (iscsi *iscsistorage) waitForPathToExist(devicePath *string, maxRetries int, deviceTransport string) bool {
	// This makes unit testing a lot easier
	return iscsi.waitForPathToExistInternal(devicePath, maxRetries, deviceTransport, os.Stat, filepath.Glob)
}

func (iscsi *iscsistorage) waitForPathToExistInternal(devicePath *string, maxRetries int, deviceTransport string, osStat StatFunc, filepathGlob GlobFunc) bool {
	if devicePath == nil {
		return false
	}

	for i := 0; i < maxRetries; i++ {
		var err error

		if deviceTransport == "tcp" {
			_, err = os.Stat(*devicePath)
		} else {
			fpath, _ := filepathGlob(*devicePath)
			if fpath == nil {
				err = os.ErrNotExist
			} else {
				// There might be a case that fpath contains multiple device paths if
				// multiple PCI devices connect to same iscsi target. We handle this
				// case at subsequent logic. Pick up only first path here.
				*devicePath = fpath[0]
			}
		}

		if err == nil {
			klog.V(4).Infof("Device path %s exists", *devicePath)
			return true
		}
		klog.V(4).Infof("Device path %s does not exist: %v", *devicePath, err)
		if !errors.Is(err, os.ErrNotExist) {
			// os.Stat did not return ErrNotExist, so we assume there is a problem anyway
			return false
		}
		if i == maxRetries-1 {
			break
		}
		time.Sleep(time.Second)
	}
	klog.Errorf("Timed out waiting for device path %s to exist", *devicePath)
	return false
}

func (iscsi *iscsistorage) createISCSIConfigFile(conf iscsiDisk, mnt string) error {
	file := path.Join("/host", mnt, conf.VolName+".json")
	klog.V(4).Infof("persistISCSI: Creating persist config file at path %s", file)
	fp, err := os.Create(file)
	if err != nil {
		klog.Errorf("persistISCSI: Failed creating persist config file with error %v", err)
		return fmt.Errorf("iscsi: Create %s err %s", file, err)
	}
	defer fp.Close()
	encoder := json.NewEncoder(fp)
	if err = encoder.Encode(conf); err != nil {
		klog.Errorf("iscsi: persistISCSI: Failed creating persist config file with error %v", err)
		return fmt.Errorf("iscsi: Encode err: %v", err)
	}
	return nil
}

func (iscsi *iscsistorage) loadDiskInfoFromFile(conf *iscsiDisk, mnt string) error {
	file := path.Join("/host", mnt, conf.VolName+".json")
	fp, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("iscsi: Open %s err %s", file, err)
	}
	defer fp.Close()
	decoder := json.NewDecoder(fp)
	if err = decoder.Decode(conf); err != nil {
		return fmt.Errorf("iscsi: Decode err: %v ", err)
	}
	return nil
}

func (iscsi *iscsistorage) extractTransportName(ifaceOutput string) (iscsiTransport string) {
	rexOutput := ifaceTransportNameRe.FindStringSubmatch(ifaceOutput)
	if rexOutput == nil {
		return ""
	}
	iscsiTransport = rexOutput[1]

	// While iface.transport_name is a required parameter, handle it being unspecified anyways
	if iscsiTransport == "<empty>" {
		iscsiTransport = "tcp"
	}
	return iscsiTransport
}

func (iscsi *iscsistorage) parseIscsiadmShow(output string) (map[string]string, error) {
	params := make(map[string]string)
	slice := strings.Split(output, "\n")
	for _, line := range slice {
		if !strings.HasPrefix(line, "iface.") || strings.Contains(line, "<empty>") {
			continue
		}
		iface := strings.Fields(line)
		if len(iface) != 3 || iface[1] != "=" {
			return nil, fmt.Errorf("iscsi: Invalid iface setting: %v", iface)
		}
		// iscsi_ifacename is immutable once the iface is created
		if iface[0] == "iface.iscsi_ifacename" {
			continue
		}
		params[iface[0]] = iface[2]
	}
	return params, nil
}

func (iscsi *iscsistorage) cloneIface(b iscsiDiskMounter, newIface string) error {
	var lastErr error
	klog.V(4).Infof("Find pre-configured iface records")
	out, err := execScsi.Command("iscsiadm", fmt.Sprintf("--mode iface --interface %s --op show", b.Iface))
	if err != nil {
		lastErr = fmt.Errorf("iscsi: Failed to show iface records: %s (%v)", string(out), err)
		return lastErr
	}
	klog.V(4).Infof("Pre-configured iface records found: %s", out)

	// parse obtained records
	params, err := iscsi.parseIscsiadmShow(string(out))
	if err != nil {
		lastErr = fmt.Errorf("iscsi: Failed to parse iface records: %s (%v)", string(out), err)
		return lastErr
	}
	// update initiatorname
	params["iface.initiatorname"] = b.InitiatorName

	klog.V(4).Infof("Create new interface")
	out, err = execScsi.Command("iscsiadm", fmt.Sprintf("--mode iface --interface %s --op new", newIface))
	if err != nil {
		lastErr = fmt.Errorf("iscsi: Failed to create new iface: %s (%v)", string(out), err)
		return lastErr
	}

	// update new iface records
	for key, val := range params {
		klog.V(4).Infof("Update records of interface '%s'", newIface)
		_, err = execScsi.Command("iscsiadm", fmt.Sprintf("--mode iface --interface %s --op update --name %q --value %q", newIface, key, val))
		if err != nil {
			klog.V(4).Infof("Failed to update records of interface '%s'", newIface)
			_, err := execScsi.Command("iscsiadm", fmt.Sprintf("--mode iface --interface %s --op delete", newIface))
			if err != nil {
				lastErr = fmt.Errorf("Failed to delete iface '%s': %s ", newIface, err)
				return lastErr
			}

			lastErr = fmt.Errorf("iscsi: Failed to update iface records: %s (%v). iface(%s) will be used", string(out), err, b.Iface)
			break
		}
	}
	return lastErr
}

// FindMultipathDeviceForDevice given a device name like /dev/sdx, find the devicemapper parent
func (iscsi *iscsistorage) findMultipathDeviceForDevice(device string) string {
	disk, err := findDeviceForPath(device)
	if err != nil {
		return ""
	}
	// TODO: Does this need a /host prefix?
	sysPath := "/sys/block/"
	if dirs, err := ioutil.ReadDir(sysPath); err == nil {
		for _, f := range dirs {
			name := f.Name()
			if strings.HasPrefix(name, "dm-") {
				if _, err1 := os.Lstat(sysPath + name + "/slaves/" + disk); err1 == nil {
					return "/dev/" + name
				}
			}
		}
	} else {
		klog.Errorf("Failed to find multipath device with error %v", err)
	}
	return ""
}

func findDeviceForPath(path string) (string, error) {
	devicePath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	// if path /dev/hdX split into "", "dev", "hdX" then we will
	// return just the last part
	devicePath = strings.Replace(devicePath, "/host", "", 1)
	parts := strings.Split(devicePath, "/")
	if len(parts) == 3 && strings.HasPrefix(parts[1], "dev") {
		return parts[2], nil
	}
	return "", errors.New("iscsi: Illegal path for device " + devicePath)
}

// Flush a multipath device map for device.
/**
func multipathFlush(mpath string) {
	klog.V(4).Infof("Running multipath -f '%s'", mpath)

	isToLogOutput := true
	if out, err := execScsi.Command("multipath", fmt.Sprintf("-f %s", mpath), isToLogOutput); err != nil {
		klog.Errorf("multipath -f '%s' failed - ignored: %s", mpath, err)
	} else {
		klog.V(4).Infof("multipath -f '%s' succeeded: %s", mpath, out)
	}

	_, _ = execScsi.Command("ls", "-l /host/dev/mapper/*; echo", isToLogOutput)
	_, _ = execScsi.Command("ls", "/host/dev/sd*; echo", isToLogOutput)
}
*/

// Given a device like '/dev/dm-0', find its matching multipath name such as 'mpathab'.
func findMpathFromDevice(device string) (mpath string, err error) {
	deviceName := strings.Replace(device, "/dev/", "", 1)
	command := fmt.Sprintf("multipath -l | grep --word-regexp %s | awk '{print $1}'", deviceName)
	pipefailCmd := fmt.Sprintf("set -o pipefail; %s", command)

	out, err := exec.Command("bash", "-c", pipefailCmd).CombinedOutput()
	mpath = strings.TrimSpace(string(out))

	if err != nil {
		msg := fmt.Sprintf("Cannot findMpathFromDevice: %s, Error: %s: %v\n", device, mpath, err)
		klog.Error(msg)
		err = errors.New(msg)
		return
	}
	klog.V(4).Infof("Device %s corresponds to multipath %s", device, mpath)
	return
}

// Used for debugging. Log a path, found by debugWalkDir, to klog.
func debugLogPath(path string, info os.FileInfo, err error) error {
	if err != nil {
		klog.Errorf(err.Error())
		return err
	}
	klog.V(4).Infof("Found path %s", path)
	return nil
}

// Used for debugging. For given walk_path, log all files found within.
func debugWalkDir(walk_path string) (err error) {
	klog.V(4).Infof("debugWalkDir called with walk_path %s", walk_path)
	err = filepath.Walk(walk_path, debugLogPath)
	if err != nil {
		klog.V(4).Infof("debugWalkDir failed: %s", err.Error())
		return err
	}
	return nil
}
