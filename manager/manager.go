package manager

import (
	"fmt"
	"sync"

	"github.com/Sirupsen/logrus"
	"github.com/pkg/errors"

	"github.com/rancher/longhorn-manager/datastore"
	"github.com/rancher/longhorn-manager/engineapi"
	"github.com/rancher/longhorn-manager/orchestrator"
	"github.com/rancher/longhorn-manager/types"
	"github.com/rancher/longhorn-manager/util"
)

type VolumeManager struct {
	currentNode *Node

	ds      datastore.DataStore
	orch    orchestrator.Orchestrator
	engines engineapi.EngineClientCollection

	EventChan           chan Event
	managedVolumes      map[string]*ManagedVolume
	managedVolumesMutex *sync.Mutex

	engineImage string
}

func NewVolumeManager(ds datastore.DataStore,
	orch orchestrator.Orchestrator,
	engines engineapi.EngineClientCollection,
	engineImage string) (*VolumeManager, error) {

	manager := &VolumeManager{
		ds:      ds,
		orch:    orch,
		engines: engines,

		EventChan:           make(chan Event),
		managedVolumes:      make(map[string]*ManagedVolume),
		managedVolumesMutex: &sync.Mutex{},

		engineImage: engineImage,
	}

	if err := manager.RegisterNode(-1); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *VolumeManager) VolumeCreate(request *VolumeCreateRequest) (err error) {
	defer func() {
		if err != nil {
			err = errors.Wrap(err, "unable to create volume")
		}
	}()

	// validate the size
	if _, err := util.ConvertSize(request.Size); err != nil {
		return err
	}

	// make it random node's responsibility
	node, err := m.GetRandomNode()
	if err != nil {
		return err
	}

	size := request.Size
	if request.FromBackup != "" {
		backup, err := engineapi.GetBackup(request.FromBackup)
		if err != nil {
			return fmt.Errorf("cannot get backup %v: %v", request.FromBackup, err)
		}
		size = backup.VolumeSize
	}

	info := &types.VolumeInfo{
		VolumeSpec: types.VolumeSpec{
			OwnerID:             node.ID,
			Size:                size,
			FromBackup:          request.FromBackup,
			NumberOfReplicas:    request.NumberOfReplicas,
			StaleReplicaTimeout: request.StaleReplicaTimeout,
			DesireState:         types.VolumeStateDetached,
		},
		VolumeStatus: types.VolumeStatus{
			Created: util.Now(),
			State:   types.VolumeStateCreated,
		},
		Metadata: types.Metadata{
			Name: request.Name,
		},
	}
	if err := m.NewVolume(info); err != nil {
		return err
	}
	logrus.Debugf("Created volume %v", info.Name)
	return nil
}

func (m *VolumeManager) VolumeAttach(request *VolumeAttachRequest) (err error) {
	defer func() {
		if err != nil {
			err = errors.Wrap(err, "unable to attach volume")
		}
	}()

	volume, err := m.ds.GetVolume(request.Name)
	if err != nil {
		return err
	}
	if volume == nil {
		return fmt.Errorf("cannot find volume %v", request.Name)
	}

	if volume.State != types.VolumeStateDetached {
		return fmt.Errorf("invalid state to attach: %v", volume.State)
	}

	volume.NodeID = request.NodeID
	volume.OwnerID = volume.NodeID
	volume.DesireState = types.VolumeStateHealthy
	if err := m.ds.UpdateVolume(volume); err != nil {
		return err
	}
	logrus.Debugf("Attaching volume %v to %v", volume.Name, volume.NodeID)
	return nil
}

func (m *VolumeManager) VolumeDetach(request *VolumeDetachRequest) (err error) {
	defer func() {
		if err != nil {
			err = errors.Wrap(err, "unable to detach volume")
		}
	}()

	volume, err := m.ds.GetVolume(request.Name)
	if err != nil {
		return err
	}
	if volume == nil {
		return fmt.Errorf("cannot find volume %v", request.Name)
	}

	if volume.State != types.VolumeStateHealthy && volume.State != types.VolumeStateDegraded {
		return fmt.Errorf("invalid state to detach: %v", volume.State)
	}

	volume.DesireState = types.VolumeStateDetached
	volume.NodeID = ""
	if err := m.ds.UpdateVolume(volume); err != nil {
		return err
	}
	logrus.Debugf("Detaching volume %v from %v", volume.Name, volume.NodeID)
	return nil
}

func (m *VolumeManager) VolumeDelete(request *VolumeDeleteRequest) (err error) {
	defer func() {
		if err != nil {
			err = errors.Wrap(err, "unable to delete volume")
		}
	}()

	volume, err := m.ds.GetVolume(request.Name)
	if err != nil {
		return err
	}
	if volume == nil {
		return fmt.Errorf("cannot find volume %v", request.Name)
	}

	volume.DesireState = types.VolumeStateDeleted
	if err := m.ds.UpdateVolume(volume); err != nil {
		return err
	}
	logrus.Debugf("Deleting volume %v", volume.Name)
	return nil
}

