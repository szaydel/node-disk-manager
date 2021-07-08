/*
Copyright 2020 The OpenEBS Authors

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

package probe

import (
	"fmt"

	apis "github.com/openebs/node-disk-manager/api/v1alpha1"
	"github.com/openebs/node-disk-manager/blockdevice"
	"github.com/openebs/node-disk-manager/db/kubernetes"
	"github.com/openebs/node-disk-manager/pkg/partition"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog"
)

const (
	internalUUIDSchemeAnnotation    = "internal.openebs.io/uuid-scheme"
	legacyUUIDScheme                = "legacy"
	gptUUIDScheme                   = "gpt"
	internalFSUUIDAnnotation        = "internal.openebs.io/fsuuid"
	internalPartitionUUIDAnnotation = "internal.openebs.io/partition-uuid"
)

// addBlockDeviceToHierarchyCache adds the given block device to the hierarchy of devices.
// returns true if the device already existed in the cache. Else returns false
func (pe *ProbeEvent) addBlockDeviceToHierarchyCache(bd blockdevice.BlockDevice) bool {
	var deviceAlreadyExistsInCache bool
	// check if the device already exists in the cache
	_, ok := pe.Controller.BDHierarchy[bd.DevPath]
	if ok {
		klog.V(4).Infof("device: %s already exists in cache, "+
			"the event was likely generated by a partition table re-read", bd.DevPath)
		deviceAlreadyExistsInCache = true
	}
	if !ok {
		klog.V(4).Infof("device: %s does not exist in cache, "+
			"the device is now connected to this node", bd.DevPath)
		deviceAlreadyExistsInCache = false
	}

	// in either case, whether it existed or not, we will update with the latest BD into the cache
	pe.Controller.BDHierarchy[bd.DevPath] = bd
	return deviceAlreadyExistsInCache
}

// addBlockDevice processed when an add event is received for a device
func (pe *ProbeEvent) addBlockDevice(bd blockdevice.BlockDevice, bdAPIList *apis.BlockDeviceList) error {

	// handle devices that are not managed by NDM
	// eg:devices in use by mayastor, zfs PV and jiva
	// TODO jiva handling is still to be added.
	if ok, err := pe.handleUnmanagedDevices(bd, bdAPIList); err != nil {
		klog.Errorf("error handling unmanaged device %s. error: %v", bd.DevPath, err)
		return err
	} else if !ok {
		klog.V(4).Infof("processed device: %s being used by mayastor/zfs-localPV", bd.DevPath)
		return nil
	}

	// if parent device in use, no need to process further
	if ok, err := pe.isParentDeviceInUse(bd); err != nil {
		klog.Error(err)
		return err
	} else if ok {
		klog.Infof("parent device of device: %s in use", bd.DevPath)
		return nil
	}

	// upgrades the devices that are in use and used the legacy method
	// for uuid generation.
	if ok, err := pe.upgradeBD(bd, bdAPIList); err != nil {
		klog.Errorf("upgrade of device: %s failed. Error: %v", bd.DevPath, err)
		return err
	} else if !ok {
		klog.V(4).Infof("device: %s upgraded", bd.DevPath)
		return nil
	}

	/*
		Cases when an add event is generated
		1. A new disk is added to the cluster to this node -  the disk is first time in this cluster
		2. A new disk is added to this node -  the disk was already present in the cluster and it was moved to this node
		3. A disk was detached and reconnected to this node
		4. An add event due to partition table reread . This may cause events to be generated without the disk
			being physically removed this node - (when a new partition is created on the device also, its the same case)
	*/

	// check if the disk can be uniquely identified. we try to generate the UUID for the device
	klog.V(4).Infof("checking if device: %s can be uniquely identified", bd.DevPath)
	uuid, ok := generateUUID(bd)
	// if UUID cannot be generated create a GPT partition on the device
	if !ok {
		klog.V(4).Infof("device: %s cannot be uniquely identified", bd.DevPath)
		if len(bd.DependentDevices.Partitions) > 0 ||
			len(bd.DependentDevices.Holders) > 0 {
			klog.V(4).Infof("device: %s has holders/partitions. %+v", bd.DevPath, bd.DependentDevices)
		} else {
			klog.Infof("starting to create partition on device: %s", bd.DevPath)
			d := partition.Disk{
				DevPath:          bd.DevPath,
				DiskSize:         bd.Capacity.Storage,
				LogicalBlockSize: uint64(bd.DeviceAttributes.LogicalBlockSize),
			}
			if err := d.CreateSinglePartition(); err != nil {
				klog.Errorf("error creating partition for %s, %v", bd.DevPath, err)
				return err
			}
			klog.Infof("created new partition in %s", bd.DevPath)
			return nil
		}
	} else {
		bd.UUID = uuid
		klog.V(4).Infof("uuid: %s has been generated for device: %s", uuid, bd.DevPath)
		// update cache after generating uuid
		pe.addBlockDeviceToHierarchyCache(bd)
		bdAPI, err := pe.Controller.GetBlockDevice(uuid)

		if errors.IsNotFound(err) {
			klog.V(4).Infof("device: %s, uuid: %s not found in etcd", bd.DevPath, uuid)
			/*
				Cases when the BlockDevice is not found in etcd
				1. The device is appearing in this cluster for the first time
				2. The device had partitions and BlockDevice was not created
			*/

			if bd.DeviceAttributes.DeviceType == blockdevice.BlockDeviceTypePartition {
				klog.V(4).Infof("device: %s is partition", bd.DevPath)
				klog.V(4).Info("checking if device has a parent")
				// check if device has a parent that is claimed
				parentBD, ok := pe.Controller.BDHierarchy[bd.DependentDevices.Parent]
				if !ok {
					klog.V(4).Infof("unable to find parent device for device: %s", bd.DevPath)
					return fmt.Errorf("cannot get parent device for device: %s", bd.DevPath)
				}

				klog.V(4).Infof("parent device: %s found for device: %s", parentBD.DevPath, bd.DevPath)
				klog.V(4).Infof("checking if parent device can be uniquely identified")
				parentUUID, parentOK := generateUUID(parentBD)
				if !parentOK {
					klog.V(4).Infof("unable to generate UUID for parent device, may be a device without WWN")
					// cannot generate UUID for parent, may be a device without WWN
					// used the new algorithm to create partitions
					return pe.createBlockDeviceResourceIfNoHolders(bd, bdAPIList)
				}

				klog.V(4).Infof("uuid: %s generated for parent device: %s", parentUUID, parentBD.DevPath)

				parentBDAPI, err := pe.Controller.GetBlockDevice(parentUUID)

				if errors.IsNotFound(err) {
					// parent not present in etcd, may be device without wwn or had partitions/holders
					klog.V(4).Infof("parent device: %s, uuid: %s not found in etcd", parentBD.DevPath, parentUUID)
					return pe.createBlockDeviceResourceIfNoHolders(bd, bdAPIList)
				}

				if err != nil {
					klog.Error(err)
					return err
					// get call failed
				}

				if parentBDAPI.Status.ClaimState != apis.BlockDeviceUnclaimed {
					// device is in use, and the consumer is doing something
					// do nothing
					klog.V(4).Infof("parent device: %s is in use, device: %s can be ignored", parentBD.DevPath, bd.DevPath)
					return nil
				} else {
					// the consumer created some partitions on the disk.
					// So the parent BD need to be deactivated and partition BD need to be created.
					// 1. deactivate parent
					// 2. create resource for partition

					pe.Controller.DeactivateBlockDevice(*parentBDAPI)
					existingBlockDeviceResource := pe.Controller.GetExistingBlockDeviceResource(bdAPIList, bd.UUID)
					annotations := map[string]string{
						internalUUIDSchemeAnnotation: gptUUIDScheme,
					}

					err = pe.createOrUpdateWithAnnotation(annotations, bd, existingBlockDeviceResource)
					if err != nil {
						klog.Error(err)
						return err
					}
					return nil
				}

			}

			if bd.DeviceAttributes.DeviceType != blockdevice.BlockDeviceTypePartition &&
				len(bd.DependentDevices.Partitions) > 0 {
				klog.V(4).Infof("device: %s has partitions: %+v", bd.DevPath, bd.DependentDevices.Partitions)
				return nil
			}

			return pe.createBlockDeviceResourceIfNoHolders(bd, bdAPIList)
		}

		if err != nil {
			klog.Errorf("querying etcd failed: %+v", err)
			return err
		}

		if bdAPI.Status.ClaimState != apis.BlockDeviceUnclaimed {
			klog.V(4).Infof("device: %s is in use. update the details of the blockdevice", bd.DevPath)

			annotation := map[string]string{
				internalUUIDSchemeAnnotation: gptUUIDScheme,
			}

			err = pe.createOrUpdateWithAnnotation(annotation, bd, bdAPI)
			if err != nil {
				klog.Errorf("updating block device resource failed: %+v", err)
				return err
			}
			return nil
		}

		klog.V(4).Infof("creating resource for device: %s with uuid: %s", bd.DevPath, bd.UUID)
		existingBlockDeviceResource := pe.Controller.GetExistingBlockDeviceResource(bdAPIList, bd.UUID)
		annotations := map[string]string{
			internalUUIDSchemeAnnotation: gptUUIDScheme,
		}

		err = pe.createOrUpdateWithAnnotation(annotations, bd, existingBlockDeviceResource)
		if err != nil {
			klog.Errorf("creation of resource failed: %+v", err)
			return err
		}
		return nil
	}
	return nil
}

