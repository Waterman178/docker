package hcsshim

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// UVMFolderFromLayerFolders searches a set of layer folders which are indexed
// from base layer at the bottom through sandbox at the top, finding the uppermost
// layer containing the image.
// This is inconsistent between v1 and v2.
func UVMFolderFromLayerFolders(layerFolders []string) (string, error) {
	var uvmFolder string
	for _, layerFolder := range layerFolders {
		_, err := os.Stat(filepath.Join(layerFolder, `UtilityVM`))
		if err == nil {
			uvmFolder = layerFolder
			break
		}
		if !os.IsNotExist(err) {
			return "", err
		}
	}
	if uvmFolder == "" {
		return "", fmt.Errorf("utility VM folder could not be found in layers")
	}
	logrus.Debugf("hcsshim: UVMFolderFromLayerFolders: %s", uvmFolder)
	return uvmFolder, nil
}

func createWCOWv2UVM(createOptions *CreateOptions) (Container, error) {
	logrus.Debugf("HCSShim: Creating utility VM id=%s", createOptions.Id)

	iocis := "invalid OCI spec:"
	if len(createOptions.Spec.Windows.LayerFolders) != 1 {
		return nil, fmt.Errorf("%s Windows.LayerFolders must have length 1 for a hosting system, pointing to a folder containing sandbox.vhdx", iocis)
	}
	if len(createOptions.Spec.Hostname) > 0 {
		return nil, fmt.Errorf("%s Hostname cannot be set for a hosting system", iocis)
	}
	if createOptions.Spec.Windows.Resources != nil && createOptions.Spec.Windows.Resources.CPU != nil && createOptions.Spec.Windows.Resources.CPU.Shares != nil {
		return nil, fmt.Errorf("%s Windows.Resources.CPU.Shares must not be set for a hosting system", iocis)
	}
	if createOptions.Spec.Windows.Resources != nil && createOptions.Spec.Windows.Resources.CPU != nil && createOptions.Spec.Windows.Resources.CPU.Maximum != nil {
		return nil, fmt.Errorf("%s Windows.Resources.CPU.Maximum must not be set for a hosting system", iocis)
	}
	if createOptions.Spec.Root != nil {
		return nil, fmt.Errorf("%s Root must not be set for a hosting system", iocis)
	}
	if createOptions.Spec.Windows.Resources != nil && createOptions.Spec.Windows.Resources.Storage != nil {
		return nil, fmt.Errorf("%s Windows.Resources.Storage must not be set for a hosting system", iocis)
	}
	if createOptions.Spec.Windows.CredentialSpec != nil {
		return nil, fmt.Errorf("%s Windows.CredentialSpec must not be set for a hosting system", iocis)
	}
	if createOptions.Spec.Windows.Network != nil {
		return nil, fmt.Errorf("%s Windows.Network must not be set for a hosting system", iocis) // Need to revisit, but blocking everything currently not hooked up
	}
	if 0 != len(createOptions.Spec.Mounts) {
		return nil, fmt.Errorf("%s Mounts must not be set for a hosting system", iocis)
	}

	// TODO:  Default the utilty VMpath under HyperV in spec if not supplied

	attachments := make(map[string]VirtualMachinesResourcesStorageAttachmentV2)
	attachments["0"] = VirtualMachinesResourcesStorageAttachmentV2{
		Path: filepath.Join(createOptions.Spec.Windows.LayerFolders[0], "sandbox.vhdx"),
		Type: "VirtualDisk",
	}
	scsi := make(map[string]VirtualMachinesResourcesStorageScsiV2)
	scsi["0"] = VirtualMachinesResourcesStorageScsiV2{Attachments: attachments}
	memory := int32(1024)
	processors := int32(2)
	if numCPU() == 1 {
		processors = 1
	}
	if createOptions.Spec.Windows.Resources != nil {
		if createOptions.Spec.Windows.Resources.Memory != nil && createOptions.Spec.Windows.Resources.Memory.Limit != nil {
			memory = int32(*createOptions.Spec.Windows.Resources.Memory.Limit / 1024 / 1024) // OCI spec is in bytes. HCS takes MB
		}
		if createOptions.Spec.Windows.Resources.CPU != nil && createOptions.Spec.Windows.Resources.CPU.Count != nil {
			processors = int32(*createOptions.Spec.Windows.Resources.CPU.Count)
		}
	}
	uvm := &ComputeSystemV2{
		Owner:         createOptions.Owner,
		SchemaVersion: createOptions.SchemaVersion,
		VirtualMachine: &VirtualMachineV2{
			Chipset: &VirtualMachinesResourcesChipsetV2{
				UEFI: &VirtualMachinesResourcesUefiV2{
					BootThis: &VirtualMachinesResourcesUefiBootEntryV2{
						DevicePath: `\EFI\Microsoft\Boot\bootmgfw.efi`,
						DiskNumber: 0,
						UefiDevice: "VMBFS",
					},
				},
			},
			ComputeTopology: &VirtualMachinesResourcesComputeTopologyV2{
				Memory: &VirtualMachinesResourcesComputeMemoryV2{
					Backing:             "Virtual",
					Startup:             memory,
					DirectFileMappingMB: 1024, // Sensible default, but could be a tuning parameter somewhere
				},
				Processor: &VirtualMachinesResourcesComputeProcessorV2{
					Count: processors,
				},
			},

			Devices: &VirtualMachinesDevicesV2{
				// Add networking here.... TODO
				SCSI: scsi,
				VirtualSMBShares: []VirtualMachinesResourcesStorageVSmbShareV2{VirtualMachinesResourcesStorageVSmbShareV2{
					Flags: VsmbFlagReadOnly | VsmbFlagPseudoOplocks | VsmbFlagTakeBackupPrivilege | VsmbFlagCacheIO | VsmbFlagShareRead,
					Name:  "os",
					Path:  createOptions.Spec.Windows.HyperV.UtilityVMPath,
				}},
				GuestInterface: &VirtualMachinesResourcesGuestInterfaceV2{ConnectToBridge: true},
			},
		},
	}

	uvmb, err := json.Marshal(uvm)
	if err != nil {
		return nil, err
	}
	uvmContainer, err := createContainer(createOptions.Id, string(uvmb), SchemaV20())
	if err != nil {
		logrus.Debugln("failed to create UVM: ", err)
		return nil, err
	}
	uvmContainer.(*container).scsiLocations.hostPath[0][0] = attachments["0"].Path
	return uvmContainer, nil
}

