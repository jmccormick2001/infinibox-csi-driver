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
	"encoding/json"
	"errors"
	"fmt"
	"infinibox-csi-driver/api"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	log "infinibox-csi-driver/helper/logger"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"k8s.io/klog"
	"k8s.io/mount-utils"
)

const (
	// StoragePoolKey : pool to be used
	StoragePoolKey = "pool_name"

	// MinVolumeSize : volume will be created with this size if requested volume size is less than this values
	MinVolumeSize = 1 * bytesofGiB

	bytesofKiB = 1024

	kiBytesofGiB = 1024 * 1024

	bytesofGiB = kiBytesofGiB * bytesofKiB
)

func isMountedByListMethod(targetHostPath string) (bool, error) {
	// Use List() to search for mount matching targetHostPath
	// Each mount in the list has this example form:
	// {/dev/mapper/mpathn /host/var/lib/kubelet/pods/d2f8fcf0-f816-4008-b8fe-5d5f16c854d0/volumes/kubernetes.io~csi/csi-f581f6711d/mount xfs [rw seclabel relatime nouuid attr2 inode64 logbufs=8 logbsize=64k sunit=128 swidth=2048 noquota] 0 0}
	//
	// type MountPoint struct {
	//    Device string
	//    Path   string
	//    Type   string
	//    Opts   []string // Opts may contain sensitive mount options (like passwords) and MUST be treated as such (e|        .g. not logged).
	//    Freq   int
	//    Pass   int
	// }

	klog.V(4).Infof("Checking mount path using mounter's List() and searching with path '%s'", targetHostPath)
	mounter := mount.New("")
	mountList, mountListErr := mounter.List()
	if mountListErr != nil {
		err := fmt.Errorf("Failed List: %+v", mountListErr)
		klog.Errorf(err.Error())
		return true, err
	}
	klog.V(5).Infof("Mount path list: %v", mountList)

	// Search list for targetHostPath
	isMountedByListMethod := false
	for i := range mountList {
		if mountList[i].Path == targetHostPath {
			isMountedByListMethod = true
			break
		}
	}
	klog.V(4).Infof("Path '%s' is mounted: %t", targetHostPath, isMountedByListMethod)
	return isMountedByListMethod, nil
}

func cleanupOldMountDirectory(targetHostPath string) error {
	klog.V(4).Infof("Cleaning up old mount directory at '%s'", targetHostPath)
	isMountEmpty, isMountEmptyErr := IsDirEmpty(targetHostPath)
	// Verify mount/ directory is empty. Fail if mount/ is not empty as that may be volume data.
	if isMountEmptyErr != nil {
		err := fmt.Errorf("Failed IsDirEmpty() using targetHostPath '%s': %v", targetHostPath, isMountEmptyErr)
		klog.Errorf(err.Error())
		return err
	}
	if !isMountEmpty {
		err := fmt.Errorf("Error: mount/ directory at targetHostPath '%s' is not empty and may contain volume data", targetHostPath)
		klog.Errorf(err.Error())
		return err
	}
	klog.V(4).Infof("Verified that targetHostPath directory '%s', aka mount path, is empty of files", targetHostPath)

	// Clean up mount/
	if _, statErr := os.Stat(targetHostPath); os.IsNotExist(statErr) {
		klog.V(4).Infof("Mount point targetHostPath '%s' already removed", targetHostPath)
	} else {
		klog.V(4).Infof("Removing mount point targetHostPath '%s'", targetHostPath)
		if removeMountErr := os.Remove(targetHostPath); removeMountErr != nil {
			err := fmt.Errorf("After unmounting, failed to Remove() path '%s': %v", targetHostPath, removeMountErr)
			klog.Errorf(err.Error())
			return err
		}
	}
	klog.V(4).Infof("Removed mount point targetHostPath '%s'", targetHostPath)

	csiHostPath := strings.TrimSuffix(targetHostPath, "/mount")
	volData := "vol_data.json"
	volDataPath := filepath.Join(csiHostPath, volData)

	// Clean up csi-NNNNNNN/vol_data.json file
	if _, statErr := os.Stat(volDataPath); os.IsNotExist(statErr) {
		klog.V(4).Infof("%s already removed from path '%s'", volData, csiHostPath)
	} else {
		klog.V(4).Infof("Removing %s from path '%s'", volData, volDataPath)
		if err := os.Remove(volDataPath); err != nil {
			klog.Warningf("After unmounting, failed to remove %s from path '%s': %v", volData, volDataPath, err)
		}
		klog.V(4).Infof("Successfully removed %s from path '%s'", volData, volDataPath)
	}

	// Clean up csi-NNNNNNN directory
	if _, statErr := os.Stat(csiHostPath); os.IsNotExist(statErr) {
		klog.V(4).Infof("CSI volume directory '%s' already removed", csiHostPath)
	} else {
		klog.V(4).Infof("Removing CSI volume directory '%s'", csiHostPath)
		if err := os.Remove(csiHostPath); err != nil {
			klog.Errorf("After unmounting, failed to remove CSI volume directory '%s': %v", csiHostPath, err)
		}
		klog.V(4).Infof("Successfully removed CSI volume directory'%s'", csiHostPath)
	}
	return nil
}

