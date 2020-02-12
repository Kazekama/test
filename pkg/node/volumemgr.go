package node

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"

	api "eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/api/generated/v1"
	"eos2git.cec.lab.emc.com/ECS/baremetal-csi-plugin.git/pkg/base"
	"github.com/sirupsen/logrus"
)

type VolumeManager struct {
	availableCapacityCache map[string]*api.AvailableCapacity
	acCacheMu              sync.Mutex

	hWMgrClient api.HWServiceClient
	// stores volumes that actually is use, key - volume ID
	volumesCache map[string]*api.Volume
	vCacheMu     sync.Mutex
	// stores drives that had discovered on previous steps, key - S/N
	drivesCache map[string]*api.Drive
	dCacheMu    sync.Mutex

	linuxUtils *base.LinuxUtils
	log        *logrus.Entry
}

// NewVolumeManager returns new instance ov VolumeManager
func NewVolumeManager(client api.HWServiceClient, executor base.CmdExecutor, logger *logrus.Logger) *VolumeManager {
	vm := &VolumeManager{
		hWMgrClient:            client,
		volumesCache:           make(map[string]*api.Volume),
		drivesCache:            make(map[string]*api.Drive),
		linuxUtils:             base.NewLinuxUtils(executor, logger),
		availableCapacityCache: make(map[string]*api.AvailableCapacity),
	}
	vm.log = logger.WithField("component", "VolumeManager")
	return vm
}

func (m *VolumeManager) SetExecutor(executor base.CmdExecutor) {
	m.linuxUtils.SetLinuxUtilsExecutor(executor)
}

// GetLocalVolumes request return array of volumes on node
func (m *VolumeManager) GetLocalVolumes(context.Context, *api.VolumeRequest) (*api.VolumeResponse, error) {
	volumes := make([]*api.Volume, len(m.volumesCache))
	i := 0
	for _, v := range m.volumesCache {
		volumes[i] = v
		i++
	}
	return &api.VolumeResponse{Volumes: volumes}, nil
}

// GetAvailableCapacity request return array of free capacity on node
func (m *VolumeManager) GetAvailableCapacity(ctx context.Context, req *api.AvailableCapacityRequest) (*api.AvailableCapacityResponse, error) {
	if err := m.DiscoverAvailableCapacity(req.NodeId); err != nil {
		return nil, err
	}
	ac := make([]*api.AvailableCapacity, len(m.availableCapacityCache))
	i := 0
	for _, item := range m.availableCapacityCache {
		ac[i] = item
		i++
	}
	return &api.AvailableCapacityResponse{AvailableCapacity: ac}, nil
}

// Discover inspects drives and create volume object if partition exist
func (m *VolumeManager) Discover() error {
	m.log.Infof("Current volumes cache is: %v", m.volumesCache)

	drivesResponse, err := m.hWMgrClient.GetDrivesList(context.Background(), &api.DrivesRequest{})
	if err != nil {
		return err
	}
	drives := drivesResponse.Disks

	m.updateDrivesCache(drives) // lock dCacheMu
	freeDrives := m.drivesAreNotUsed()

	return m.updateVolumesCache(freeDrives) // lock vCacheMu
}