// removeVSMB removes a VSMB share from a utility VM. The mutex must be
// held when calling this function
func removeVSMB(c Container, id string) error {
	logrus.Debugf("HCSShim: Removing vsmb %s", id)
	if _, ok := c.(*container).vsmbShares.guids[id]; !ok {
		return fmt.Errorf("failed to remove vsmbShare %s as it is not in utility VM %s", id, c.(*container).id)
	} else {
		logrus.Debugf("VSMB: %s refcount: %d", id, c.(*container).vsmbShares.guids[id])
		c.(*container).vsmbShares.guids[id]--
		if c.(*container).vsmbShares.guids[id] == 0 {
			delete(c.(*container).vsmbShares.guids, id)
			modification := &ModifySettingsRequestV2{
				ResourceType: ResourceTypeVSmbShare,
				RequestType:  RequestTypeRemove,
				// TODO: https://microsoft.visualstudio.com/OS/_queries?_a=edit&id=17031676&triage=true. Settings should not be required, just ResourceUri
				Settings:    VirtualMachinesResourcesStorageVSmbShareV2{Name: id},
				ResourceUri: fmt.Sprintf("virtualmachine/devices/virtualsmbshares/%s", id),
			}
			if err := c.Modify(modification); err != nil {
				return fmt.Errorf("failed to remove vsmbShare %s from utility VM %s after refcount dropped to zero: %s", id, c.(*container).id, err)
			}
		}
	}
	return nil
}

// removeVSMBOnFailure is a helper to roll-back any VSMB shares added to a utility VM on a failure path
func removeVSMBOnFailure(c Container, toRemove []string) {
	if len(toRemove) == 0 {
		return
	}
	c.(*container).vsmbShares.Lock()
	defer c.(*container).vsmbShares.Unlock()
	for _, vsmbShare := range toRemove {
		if err := removeVSMB(c, vsmbShare); err != nil {
			logrus.Warnf("Possibly leaked vsmbshare on error removal path: %s", err)
		}
	}
}

// removeSCSI removes a mapped virtual disk from a containers SCSI controller. The mutex
// MUST be held when calling this function
func removeSCSI(c Container, controller int, lun int, containerPath string) error {
	scsiModification := &ModifySettingsRequestV2{
		ResourceType: ResourceTypeMappedVirtualDisk,
		RequestType:  RequestTypeRemove,
		ResourceUri:  fmt.Sprintf("VirtualMachine/Devices/SCSI/%d/%d", controller, lun),
	}
	if containerPath != "" {
		scsiModification.HostedSettings = ContainersResourcesMappedDirectoryV2{
			ContainerPath: containerPath,
			Lun:           uint8(lun),
		}
	}
	if err := c.Modify(scsiModification); err != nil {
		return err
	}
	c.(*container).scsiLocations.hostPath[controller][lun] = ""
	return nil
}

