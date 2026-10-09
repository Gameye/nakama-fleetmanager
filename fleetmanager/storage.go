package fleetmanager

import (
	"context"
	"encoding/json"

	"github.com/heroiclabs/nakama-common/runtime"
)

// InstanceStorage persists the fleet manager's view of Gameye sessions. The
// default implementation stores system-owned rows in Nakama storage under
// StorageGameyeInstancesCollection, keyed by session id.
type InstanceStorage interface {
	// Read returns the stored instance, or nil and no error when absent.
	Read(ctx context.Context, id string) (*runtime.InstanceInfo, error)
	Write(ctx context.Context, instances []*runtime.InstanceInfo) error
	Delete(ctx context.Context, ids []string) error
	// ListIds returns the ids of every stored instance.
	ListIds(ctx context.Context) ([]string, error)
}

// storageListPageSize is the page size for listing the instance collection.
const storageListPageSize = 100

type nakamaStorage struct {
	nk runtime.NakamaModule
}

// NewNakamaStorage returns an InstanceStorage backed by Nakama storage.
func NewNakamaStorage(nk runtime.NakamaModule) InstanceStorage {
	return &nakamaStorage{nk: nk}
}

func (s *nakamaStorage) Read(ctx context.Context, id string) (*runtime.InstanceInfo, error) {
	objects, err := s.nk.StorageRead(ctx, []*runtime.StorageRead{{
		Collection: StorageGameyeInstancesCollection,
		Key:        id,
	}})
	if err != nil {
		return nil, err
	}
	if len(objects) == 0 {
		return nil, nil
	}

	var instance *runtime.InstanceInfo
	if err = json.Unmarshal([]byte(objects[0].Value), &instance); err != nil {
		return nil, err
	}
	return instance, nil
}

func (s *nakamaStorage) Write(ctx context.Context, instances []*runtime.InstanceInfo) error {
	if len(instances) == 0 {
		return nil
	}

	writes := make([]*runtime.StorageWrite, 0, len(instances))
	for _, i := range instances {
		v, err := json.Marshal(i)
		if err != nil {
			return err
		}
		writes = append(writes, &runtime.StorageWrite{
			Collection: StorageGameyeInstancesCollection,
			Key:        i.Id,
			Value:      string(v),
		})
	}

	_, err := s.nk.StorageWrite(ctx, writes)
	return err
}

func (s *nakamaStorage) Delete(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	deletes := make([]*runtime.StorageDelete, 0, len(ids))
	for _, id := range ids {
		deletes = append(deletes, &runtime.StorageDelete{
			Collection: StorageGameyeInstancesCollection,
			Key:        id,
		})
	}
	return s.nk.StorageDelete(ctx, deletes)
}

func (s *nakamaStorage) ListIds(ctx context.Context) ([]string, error) {
	var ids []string
	cursor := ""
	for {
		// An empty caller and owner lists the system-owned rows the fleet
		// manager writes.
		objects, next, err := s.nk.StorageList(ctx, "", "", StorageGameyeInstancesCollection, storageListPageSize, cursor)
		if err != nil {
			return nil, err
		}
		for _, o := range objects {
			ids = append(ids, o.Key)
		}
		if next == "" || next == cursor {
			return ids, nil
		}
		cursor = next
	}
}