// updateDrivesCache updates drives cache based on provided list of Drives
func (m *VolumeManager) updateDrivesCache(discoveredDrives []*api.Drive) {
	ll := m.log.WithFields(logrus.Fields{
		"component": "VolumeManager",
		"method":    "updateDrivesCache",
	})

	m.dCacheMu.Lock()
	defer m.dCacheMu.Unlock()

	if len(m.drivesCache) == 0 {
		ll.Info("Initialize drivesCache for the first time")
		for _, d := range discoveredDrives {
			m.drivesCache[d.SerialNumber] = d
		}
		ll.Infof("Drives cache now is: %v", m.drivesCache)
	} else {
		// search drive(s) from discoveredDrives that isn't in cache and add them
		for _, d := range discoveredDrives {
			if _, ok := m.drivesCache[d.SerialNumber]; !ok {
				// add to cache
				ll.Infof("Append to drives cache drive %v", d)
				m.drivesCache[d.SerialNumber] = d
			}
		}
		// search drive(s) that is in cache and isn't found in discoveredDrives, mark them as a OFFLINE
		for _, c := range m.drivesCache {
			exist := false
			for _, d := range discoveredDrives {
				if d.SerialNumber == c.SerialNumber {
					exist = true
					break
				}
			}
			if !exist {
				ll.Warnf("Set status OFFLINE for drive with S/N %s", c.SerialNumber)
				c.Status = api.Status_OFFLINE
			}
		}
	}
}

// updateVolumesCache updates volumes cache based on provided freeDrives
// search drives in freeDrives that are not have volume and if there are
// some partitions on them - try to read partition uuid and create volume object
func (m *VolumeManager) updateVolumesCache(freeDrives []*api.Drive) error {
	ll := m.log.WithFields(logrus.Fields{
		"method": "updateVolumesCache",
	})

	// explore each drive from freeDrives
	lsblk, err := m.linuxUtils.Lsblk(base.DriveTypeDisk)
	if err != nil {
		return fmt.Errorf("unable to inspect system block devices via lsblk, error: %v", err)
	}

	m.vCacheMu.Lock()
	defer m.vCacheMu.Unlock()

	for _, d := range freeDrives {
		for _, ld := range *lsblk {
			if strings.EqualFold(ld.Serial, d.SerialNumber) && len(ld.Children) > 0 {
				uuid, err := m.linuxUtils.GetPartitionUUID(ld.Name)
				if err != nil {
					ll.Warnf("Unable to determine partition UUID for device %s, error: %v", ld.Name, err)
					continue
				}
				size, err := strconv.ParseInt(ld.Size, 10, 64)
				if err != nil {
					ll.Warnf("Unable parse string %s to int, for device %s, error: %v", ld.Size, ld.Name, err)
					continue
				}
				v := &api.Volume{
					Id:           uuid,
					Owner:        "", // TODO: need to search owner ??? CRD ???
					Size:         size,
					Location:     d.SerialNumber,
					LocationType: api.LocationType_Drive,
					Mode:         api.Mode_FS,
					Type:         ld.FSType,
					Health:       d.Health,
					Status:       api.OperationalStatus_Operative,
				}
				ll.Infof("Add in cache volume: %v", v)
				m.volumesCache[v.Id] = v
			}
		}
	}
	return nil
}

//DiscoverAvailableCapacity inspect current available capacity on nodes and fill cache
func (m *VolumeManager) DiscoverAvailableCapacity(nodeID string) error {
	ll := m.log.WithFields(logrus.Fields{
		"component": "VolumeManager",
		"method":    "DiscoverAvailableCapacity",
	})
	ll.Infof("Current available capacity cache is: %v", m.availableCapacityCache)

	m.acCacheMu.Lock()
	defer m.acCacheMu.Unlock()

	for _, drive := range m.drivesCache {
		if drive.Health == api.Health_GOOD && drive.Status == api.Status_ONLINE {
			removed := false
			for _, volume := range m.volumesCache {
				//if drive contains volume then available capacity for this drive will be removed
				if strings.EqualFold(volume.Location, drive.SerialNumber) {
					delete(m.availableCapacityCache, drive.SerialNumber)
					ll.Infof("Remove available capacity on node %s, because drive %s has volume", nodeID, drive.SerialNumber)
					removed = true
				}
			}
			//if drive is empty
			if !removed {
				capacity := &api.AvailableCapacity{
					Size:     drive.Size,
					Type:     api.StorageClass_ANY,
					Location: drive.SerialNumber,
					NodeId:   nodeID,
				}
				ll.Infof("Adding available capacity: %s-%s", capacity.NodeId, capacity.Location)
				m.availableCapacityCache[capacity.Location] = capacity
			}
		} else {
			//If drive is unhealthy or offline, remove available capacity
			for _, ac := range m.availableCapacityCache {
				if drive.SerialNumber == ac.Location {
					ll.Infof("Remove available capacity on node %s, because drive %s is not ready", ac.NodeId, ac.Location)
					delete(m.availableCapacityCache, ac.Location)
					break
				}
			}
		}
	}
	ll.Info("Current available capacity cache: ", m.availableCapacityCache)
	return nil
}