// removeSCSIOnFailure is a helper to roll-back a SCSI disk added to a utility VM on a failure path.
// The mutex  must NOT be held when calling this function.
func removeSCSIOnFailure(c Container, controller int, lun int) {
	c.(*container).scsiLocations.Lock()
	defer c.(*container).scsiLocations.Unlock()
	if err := removeSCSI(c, controller, lun, ""); err != nil {
		logrus.Warnf("Possibly leaked SCSI disk on error removal path: %s", err)
	}
}

// CreateHCSContainerDocument creates a document suitable for calling HCS to create
// a container, both hosted and process isolated. It can create both v1 and v2
// schema. This is exported just in case a client could find it useful, but
// not strictly necessary as it will be called by CreateContainerEx()
func CreateHCSContainerDocument(createOptions *CreateOptions) (string, error) {
	logrus.Debugf("hcsshim: CreateHCSContainerDocument")

	// TODO: Make this safe if exported so no null pointer dereferences.
	// TODO: Should this be a Windows function explicitly in the name

	if createOptions.Spec == nil {
		return "", fmt.Errorf("cannot create HCS container document - OCI spec is missing")
	}

	if createOptions.Spec.Windows == nil {
		return "", fmt.Errorf("cannot create HCS container document - OCI spec Windows section is missing ")
	}

	v1 := &ContainerConfig{
		SystemType:              "Container",
		Name:                    createOptions.Id,
		Owner:                   createOptions.Owner,
		HvPartition:             false,
		IgnoreFlushesDuringBoot: createOptions.Spec.Windows.IgnoreFlushesDuringBoot,
	}

	// IgnoreFlushesDuringBoot is a property of the SCSI attachment for the sandbox. Set when it's hot-added to the utility VM
	// ID is a property on the create call in V2 rather than part of the schema.
	v2 := &ComputeSystemV2{
		Owner:                             createOptions.Owner,
		SchemaVersion:                     SchemaV20(),
		ShouldTerminateOnLastHandleClosed: true,
	}
	v2Container := &ContainerV2{}
	if createOptions.HostingSystem == nil {
		v2Container.Storage = &ContainersResourcesStorageV2{}
	} else {
		if createOptions.MountedLayers == nil {
			return "", fmt.Errorf("v2 schema call for a hosted container must supply mounted layers")
		}
		v2Container.Storage = createOptions.MountedLayers
	}

	if createOptions.Spec.Hostname != "" {
		v1.HostName = createOptions.Spec.Hostname
		v2Container.GuestOS = &GuestOsV2{HostName: createOptions.Spec.Hostname}
	}

	if createOptions.Spec.Windows.Resources != nil {
		if createOptions.Spec.Windows.Resources.CPU != nil {
			if createOptions.Spec.Windows.Resources.CPU.Count != nil ||
				createOptions.Spec.Windows.Resources.CPU.Shares != nil ||
				createOptions.Spec.Windows.Resources.CPU.Maximum != nil {
				v2Container.Processor = &ContainersResourcesProcessorV2{}
			}
			if createOptions.Spec.Windows.Resources.CPU.Count != nil {
				cpuCount := *createOptions.Spec.Windows.Resources.CPU.Count
				hostCPUCount := uint64(numCPU())
				if cpuCount > hostCPUCount {
					createOptions.Logger.Warnf("Changing requested CPUCount of %d to current number of processors, %d", cpuCount, hostCPUCount)
					cpuCount = hostCPUCount
				}
				v1.ProcessorCount = uint32(cpuCount)
				v2Container.Processor.Count = v1.ProcessorCount
			}
			if createOptions.Spec.Windows.Resources.CPU.Shares != nil {
				v1.ProcessorWeight = uint64(*createOptions.Spec.Windows.Resources.CPU.Shares)
				v2Container.Processor.Weight = v1.ProcessorWeight
			}
			if createOptions.Spec.Windows.Resources.CPU.Maximum != nil {
				v1.ProcessorMaximum = int64(*createOptions.Spec.Windows.Resources.CPU.Maximum)
				v2Container.Processor.Maximum = uint64(v1.ProcessorMaximum)
			}
		}
		if createOptions.Spec.Windows.Resources.Memory != nil {
			if createOptions.Spec.Windows.Resources.Memory.Limit != nil {
				v1.MemoryMaximumInMB = int64(*createOptions.Spec.Windows.Resources.Memory.Limit) / 1024 / 1024
				v2Container.Memory = &ContainersResourcesMemoryV2{Maximum: uint64(v1.MemoryMaximumInMB)}

			}
		}
		if createOptions.Spec.Windows.Resources.Storage != nil {
			if createOptions.Spec.Windows.Resources.Storage.Bps != nil || createOptions.Spec.Windows.Resources.Storage.Iops != nil {
				v2Container.Storage.StorageQoS = &ContainersResourcesStorageQoSV2{}
			}
			if createOptions.Spec.Windows.Resources.Storage.Bps != nil {
				v1.StorageBandwidthMaximum = *createOptions.Spec.Windows.Resources.Storage.Bps
				v2Container.Storage.StorageQoS.BandwidthMaximum = *createOptions.Spec.Windows.Resources.Storage.Bps
			}
			if createOptions.Spec.Windows.Resources.Storage.Iops != nil {
				v1.StorageIOPSMaximum = *createOptions.Spec.Windows.Resources.Storage.Iops
				v2Container.Storage.StorageQoS.IOPSMaximum = *createOptions.Spec.Windows.Resources.Storage.Iops
			}
		}
	}

	// TODO V2 networking. Only partial at the moment. v2.Container.Networking.Namespace specifically
	if createOptions.Spec.Windows.Network != nil {
		v2Container.Networking = &ContainersResourcesNetworkingV2{}

		v1.EndpointList = createOptions.Spec.Windows.Network.EndpointList
		v2Container.Networking.NetworkAdapters = v1.EndpointList

		v1.AllowUnqualifiedDNSQuery = createOptions.Spec.Windows.Network.AllowUnqualifiedDNSQuery
		v2Container.Networking.AllowUnqualifiedDnsQuery = v1.AllowUnqualifiedDNSQuery

		if createOptions.Spec.Windows.Network.DNSSearchList != nil {
			v1.DNSSearchList = strings.Join(createOptions.Spec.Windows.Network.DNSSearchList, ",")
			v2Container.Networking.DNSSearchList = v1.DNSSearchList
		}

		v1.NetworkSharedContainerName = createOptions.Spec.Windows.Network.NetworkSharedContainerName
		v2Container.Networking.NetworkSharedContainerName = v1.NetworkSharedContainerName
	}

	//	// TODO V2 Credentials not in the schema yet.
	if cs, ok := createOptions.Spec.Windows.CredentialSpec.(string); ok {
		v1.Credentials = cs
	}

	// We must have least two layers in the spec - a base and RW layer.

	// TODO JJH HACK HACK. Only want to check this if not a hosting system. I think. As the layers are passed in through mountedLayers

	//	if createOptions.Spec.Windows.LayerFolders == nil || len(createOptions.Spec.Windows.LayerFolders) < 2 {
	//		return "", fmt.Errorf("invalid spec - not enough layer folders supplied")
	//	}

	if createOptions.HostingSystem == nil { // ie Not a v2 xenon

		// Strip off the top-most RW/Sandbox layer as that's passed in separately to HCS for v1
		// TODO Should this be inside the check below?
		v1.LayerFolderPath = createOptions.Spec.Windows.LayerFolders[len(createOptions.Spec.Windows.LayerFolders)-1]

		if createOptions.Spec.Windows.HyperV == nil {
			if createOptions.Spec.Root == nil {
				return "", fmt.Errorf("invalid container spec - Root must be set")
			}
			const volumeGUIDRegex = `^\\\\\?\\(Volume)\{{0,1}[0-9a-fA-F]{8}\-[0-9a-fA-F]{4}\-[0-9a-fA-F]{4}\-[0-9a-fA-F]{4}\-[0-9a-fA-F]{12}(\}){0,1}\}\\$`
			if _, err := regexp.MatchString(volumeGUIDRegex, createOptions.Spec.Root.Path); err != nil {
				return "", fmt.Errorf(`invalid container spec - Root.Path '%s' must be a volume GUID path in the format '\\?\Volume{GUID}\'`, createOptions.Spec.Root.Path)
			}
			rootPath := createOptions.Spec.Root.Path
			if rootPath[len(rootPath)-1] != '\\' {
				rootPath = fmt.Sprintf(`%s\`, rootPath) // Be nice to clients and make sure well-formed for back-compat
			}
			v1.VolumePath = rootPath[:len(rootPath)-1] // Strip the trailing backslash. Required for v1.
			v2Container.Storage.Path = rootPath
		} else {
			if createOptions.Spec.Root != nil && createOptions.Spec.Root.Path != "" {
				return "", fmt.Errorf("invalid container spec - Root.Path must be omitted for a Hyper-V container")
			}
			if createOptions.SchemaVersion.IsV10() {
				v1.HvPartition = true
				if createOptions.Spec.Windows.HyperV.UtilityVMPath != "" {
					v1.HvRuntime = &HvRuntime{ImagePath: createOptions.Spec.Windows.HyperV.UtilityVMPath}
				} else {
					uvmImagePath, err := UVMFolderFromLayerFolders(createOptions.Spec.Windows.LayerFolders)
					if err != nil {
						return "", err
					}
					v1.HvRuntime = &HvRuntime{ImagePath: uvmImagePath}
				}
			}
		}
	}

	if createOptions.Spec.Root != nil && createOptions.Spec.Root.Readonly {
		return "", fmt.Errorf(`invalid container spec - readonly is not supported`)
	}

	if createOptions.HostingSystem == nil { // ie Not a v2 xenon. As the mounted layers were passed in instead.
		for _, layerPath := range createOptions.Spec.Windows.LayerFolders[:len(createOptions.Spec.Windows.LayerFolders)-1] {
			_, filename := filepath.Split(layerPath)
			g, err := NameToGuid(filename)
			if err != nil {
				return "", err
			}
			v1.Layers = append(v1.Layers, Layer{ID: g.ToString(), Path: layerPath})
			v2Container.Storage.Layers = append(v2Container.Storage.Layers, ContainersResourcesLayerV2{Id: g.ToString(), Path: layerPath})
		}
	}

	// Add the mounts as mapped directories or mapped pipes
	var (
		mdsv1 []MappedDir
		mpsv1 []MappedPipe
		mdsv2 []ContainersResourcesMappedDirectoryV2
		mpsv2 []ContainersResourcesMappedPipeV2
	)
	for _, mount := range createOptions.Spec.Mounts {
		const pipePrefix = `\\.\pipe\`
		if mount.Type != "" {
			return "", fmt.Errorf("invalid container spec - Mount.Type '%s' must not be set", mount.Type)
		}
		if strings.HasPrefix(mount.Destination, pipePrefix) {
			mpsv1 = append(mpsv1, MappedPipe{HostPath: mount.Source, ContainerPipeName: mount.Destination[len(pipePrefix):]})
			mpsv2 = append(mpsv2, ContainersResourcesMappedPipeV2{HostPath: mount.Source, ContainerPipeName: mount.Destination[len(pipePrefix):]})
		} else {
			mdv1 := MappedDir{HostPath: mount.Source, ContainerPath: mount.Destination, ReadOnly: false}
			mdv2 := ContainersResourcesMappedDirectoryV2{HostPath: mount.Source, ContainerPath: mount.Destination, ReadOnly: false}
			for _, o := range mount.Options {
				if strings.ToLower(o) == "ro" {
					mdv1.ReadOnly = true
					mdv2.ReadOnly = true
				}
			}
			mdsv1 = append(mdsv1, mdv1)
			mdsv2 = append(mdsv2, mdv2)
		}
	}
	v1.MappedDirectories = mdsv1
	v2Container.MappedDirectories = mdsv2
	if len(mpsv1) > 0 && GetOSVersion().Build < 16299 { // RS3
		return "", fmt.Errorf("named pipe mounts are not supported on this version of Windows")
	}
	v1.MappedPipes = mpsv1
	v2Container.MappedPipes = mpsv2

	// Put the v2Container object as a HostedSystem for a Xenon, or directly in the schema for an Argon.
	if createOptions.HostingSystem == nil {
		v2.Container = v2Container
	} else {
		v2.HostingSystemId = createOptions.HostingSystem.(*container).id
		v2.HostedSystem = &HostedSystemV2{
			SchemaVersion: SchemaV20(),
			Container:     v2Container,
		}
	}

	if createOptions.SchemaVersion.IsV10() {
		v1b, err := json.Marshal(v1)
		if err != nil {
			return "", err
		}
		logrus.Debugln("hcsshim: HCS Document:", string(v1b))
		return string(v1b), nil
	} else {
		v2b, err := json.Marshal(v2)
		if err != nil {
			return "", err
		}
		logrus.Debugln("hcsshim: HCS Document:", string(v2b))
		return string(v2b), nil
	}
}

// Mount is a helper for clients to hide all the complexity of layer mounting
// Layer folder are in order: base, [rolayer1..rolayern,] sandbox
// TODO: Extend for LCOW?
//
// v1/v2: Argon WCOW: Returns the mount path on the host as a volume GUID.
// v1:    Xenon WCOW: Done internally in HCS, so no point calling doing anything here.
// v2:    Xenon WCOW: Returns a CombinedLayersV2 structure where ContainerRootPath is a folder
//                    inside the utility VM which is a GUID mapping of the sandbox folder. Each
//                    of the layers are the VSMB locations where the read-only layers are mounted.

// TODO Should this return a string or an object? More efficient as object, but requires more client work to marshall it again.
func Mount(layerFolders []string, hostingSystem Container, sv *SchemaVersion) (interface{}, error) {
	if err := sv.isSupported(); err != nil {
		return nil, err
	}
	if sv.IsV10() ||
		(sv.IsV20() && hostingSystem == nil) {
		if len(layerFolders) < 2 {
			return nil, fmt.Errorf("need at least two layers - base and sandbox")
		}
		id := filepath.Base(layerFolders[len(layerFolders)-1])
		homeDir := filepath.Dir(layerFolders[len(layerFolders)-1])
		di := DriverInfo{HomeDir: homeDir}

		if err := ActivateLayer(di, id); err != nil {
			return nil, err
		}
		if err := PrepareLayer(di, id, layerFolders[:len(layerFolders)-1]); err != nil {
			if err2 := DeactivateLayer(di, id); err2 != nil {
				logrus.Warnf("Failed to Deactivate %s: %s", id, err)
			}
			return nil, err
		}

		mountPath, err := GetLayerMountPath(di, id)
		if err != nil {
			if err := UnprepareLayer(di, id); err != nil {
				logrus.Warnf("Failed to Unprepare %s: %s", id, err)
			}
			if err2 := DeactivateLayer(di, id); err2 != nil {
				logrus.Warnf("Failed to Deactivate %s: %s", id, err)
			}
			return nil, err
		}
		return mountPath, nil
	}

	// V2 UVM

	// 	Add each read-only layers as a VSMB share. In each case, the ResourceUri will end in a GUID based on the folder path.
	//  Each VSMB share is ref-counted so that multiple containers in the same utility VM can share them.
	// TODO OK check here.
	c := hostingSystem.(*container)
	c.vsmbShares.Lock()
	if c.vsmbShares.guids == nil {
		c.vsmbShares.guids = make(map[string]int)
	}
	var vsmbAdded []string
	logrus.Debugln("hcsshim::Mount v2 for hosted system")
	for _, layerPath := range layerFolders[:len(layerFolders)-1] {
		logrus.Debugf("hcsshim::Mount %s as VSMB share", layerPath)
		_, filename := filepath.Split(layerPath)
		guid, err := NameToGuid(filename)
		if err != nil {
			removeVSMBOnFailure(hostingSystem, vsmbAdded)
			c.vsmbShares.Unlock()
			return nil, err
		}
		if _, ok := c.vsmbShares.guids[guid.ToString()]; !ok {
			modification := &ModifySettingsRequestV2{
				ResourceType: ResourceTypeVSmbShare,
				RequestType:  RequestTypeAdd,
				Settings: VirtualMachinesResourcesStorageVSmbShareV2{
					Name:  guid.ToString(),
					Flags: VsmbFlagReadOnly | VsmbFlagPseudoOplocks | VsmbFlagTakeBackupPrivilege | VsmbFlagCacheIO | VsmbFlagShareRead,
					Path:  layerPath,
				},
				ResourceUri: fmt.Sprintf("virtualmachine/devices/virtualsmbshares/%s", guid.ToString()),
			}
			if err := hostingSystem.Modify(modification); err != nil {
				c.vsmbShares.Unlock()
				removeVSMBOnFailure(hostingSystem, vsmbAdded)
				return nil, err
			}
			c.vsmbShares.guids[guid.ToString()] = 1
		} else {
			c.vsmbShares.guids[guid.ToString()]++
		}
		vsmbAdded = append(vsmbAdded, guid.ToString())
		logrus.Debugf("HCSShim::Mount %s: refcount=%d", layerPath, c.vsmbShares.guids[guid.ToString()])
	}
	c.vsmbShares.Unlock()

	// 	Add the sandbox at an unused SCSI location. The container path inside the utility VM will be C:\<GUID> where
	// 	GUID is based on the folder in which the sandbox is located. Therefore, it is critical that if two containers
	// 	are created in the same utility VM, they have unique sandbox directories.

	_, sandboxPath := filepath.Split(layerFolders[len(layerFolders)-1])
	containerPathGUID, err := NameToGuid(sandboxPath)
	if err != nil {
		removeVSMBOnFailure(hostingSystem, vsmbAdded)
		return nil, err
	}
	hostPath := filepath.Join(layerFolders[len(layerFolders)-1], "sandbox.vhdx")
	containerPath := fmt.Sprintf(`C:\%s`, containerPathGUID.ToString())
	controller, lun, err := allocateSCSI(c, hostPath, containerPath)
	if err != nil {
		removeVSMBOnFailure(hostingSystem, vsmbAdded)
		return nil, err
	}

	// TODO: Currently GCS doesn't support more than one SCSI controller. @jhowardmsft/@swernli. This will hopefully be fixed in GCS for RS5.
	// It will also require the HostedSettings to be extended in the call below to include the controller as well as the LUN.
	if controller > 0 {
		return nil, fmt.Errorf("too many SCSI attachments for a single controller")
	}

	sandboxModification := &ModifySettingsRequestV2{
		ResourceType: ResourceTypeMappedVirtualDisk,
		RequestType:  RequestTypeAdd,
		Settings: VirtualMachinesResourcesStorageAttachmentV2{
			Path: hostPath,
			Type: "VirtualDisk",
			// TODO Hmmm....  Where do we do this now????  IgnoreFlushes: createOptions.Spec.Windows.IgnoreFlushesDuringBoot,
		},
		ResourceUri: fmt.Sprintf("VirtualMachine/Devices/SCSI/%d/%d", controller, lun),
		HostedSettings: ContainersResourcesMappedDirectoryV2{
			ContainerPath: containerPath,
			Lun:           uint8(lun),
		},
	}
	if err := hostingSystem.Modify(sandboxModification); err != nil {
		removeVSMBOnFailure(hostingSystem, vsmbAdded)
		return nil, err
	}

	// 	Load the filter at the C:\<GUID> location calculated above. We pass into this request each of the
	// 	read-only layer folders.
	layers := []ContainersResourcesLayerV2{}
	for _, vsmb := range vsmbAdded {
		layers = append(layers, ContainersResourcesLayerV2{
			Id:   vsmb,
			Path: fmt.Sprintf(`\\?\VMSMB\VSMB-{dcc079ae-60ba-4d07-847c-3493609c0870}\%s`, vsmb),
		})
	}
	combinedLayers := CombinedLayersV2{
		ContainerRootPath: fmt.Sprintf(`C:\%s`, containerPathGUID.ToString()),
		Layers:            layers,
	}
	combinedLayersModification := &ModifySettingsRequestV2{
		ResourceType:   ResourceTypeCombinedLayers,
		RequestType:    RequestTypeAdd,
		HostedSettings: combinedLayers,
	}
	if err := hostingSystem.Modify(combinedLayersModification); err != nil {
		removeVSMBOnFailure(hostingSystem, vsmbAdded)
		removeSCSIOnFailure(hostingSystem, controller, lun)
		return nil, err
	}

	logrus.Debugln("HCSShim::Mount Succeeded")
	return combinedLayers, nil
}

// UnmountOperation is used when calling Unmount() to determine what type of unmount is
// required. In V1 schema, this must be UnmountOperationAll. In V2, client can
// be more optimal and only unmount what they need which can be a minor performance
// improvement (eg if you know only one container is running in a utility VM, and
// the UVM is about to be torn down, there's no need to unmount the VSMB shares,
// just SCSI to have a consistent file system).
type UnmountOperation uint

const (
	UnmountOperationSCSI = 0x01
	UnmountOperationVSMB = 0x02
	UnmountOperationAll  = UnmountOperationSCSI | UnmountOperationVSMB
)

// Unmount is a helper for clients to hide all the complexity of layer unmounting
func Unmount(layerFolders []string, hostingSystem Container, sv *SchemaVersion, op UnmountOperation) error {
	if err := sv.isSupported(); err != nil {
		return err
	}
	if sv.IsV10() || (sv.IsV20() && hostingSystem == nil) {
		if op != UnmountOperationAll {
			return fmt.Errorf("Only UnmountOperationAll is supported in the v1 schema, or for v2 schema argons")
		}

		// We only need to have the sandbox.
		if len(layerFolders) < 1 {
			return fmt.Errorf("need at least one layer for Unmount")
		}
		id := filepath.Base(layerFolders[len(layerFolders)-1])
		homeDir := filepath.Dir(layerFolders[len(layerFolders)-1])
		di := DriverInfo{HomeDir: homeDir}
		if err := UnprepareLayer(di, id); err != nil {
			return err
		}
		return DeactivateLayer(di, id)
	}

	// V2 Xenon

	// Base+Sandbox as a minimum. This is different to v1 which only requires the sandbox
	if len(layerFolders) < 2 {
		return fmt.Errorf("at least two layers are required for unmount")
	}

	var retError error
	c := hostingSystem.(*container)

	// Unload the storage filter followed by the SCSI sandbox
	if (op | UnmountOperationSCSI) == UnmountOperationSCSI {
		_, sandboxPath := filepath.Split(layerFolders[len(layerFolders)-1])
		containerPathGUID, err := NameToGuid(sandboxPath)
		if err != nil {
			logrus.Warnf("may leak a sandbox in %s as nametoguid failed: %s", err)
		} else {
			combinedLayersModification := &ModifySettingsRequestV2{
				ResourceType:   ResourceTypeCombinedLayers,
				RequestType:    RequestTypeRemove,
				HostedSettings: CombinedLayersV2{ContainerRootPath: fmt.Sprintf(`C:\%s`, containerPathGUID.ToString())},
			}
			if err := hostingSystem.Modify(combinedLayersModification); err != nil {
				logrus.Errorf(err.Error())
			}
		}

		// Hot remove the sandbox from the SCSI controller
		c.scsiLocations.Lock()
		hostSandboxFile := filepath.Join(layerFolders[len(layerFolders)-1], "sandbox.vhdx")
		controller, lun, err := findSCSIAttachment(c, hostSandboxFile)
		if err != nil {
			logrus.Warnf("sandbox %s is not attached to SCSI - cannot remove!", hostSandboxFile)
		} else {
			if err := removeSCSI(c, controller, lun, fmt.Sprintf(`C:\%s`, containerPathGUID.ToString())); err != nil {
				e := fmt.Errorf("failed to remove SCSI %s: %s", hostSandboxFile, err)
				logrus.Debugln(e)
				if retError == nil {
					retError = e
				} else {
					retError = errors.Wrapf(retError, e.Error())
				}
			}
		}
		c.scsiLocations.Unlock()
	}

	// Remove each of the read-only layers from VSMB. These's are ref-counted and
	// only removed once the count drops to zero. This allows multiple containers
	// to share layers.
	if len(layerFolders) > 1 && (op|UnmountOperationVSMB) == UnmountOperationVSMB {
		c.vsmbShares.Lock()
		if c.vsmbShares.guids == nil {
			c.vsmbShares.guids = make(map[string]int)
		}
		for _, layerPath := range layerFolders[:len(layerFolders)-1] {
			logrus.Debugf("Processing layerPath %s as read-only VSMB share", layerPath)
			_, filename := filepath.Split(layerPath)
			guid, err := NameToGuid(filename)
			if err != nil {
				logrus.Warnf("may have leaked a VSMB share - failed to NameToGuid on %s: %s", filename, err)
				continue
			}
			if _, ok := c.vsmbShares.guids[guid.ToString()]; !ok {
				logrus.Warnf("layer %s is not mounted as a VSMB share - cannot unmount!", layerPath)
				continue
			}
			c.vsmbShares.guids[guid.ToString()]--
			if c.vsmbShares.guids[guid.ToString()] > 0 {
				logrus.Debugf("VSMB read-only layer %s is still in use by another container, not removing from utility VM", layerPath)
				continue
			}
			delete(c.vsmbShares.guids, guid.ToString())
			logrus.Debugf("Processing layerPath %s: Perfoming modify to remove VSMB share", layerPath)
			modification := &ModifySettingsRequestV2{
				ResourceType: ResourceTypeVSmbShare,
				RequestType:  RequestTypeRemove,
				Settings:     VirtualMachinesResourcesStorageVSmbShareV2{Name: guid.ToString()},
				ResourceUri:  fmt.Sprintf("virtualmachine/devices/virtualsmbshares/%s", guid.ToString()),
			}
			if err := hostingSystem.Modify(modification); err != nil {
				e := fmt.Errorf("failed to remove vsmb share %s: %s: %s", layerPath, modification, err)
				logrus.Debugln(e)
				if retError == nil {
					retError = e
				} else {
					retError = errors.Wrapf(retError, e.Error())
				}
			}
		}
		c.vsmbShares.Unlock()
	}

	// TODO (possibly) Consider deleting the container directory in the utility VM

	return retError
}