// Unmount using targetPath and cleanup directories and files.
func unmountAndCleanUp(targetPath string) (err error) {
	klog.V(2).Infof("Unmounting and cleaning up pathf for targetPath '%s'", targetPath)
	defer func() {
		if res := recover(); res != nil && err == nil {
			err = errors.New("Recovered from unmountAndCleanUp  " + fmt.Sprint(res))
		}
	}()

	mounter := mount.New("")
	targetHostPath := path.Join("/host", targetPath)

	klog.V(4).Infof("Unmounting targetPath '%s'", targetPath)
	if err := mounter.Unmount(targetPath); err != nil {
		klog.Warningf("Failed to unmount targetPath '%s' but rechecking: %v", targetPath, err)
	} else {
		klog.V(4).Infof("Successfully unmounted targetPath '%s'", targetPath)
	}

	isMounted, isMountedErr := isMountedByListMethod(targetHostPath)
	if isMountedErr != nil {
		err := fmt.Errorf("Error: Failed to check if targetHostPath '%s' is unmounted after unmounting", targetHostPath)
		klog.Errorf(err.Error())
		return err
	}
	if isMounted {
		// TODO - Should include volume ID
		err := fmt.Errorf("Error: Volume remains mounted at targetHostPath '%s'", targetHostPath)
		klog.Errorf(err.Error())
		return err
	}
	klog.V(4).Infof("Verified that targetHostPath '%s' is not mounted", targetHostPath)

	// Check if targetHostPath exists
	if _, err := os.Stat(targetHostPath); os.IsNotExist(err) {
		klog.V(4).Infof("targetHostPath '%s' does not exist and does not need to be cleaned up", targetHostPath)
		return nil
	}

	// Check if targetHostPath is a directory or a file
	isADir, isADirError := IsDirectory(targetHostPath)
	if isADirError != nil {
		err := fmt.Errorf("Failed to check if targetHostPath '%s' is a directory: %v", targetHostPath, isADirError)
		klog.Errorf(err.Error())
		return err
	}

	if isADir {
		klog.V(4).Infof("targetHostPath '%s' is a directory, not a file", targetHostPath)
		if err := cleanupOldMountDirectory(targetHostPath); err != nil {
			return err
		}
		klog.V(4).Infof("Successfully cleaned up directory based targetHostPath '%s'", targetHostPath)
	} else {
		// TODO - Could check this is a file using IsDirectory().
		klog.V(4).Infof("targetHostPath '%s' is a file, not a directory", targetHostPath)
		if removeMountErr := os.Remove(targetHostPath); removeMountErr != nil {
			err := fmt.Errorf("Failed to Remove() path '%s': %v", targetHostPath, removeMountErr)
			klog.Errorf(err.Error())
			return err
		}
		klog.V(4).Infof("Successfully cleaned up file based targetHostPath '%s'", targetHostPath)
	}

	return nil
}