// createBlockDeviceResourceIfNoHolders creates/updates a blockdevice resource if it does not have any
// holder devices
func (pe *ProbeEvent) createBlockDeviceResourceIfNoHolders(bd blockdevice.BlockDevice, bdAPIList *apis.BlockDeviceList) error {
	if len(bd.DependentDevices.Holders) > 0 {
		klog.V(4).Infof("device: %s has holder devices: %+v", bd.DevPath, bd.DependentDevices.Holders)
		klog.V(4).Infof("skip creating BlockDevice resource")
		return nil
	}

	klog.V(4).Infof("creating block device resource for device: %s with uuid: %s", bd.DevPath, bd.UUID)

	existingBlockDeviceResource := pe.Controller.GetExistingBlockDeviceResource(bdAPIList, bd.UUID)

	annotations := map[string]string{
		internalUUIDSchemeAnnotation: gptUUIDScheme,
	}

	err := pe.createOrUpdateWithAnnotation(annotations, bd, existingBlockDeviceResource)
	if err != nil {
		klog.Error(err)
		return err
	}
	return nil
}

// upgradeBD returns true if further processing required after upgrade
// NOTE: only cstor and localPV will be upgraded. upgrade of local PV raw block is not supported
func (pe *ProbeEvent) upgradeBD(bd blockdevice.BlockDevice, bdAPIList *apis.BlockDeviceList) (bool, error) {
	if !bd.DevUse.InUse {
		// device not in use
		return true, nil
	}

	if bd.DevUse.UsedBy == blockdevice.LocalPV {
		if ok, err := pe.upgradeDeviceInUseByLocalPV(bd, bdAPIList); err != nil {
			return false, err
		} else {
			return ok, nil
		}

	}

	if bd.DevUse.UsedBy == blockdevice.CStor {
		if ok, err := pe.upgradeDeviceInUseByCStor(bd, bdAPIList); err != nil {
			return false, err
		} else {
			return ok, nil
		}
	}
	// device is not used by any storage engines. proceed with normal workflow
	return true, nil
}