func (m *VolumeManager) VolumeSalvage(request *VolumeSalvageRequest) (err error) {
	defer func() {
		if err != nil {
			err = errors.Wrap(err, "unable to salvage volume")
		}
	}()

	volume, err := m.ds.GetVolume(request.Name)
	if err != nil {
		return err
	}
	if volume == nil {
		return fmt.Errorf("cannot find volume %v", request.Name)
	}

	if volume.State != types.VolumeStateFault {
		return fmt.Errorf("invalid state to salvage: %v", volume.State)
	}

	for _, repName := range request.SalvageReplicaNames {
		replica, err := m.ds.GetVolumeReplica(volume.Name, repName)
		if err != nil {
			return err
		}
		if replica.FailedAt == "" {
			return fmt.Errorf("replica %v is not bad", repName)
		}
		replica.FailedAt = ""
		if err := m.ds.UpdateVolumeReplica(replica); err != nil {
			return err
		}
	}

	volume.DesireState = types.VolumeStateDetached
	if err := m.ds.UpdateVolume(volume); err != nil {
		return err
	}
	logrus.Debugf("Salvaging volume %v", volume.Name)
	return nil
}

func (m *VolumeManager) VolumeRecurringUpdate(request *VolumeRecurringUpdateRequest) (err error) {
	defer func() {
		if err != nil {
			err = errors.Wrap(err, "unable to update volume recurring jobs")
		}
	}()

	volume, err := m.ds.GetVolume(request.Name)
	if err != nil {
		return err
	}
	if volume == nil {
		return fmt.Errorf("cannot find volume %v", request.Name)
	}

	volume.RecurringJobs = request.RecurringJobs
	if err := m.ds.UpdateVolume(volume); err != nil {
		return err
	}
	logrus.Debugf("Updating volume %v recurring schedule", volume.Name)
	return nil
}

func (m *VolumeManager) Shutdown() {
	logrus.Debugf("Shutting down")
}

func (m *VolumeManager) VolumeList() (map[string]*types.VolumeInfo, error) {
	return m.ds.ListVolumes()
}

func (m *VolumeManager) VolumeInfo(volumeName string) (*types.VolumeInfo, error) {
	return m.ds.GetVolume(volumeName)
}

func (m *VolumeManager) VolumeControllerInfo(volumeName string) (*types.ControllerInfo, error) {
	return m.ds.GetVolumeController(volumeName)
}

func (m *VolumeManager) VolumeReplicaList(volumeName string) (map[string]*types.ReplicaInfo, error) {
	return m.ds.ListVolumeReplicas(volumeName)
}

func (m *VolumeManager) SettingsGet() (*types.SettingsInfo, error) {
	settings, err := m.ds.GetSettings()
	if err != nil {
		return nil, err
	}
	if settings == nil {
		settings = &types.SettingsInfo{}
		err := m.ds.CreateSettings(settings)
		if err != nil {
			logrus.Warnf("fail to create settings")
		}
		settings, err = m.ds.GetSettings()
	}
	return settings, err
}

func (m *VolumeManager) SettingsSet(settings *types.SettingsInfo) error {
	return m.ds.UpdateSettings(settings)
}

func (m *VolumeManager) GetEngineClient(volumeName string) (engineapi.EngineClient, error) {
	volume, err := m.getManagedVolume(volumeName, false)
	if err != nil {
		return nil, err
	}
	return volume.GetEngineClient()
}

func (m *VolumeManager) SnapshotPurge(volumeName string) error {
	volume, err := m.getManagedVolume(volumeName, false)
	if err != nil {
		return err
	}
	return volume.SnapshotPurge()
}

func (m *VolumeManager) SnapshotBackup(volumeName, snapshotName, backupTarget string) error {
	volume, err := m.getManagedVolume(volumeName, false)
	if err != nil {
		return err
	}
	return volume.SnapshotBackup(snapshotName, backupTarget)
}

func (m *VolumeManager) ReplicaRemove(volumeName, replicaName string) error {
	volume, err := m.getManagedVolume(volumeName, false)
	if err != nil {
		return err
	}
	return volume.ReplicaRemove(replicaName)
}

func (m *VolumeManager) JobList(volumeName string) (map[string]Job, error) {
	volume, err := m.getManagedVolume(volumeName, false)
	if err != nil {
		return nil, err
	}
	return volume.ListJobsInfo(), nil
}

func (m *VolumeManager) VolumeCreateBySpec(name string) (err error) {
	defer func() {
		if err != nil {
			err = errors.Wrap(err, "unable to create volume by spec")
		} else {
			m.notifyVolume(name)
		}
	}()

	volume, err := m.ds.GetVolume(name)
	if err != nil {
		return err
	}
	if volume == nil {
		return fmt.Errorf("cannot find volume %v", volume.Name)
	}

	// It has been created by manager API call
	if volume.State == types.VolumeStateCreated {
		return nil
	}

	if volume.OwnerID == "" {
		return fmt.Errorf("cannot create volume without target node ID: volume %v", volume.Name)
	}
	// Validate the size
	if _, err := util.ConvertSize(volume.Size); err != nil {
		return err
	}

	volume.Created = util.Now()
	volume.State = types.VolumeStateCreated
	volume.Metadata.Name = name
	volume.DesireState = types.VolumeStateDetached

	if err := m.ValidateVolume(volume); err != nil {
		return err
	}
	if err := m.ds.UpdateVolume(volume); err != nil {
		return err
	}
	logrus.Debugf("Created volume by spec %v", volume.Name)

	return nil
}

func (m *VolumeManager) GetEngineImage() string {
	return m.engineImage
}