// drivesAreNotUsed search drives in drives cache that isn't have any volumes
func (m *VolumeManager) drivesAreNotUsed() []*api.Drive {
	ll := m.log.WithFields(logrus.Fields{
		"method": "drivesAreNotUsed",
	})

	// search drives that don't have parent volume
	drivesNotInUse := make([]*api.Drive, 0)
	for _, d := range m.drivesCache {
		isUsed := false
		for _, v := range m.volumesCache {
			// expect only Drive LocationType, for Drive LocationType Location will be a SN of the drive
			if d.Type != api.DriveType_NVMe &&
				v.LocationType == api.LocationType_Drive &&
				strings.EqualFold(d.SerialNumber, v.Location) {
				isUsed = true
				ll.Infof("Found volume with ID \"%s\" in cache for drive with S/N \"%s\"",
					v.Id, d.SerialNumber)
				break
			}
		}
		if !isUsed {
			drivesNotInUse = append(drivesNotInUse, d)
		}
	}

	return drivesNotInUse
}

func (m *VolumeManager) CreateLocalVolume(ctx context.Context, req *api.CreateLocalVolumeRequest) (*api.CreateLocalVolumeResponse, error) {
	ll := m.log.WithFields(logrus.Fields{
		"method":   "CreateLocalVolume",
		"volumeID": req.GetPvcUUID(),
	})

	ll.Infof("Processing request: %v", req)

	resp := &api.CreateLocalVolumeResponse{Drive: "", Capacity: 0, Ok: false}

	// TODO: quick hack, here we should be sure that drives cache has been filled
	// TODO: m.Discover() should be the flag that node service pod is ready AK8S-65
	if len(m.drivesCache) == 0 {
		ll.Info("Drives Cache has been initialized. Initialize it ...")
		err := m.Discover()
		if err != nil {
			return resp, fmt.Errorf("unable to perform first Discover and fills drivesCache, error: %v", err)
		}
	}

	m.vCacheMu.Lock()
	defer m.vCacheMu.Unlock()

	var drive *api.Drive
	var err error
	if req.Location != "" {
		drive = m.drivesCache[req.Location]
	} else {
		drive, err = m.searchFreeDrive(req.Capacity)
		if err != nil {
			return resp, err
		}
	}
	device, err := m.searchDrivePathBySN(drive.SerialNumber)
	if err != nil {
		return resp, err
	}
	ll.Infof("Choose device: %s", device)

	rollBacked, err := m.setPartitionUUIDForDev(device, req.PvcUUID)
	if err != nil {
		if !rollBacked {
			ll.Errorf("unable set partition uuid for dev %s, roll back failed too, set drive status to OFFLINE", device)
			m.drivesCache[drive.SerialNumber].Status = api.Status_OFFLINE
		}
		return resp, err
	}

	m.volumesCache[req.PvcUUID] = &api.Volume{
		Id:           req.PvcUUID,
		Owner:        "",
		Size:         drive.Size,
		Location:     drive.SerialNumber,
		LocationType: api.LocationType_Drive,
		Mode:         api.Mode_FS,
		Type:         "", // TODO: set that filed to FSType
		Health:       api.Health_GOOD,
		Status:       api.OperationalStatus_Staging, // becomes operative in NodePublishCall
	}

	return &api.CreateLocalVolumeResponse{Drive: device, Capacity: drive.Size, Ok: true}, nil
}