// handleUnmanagedDevices handles add event for devices that are currently not managed by the NDM daemon
// returns true, if further processing is required, else false
// TODO include jiva storage engine also
func (pe *ProbeEvent) handleUnmanagedDevices(bd blockdevice.BlockDevice, bdAPIList *apis.BlockDeviceList) (bool, error) {
	// handle if the device is used by mayastor
	if ok, err := pe.deviceInUseByMayastor(bd, bdAPIList); err != nil {
		return ok, err
	} else if !ok {
		return false, nil
	}

	// handle if the device is used by zfs localPV
	if ok, err := pe.deviceInUseByZFSLocalPV(bd, bdAPIList); err != nil {
		return ok, err
	} else if !ok {
		return false, nil
	}
	return true, nil
}

// deviceInUseByMayastor checks if the device is in use by mayastor and returns true if further processing of the event
// is required
func (pe *ProbeEvent) deviceInUseByMayastor(bd blockdevice.BlockDevice, bdAPIList *apis.BlockDeviceList) (bool, error) {
	if !bd.DevUse.InUse {
		return true, nil
	}

	// not in use by mayastor
	if bd.DevUse.UsedBy != blockdevice.Mayastor {
		return true, nil
	}

	klog.V(4).Infof("Device: %s in use by mayastor. ignoring the event", bd.DevPath)
	return false, nil
}