func verifyVolumeSize(caprange *csi.CapacityRange) (int64, error) {
	requiredVolSize := int64(caprange.GetRequiredBytes())
	allowedMaxVolSize := int64(caprange.GetLimitBytes())
	if requiredVolSize < 0 || allowedMaxVolSize < 0 {
		return 0, errors.New("not valid volume size")
	}

	if requiredVolSize == 0 {
		requiredVolSize = MinVolumeSize
	}

	var (
		sizeinGB   int64
		sizeinByte int64
	)

	sizeinGB = requiredVolSize / bytesofGiB
	if sizeinGB == 0 {
		log.Warn("Volumen Minimum capacity should be greater 1 GB")
		sizeinGB = 1
	}

	sizeinByte = sizeinGB * bytesofGiB
	if allowedMaxVolSize != 0 {
		if sizeinByte > allowedMaxVolSize {
			return 0, errors.New("volume size is out of allowed limit")
		}
	}

	return sizeinByte, nil
}

func validateStorageClassParameters(requiredStorageClassParams map[string]string, providedStorageClassParams map[string]string) error {
	// Loop through and check required parameters only, consciously ignore parameters that aren't required
	badParamsMap := make(map[string]string)
	for param, required_regex := range requiredStorageClassParams {
		if param_value, ok := providedStorageClassParams[param]; ok {
			if matched, _ := regexp.MatchString(required_regex, param_value); !matched {
				badParamsMap[param] = "Required input parameter " + param_value + " didn't match expected pattern " + required_regex
			}
		} else {
			badParamsMap[param] = "Parameter required but not provided"
		}
	}

	if len(badParamsMap) > 0 {
		klog.Errorf("Invalid StorageClass parameters provided: %s", badParamsMap)
		return fmt.Errorf("Invalid StorageClass parameters provided: %s", badParamsMap)
	}

	return nil
}

func copyRequestParameters(parameters, out map[string]string) {
	for key, val := range parameters {
		if val != "" {
			out[key] = val
			klog.V(2).Infof("%s: %s", key, val)
		} else {
			klog.V(2).Infof("%s: empty", key)
		}
	}
}

func validateVolumeID(str string) (volprotoconf api.VolumeProtocolConfig, err error) {
	volproto := strings.Split(str, "$$")
	if len(volproto) != 2 {
		return volprotoconf, errors.New("volume Id and other details not found")
	}
	volprotoconf.VolumeID = volproto[0]
	volprotoconf.StorageType = volproto[1]
	return volprotoconf, nil
}

func getPermissionMaps(permission string) ([]map[string]interface{}, error) {
	permissionFixed := strings.Replace(permission, "'", "\"", -1)
	var permissionsMapArray []map[string]interface{}
	err := json.Unmarshal([]byte(permissionFixed), &permissionsMapArray)
	if err != nil {
		klog.Errorf("invalid nfs_export_permissions format %v", err)
	}

	for _, pass := range permissionsMapArray {
		no_root_squash_str, ok := pass["no_root_squash"].(string)
		if ok {
			rootsq, err := strconv.ParseBool(no_root_squash_str)
			if err != nil {
				klog.V(4).Infof("failed to cast no_root_squash value in export permission - setting default value 'true'")
				rootsq = true
			}
			pass["no_root_squash"] = rootsq
		}
	}
	return permissionsMapArray, err
}

// Check if a directory is empty.
// Return an isEmpty boolean and an error.
func IsDirEmpty(name string) (bool, error) {
	f, err := os.Open(name)
	if err != nil {
		return false, err
	}
	defer f.Close()

	_, err = f.Readdirnames(1) // Or f.Readdir(1)
	if err == io.EOF {
		return true, nil
	}
	return false, err // Either not empty or error, suits both cases
}

// Determine if a file represented
// by `path` is a directory or not.
func IsDirectory(path string) (bool, error) {
    fileInfo, err := os.Stat(path)
    if err != nil {
        return false, err
    }

    return fileInfo.IsDir(), err
}