// searchDrivePathBySN returns drive path based on drive S/N
func (m *VolumeManager) searchDrivePathBySN(sn string) (string, error) {
	lsblkOut, err := m.linuxUtils.Lsblk("disk")
	if err != nil {
		return "", err
	}

	device := ""
	for _, l := range *lsblkOut {
		if strings.EqualFold(l.Serial, sn) {
			device = l.Name
			break
		}
	}

	if device == "" {
		return "", fmt.Errorf("unable to find drive path by S/N %s", sn)
	}

	return device, nil
}

// searchFreeDrive search drive in drives cache with appropriate capacity
func (m *VolumeManager) searchFreeDrive(capacity int64) (*api.Drive, error) {
	freeDrives := m.drivesAreNotUsed()
	minSize := int64(math.MaxInt64)
	var drive *api.Drive
	for _, d := range freeDrives {
		if d.Size >= capacity && d.Size < minSize {
			drive = d
			minSize = d.Size
		}
	}

	if drive == nil {
		return nil, fmt.Errorf("unable to find suitable drive with capacity %d", capacity)
	}

	return drive, nil
}

// setPartitionUUIDForDev creates partition and sets partition UUID, if some step fails
// will try to rollback operation, returns error and roll back operation status (bool)
// if error occurs, status value will show whether device has roll back to the initial state
func (m *VolumeManager) setPartitionUUIDForDev(device string, uuid string) (rollBacked bool, err error) {
	var exist bool
	rollBacked = true

	// check existence
	exist, err = m.linuxUtils.IsPartitionExists(device)
	if err != nil {
		return
	}
	if exist {
		return rollBacked, fmt.Errorf("partition has already exist on device %s", device)
	}

	// create partition table
	err = m.linuxUtils.CreatePartitionTable(device)
	if err != nil {
		return
	}

	// create partition
	err = m.linuxUtils.CreatePartition(device)
	if err != nil {
		// try to delete partition
		exist, _ = m.linuxUtils.IsPartitionExists(device)
		if exist {
			if errDel := m.linuxUtils.DeletePartition(device); errDel != nil {
				rollBacked = false
				return
			}
		}
		return
	}

	// set partition UUID
	err = m.linuxUtils.SetPartitionUUID(device, uuid)
	if err != nil {
		errDel := m.linuxUtils.DeletePartition(device)
		if errDel != nil {
			rollBacked = false
			return
		}
		return
	}
	return rollBacked, err
}

func (m *VolumeManager) DeleteLocalVolume(ctx context.Context, request *api.DeleteLocalVolumeRequest) (*api.DeleteLocalVolumeResponse, error) {
	ll := m.log.WithFields(logrus.Fields{
		"method":   "DeleteLocalVolume",
		"volumeID": request.GetPvcUUID(),
	})

	ll.Infof("Processing request: %v", request)

	m.vCacheMu.Lock()
	defer m.vCacheMu.Unlock()

	volume := m.volumesCache[request.PvcUUID]

	if volume == nil {
		return &api.DeleteLocalVolumeResponse{Ok: false}, errors.New("unable to find volume by PVC UUID in volume manager cache")
	}

	device, err := m.searchDrivePathBySN(volume.Location)
	if err != nil {
		return &api.DeleteLocalVolumeResponse{Ok: false},
			fmt.Errorf("unable to find device for drive with S/N %s", volume.Location)
	}

	err = m.linuxUtils.DeletePartition(device)
	if err != nil {
		wErr := fmt.Errorf("failed to delete partition, error: %v", err)
		ll.Errorf("%v, set operational status - fail to remove", wErr)
		volume.Status = api.OperationalStatus_FailToRemove
		return &api.DeleteLocalVolumeResponse{Ok: false}, wErr
	}

	delete(m.volumesCache, volume.Id)

	return &api.DeleteLocalVolumeResponse{Ok: true}, nil
}