// deviceInUseByZFSLocalPV check if the device is in use by zfs localPV and returns true if further processing of
// event is required. If the device has ZFS pv on it, then a blockdevice resource will be created and zfs PV tag
// will be added on to the resource
func (pe *ProbeEvent) deviceInUseByZFSLocalPV(bd blockdevice.BlockDevice, bdAPIList *apis.BlockDeviceList) (bool, error) {
	if bd.DeviceAttributes.DeviceType == blockdevice.BlockDeviceTypePartition {
		parentBD, ok := pe.Controller.BDHierarchy[bd.DependentDevices.Parent]
		if !ok {
			klog.Errorf("unable to find parent device for %s", bd.DevPath)
			return false, fmt.Errorf("error in getting parent device for %s from device hierarchy", bd.DevPath)
		}
		if parentBD.DevUse.InUse && parentBD.DevUse.UsedBy == blockdevice.ZFSLocalPV {
			klog.V(4).Infof("ParentDevice: %s of device: %s in use by zfs-localPV", parentBD.DevPath, bd.DevPath)
			return false, nil
		}

	}
	if !bd.DevUse.InUse {
		return true, nil
	}

	// not in use by zfs localpv
	if bd.DevUse.UsedBy != blockdevice.ZFSLocalPV {
		return true, nil
	}

	klog.Infof("device: %s in use by zfs-localPV", bd.DevPath)

	uuid, ok := generateUUIDFromPartitionTable(bd)
	if !ok {
		klog.Errorf("unable to generate uuid for zfs-localPV device: %s", bd.DevPath)
		return false, fmt.Errorf("error generating uuid for zfs-localPV disk: %s", bd.DevPath)
	}

	bd.UUID = uuid

	deviceInfo := pe.Controller.NewDeviceInfoFromBlockDevice(&bd)
	bdAPI := deviceInfo.ToDevice()
	bdAPI.Labels[kubernetes.BlockDeviceTagLabel] = string(blockdevice.ZFSLocalPV)

	err := pe.Controller.CreateBlockDevice(bdAPI)
	if err != nil {
		klog.Errorf("unable to push %s (%s) to etcd", bd.UUID, bd.DevPath)
		return false, err
	}
	klog.Infof("Pushed zfs-localPV device: %s (%s) to etcd", bd.UUID, bd.DevPath)
	return false, nil
}

// upgradeDeviceInUseByCStor handles the upgrade if the device is used by cstor. returns true if further processing
// is required
func (pe *ProbeEvent) upgradeDeviceInUseByCStor(bd blockdevice.BlockDevice, bdAPIList *apis.BlockDeviceList) (bool, error) {
	uuid, ok := generateUUID(bd)
	if ok {
		existingBD := pe.Controller.GetExistingBlockDeviceResource(bdAPIList, uuid)
		if existingBD != nil {
			if existingBD.Status.ClaimState != apis.BlockDeviceUnclaimed {
				// device in use using gpt UUID
				return true, nil
			} else {
				// should never reach this case
				klog.Error("unreachable state")
				return false, fmt.Errorf("unreachable state")
			}
		}
	}

	legacyUUID, isVirt := generateLegacyUUID(bd)
	existingLegacyBD := pe.Controller.GetExistingBlockDeviceResource(bdAPIList, legacyUUID)

	// check if any blockdevice exist with the annotation, if yes, that will be used.
	// This is to handle the case where device comes at the same path of an earlier device
	if r := getExistingBDWithPartitionUUID(bd, bdAPIList); r != nil {
		existingLegacyBD = r
	}

	if existingLegacyBD == nil {
		// create device with partition annotation and legacy annotation
		// the custom create / update method should be called here
		// no further processing is required
		bd.UUID = legacyUUID
		err := pe.createOrUpdateWithPartitionUUID(bd, existingLegacyBD)
		return false, err
	}

	if existingLegacyBD.Status.ClaimState != apis.BlockDeviceUnclaimed {
		// update resource with legacy and partition table uuid annotation
		// further processing is not required
		bd.UUID = existingLegacyBD.Name
		err := pe.createOrUpdateWithPartitionUUID(bd, existingLegacyBD)
		return false, err
	}

	if isVirt {
		// update the resource with partition and legacy annotation
		bd.UUID = existingLegacyBD.Name
		err := pe.createOrUpdateWithPartitionUUID(bd, existingLegacyBD)
		return false, err
	} else {
		// should never reach this case.
		klog.Error("unreachable state")
		return false, fmt.Errorf("unreachable state")
	}
}

// upgradeDeviceInUseByLocalPV handles upgrade for devices in use by localPV. returns true if further processing required.
// NOTE: localPV raw block upgrade is not supported
func (pe *ProbeEvent) upgradeDeviceInUseByLocalPV(bd blockdevice.BlockDevice, bdAPIList *apis.BlockDeviceList) (bool, error) {
	uuid, ok := generateUUID(bd)
	if ok {
		existingBD := pe.Controller.GetExistingBlockDeviceResource(bdAPIList, uuid)
		if existingBD != nil {
			if existingBD.Status.ClaimState != apis.BlockDeviceUnclaimed {
				// device in use using gpt UUID
				return true, nil
			} else {
				// should never reach this case
				klog.Error("unreachable state")
				return false, fmt.Errorf("unreachable state")
			}
		}
	}

	legacyUUID, isVirt := generateLegacyUUID(bd)
	existingLegacyBD := pe.Controller.GetExistingBlockDeviceResource(bdAPIList, legacyUUID)

	// check if any blockdevice exist with the annotation, if yes, that will be used.
	// This is to handle the case where device comes at the same path of an earlier device
	if r := getExistingBDWithFsUuid(bd, bdAPIList); r != nil {
		existingLegacyBD = r
	}

	// if existingBD is nil. i.e no blockdevice exist with the uuid or fsuuid annotation, then we create
	// the resource.
	if existingLegacyBD == nil {
		// create device with fs annotation and legacy annotation
		// the custom create / update method should be called here
		// no further processing is required
		bd.UUID = legacyUUID
		err := pe.createOrUpdateWithFSUUID(bd, existingLegacyBD)
		return false, err
	}

	if existingLegacyBD.Status.ClaimState != apis.BlockDeviceUnclaimed {
		// update resource with legacy and fsuuid annotation
		// further processing is not required
		bd.UUID = existingLegacyBD.Name
		err := pe.createOrUpdateWithFSUUID(bd, existingLegacyBD)
		return false, err
	}

	if isVirt {
		// update the resource with fs and legacy annotation
		bd.UUID = existingLegacyBD.Name
		err := pe.createOrUpdateWithFSUUID(bd, existingLegacyBD)
		return false, err
	} else {
		// should never reach this case.
		klog.Error("unreachable state")
		return false, fmt.Errorf("unreachable state")
	}
}

// isParentDeviceInUse checks if the parent device of a given device is in use.
// The check is made only if the device is a partition
func (pe *ProbeEvent) isParentDeviceInUse(bd blockdevice.BlockDevice) (bool, error) {
	if bd.DeviceAttributes.DeviceType != blockdevice.BlockDeviceTypePartition {
		return false, nil
	}

	parentBD, ok := pe.Controller.BDHierarchy[bd.DependentDevices.Parent]
	if !ok {
		return false, fmt.Errorf("cannot find parent device of %s", bd.DevPath)
	}

	return parentBD.DevUse.InUse, nil
}

// getExistingBDWithFsUuid returns the blockdevice with matching FSUUID annotation from etcd
func getExistingBDWithFsUuid(bd blockdevice.BlockDevice, bdAPIList *apis.BlockDeviceList) *apis.BlockDevice {
	if len(bd.FSInfo.FileSystemUUID) == 0 {
		return nil
	}
	for _, bdAPI := range bdAPIList.Items {
		fsUUID, ok := bdAPI.Annotations[internalFSUUIDAnnotation]
		if !ok {
			continue
		}
		if fsUUID == bd.FSInfo.FileSystemUUID {
			return &bdAPI
		}
	}
	return nil
}

// getExistingBDWithPartitionUUID returns the blockdevice with matching partition uuid annotation from etcd
func getExistingBDWithPartitionUUID(bd blockdevice.BlockDevice, bdAPIList *apis.BlockDeviceList) *apis.BlockDevice {
	if len(bd.PartitionInfo.PartitionTableUUID) == 0 {
		return nil
	}
	for _, bdAPI := range bdAPIList.Items {
		partitionUUID, ok := bdAPI.Annotations[internalPartitionUUIDAnnotation]
		if !ok {
			continue
		}
		if partitionUUID == bd.PartitionInfo.PartitionTableUUID {
			return &bdAPI
		}
	}
	return nil
}

// createOrUpdateWithFSUUID creates/updates a resource in etcd. It additionally adds an annotation with the
// fs uuid of the blockdevice
func (pe *ProbeEvent) createOrUpdateWithFSUUID(bd blockdevice.BlockDevice, existingBD *apis.BlockDevice) error {
	annotation := map[string]string{
		internalUUIDSchemeAnnotation: legacyUUIDScheme,
		internalFSUUIDAnnotation:     bd.FSInfo.FileSystemUUID,
	}
	err := pe.createOrUpdateWithAnnotation(annotation, bd, existingBD)
	if err != nil {
		klog.Errorf("could not push localPV device: %s (%s) to etcd", bd.UUID, bd.DevPath)
		return err
	}
	klog.Infof("Pushed localPV device: %s (%s) to etcd", bd.UUID, bd.DevPath)
	return nil
}

// createOrUpdateWithPartitionUUID create/update a resource in etcd. It additionally adds an annotation with the
// partition table uuid of the blockdevice
func (pe *ProbeEvent) createOrUpdateWithPartitionUUID(bd blockdevice.BlockDevice, existingBD *apis.BlockDevice) error {
	annotation := map[string]string{
		internalUUIDSchemeAnnotation:    legacyUUIDScheme,
		internalPartitionUUIDAnnotation: bd.PartitionInfo.PartitionTableUUID,
	}
	err := pe.createOrUpdateWithAnnotation(annotation, bd, existingBD)
	if err != nil {
		klog.Errorf("could not push cstor device: %s (%s) to etcd", bd.UUID, bd.DevPath)
		return err
	}
	klog.Infof("Pushed cstor device: %s (%s) to etcd", bd.UUID, bd.DevPath)
	return nil
}

// createOrUpdateWithAnnotation creates or updates a resource in etcd with given annotation.
func (pe *ProbeEvent) createOrUpdateWithAnnotation(annotation map[string]string, bd blockdevice.BlockDevice, existingBD *apis.BlockDevice) error {
	deviceInfo := pe.Controller.NewDeviceInfoFromBlockDevice(&bd)
	bdAPI := deviceInfo.ToDevice()

	bdAPI.Annotations = annotation

	var err error
	if existingBD != nil {
		err = pe.Controller.UpdateBlockDevice(bdAPI, existingBD)
	} else {
		err = pe.Controller.CreateBlockDevice(bdAPI)
	}
	if err != nil {
		klog.Errorf("unable to push %s (%s) to etcd", bd.UUID, bd.DevPath)
		return err
	}
	return nil
}
