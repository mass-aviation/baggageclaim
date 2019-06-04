package volume

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/concourse/baggageclaim/uidgid"

	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagerctx"
)

var ErrVolumeDoesNotExist = errors.New("volume does not exist")
var ErrVolumeIsCorrupted = errors.New("volume is corrupted")

//go:generate counterfeiter . Repository

type Repository interface {
	ListVolumes(ctx context.Context, queryProperties Properties) (Volumes, []string, error)
	GetVolume(ctx context.Context, handle string) (Volume, bool, error)
	CreateVolume(ctx context.Context, handle string, strategy Strategy, properties Properties, isPrivileged bool) (Volume, error)
	DestroyVolume(ctx context.Context, handle string) error
	DestroyVolumeAndDescendants(ctx context.Context, handle string) error

	SetProperty(ctx context.Context, handle string, propertyName string, propertyValue string) error
	GetPrivileged(ctx context.Context, handle string) (bool, error)
	SetPrivileged(ctx context.Context, handle string, privileged bool) error

	StreamIn(ctx context.Context, handle string, path string, stream io.Reader) (bool, error)
	StreamOut(ctx context.Context, handle string, path string, dest io.Writer) error

	VolumeParent(ctx context.Context, handle string) (Volume, bool, error)
}

type repository struct {
	filesystem Filesystem

	locker LockManager

	streamer   *streamer
	namespacer func(bool) uidgid.Namespacer
}

func NewRepository(
	filesystem Filesystem,
	locker LockManager,
	privilegedNamespacer uidgid.Namespacer,
	unprivilegedNamespacer uidgid.Namespacer,
) Repository {
	return &repository{
		filesystem: filesystem,
		locker:     locker,

		streamer: &streamer{
			namespacer: unprivilegedNamespacer,
		},

		namespacer: func(privileged bool) uidgid.Namespacer {
			if privileged {
				return privilegedNamespacer
			} else {
				return unprivilegedNamespacer
			}
		},
	}
}

func (repo *repository) DestroyVolume(ctx context.Context, handle string) error {
	repo.locker.Lock(handle)
	defer repo.locker.Unlock(handle)

	logger := lagerctx.FromContext(ctx).Session("destroy-volume", lager.Data{
		"volume": handle,
	})

	volume, found, err := repo.filesystem.LookupVolume(handle)
	if err != nil {
		logger.Error("failed-to-lookup-volume", err)
		return err
	}

	if !found {
		logger.Info("volume-not-found")
		return ErrVolumeDoesNotExist
	}

	err = volume.Destroy()
	if err != nil {
		logger.Error("failed-to-destroy", err)
		return err
	}

	logger.Info("destroyed")

	return nil
}

func (repo *repository) DestroyVolumeAndDescendants(ctx context.Context, handle string) error {
	allVolumes, err := repo.filesystem.ListVolumes()
	if err != nil {
		return err
	}

	found := false
	for _, candidate := range allVolumes {
		if candidate.Handle() == handle {
			found = true
		}
	}
	if !found {
		return ErrVolumeDoesNotExist
	}

	for _, candidate := range allVolumes {
		candidateParent, found, err := candidate.Parent()
		if err != nil {
			continue
		}
		if !found {
			continue
		}

		if candidateParent.Handle() == handle {
			err = repo.DestroyVolumeAndDescendants(ctx, candidate.Handle())
			if err != nil {
				return err
			}
		}
	}

	return repo.DestroyVolume(ctx, handle)
}

func (repo *repository) CreateVolume(ctx context.Context, handle string, strategy Strategy, properties Properties, isPrivileged bool) (Volume, error) {
	logger := lagerctx.FromContext(ctx).Session("create-volume", lager.Data{"handle": handle})

	initVolume, err := strategy.Materialize(logger, handle, repo.filesystem, repo.streamer)
	if err != nil {
		logger.Error("failed-to-materialize-strategy", err)
		return Volume{}, err
	}

	var initialized bool
	defer func() {
		if !initialized {
			initVolume.Destroy()
		}
	}()

	err = initVolume.StoreProperties(properties)
	if err != nil {
		logger.Error("failed-to-set-properties", err)
		return Volume{}, err
	}

	err = initVolume.StorePrivileged(isPrivileged)
	if err != nil {
		logger.Error("failed-to-set-privileged", err)
		return Volume{}, err
	}

	err = repo.namespacer(isPrivileged).NamespacePath(logger, initVolume.DataPath())
	if err != nil {
		logger.Error("failed-to-namespace-data", err)
		return Volume{}, err
	}

	liveVolume, err := initVolume.Initialize()
	if err != nil {
		logger.Error("failed-to-initialize-volume", err)
		return Volume{}, err
	}

	initialized = true

	return Volume{
		Handle:     liveVolume.Handle(),
		Path:       liveVolume.DataPath(),
		Properties: properties,
	}, nil
}

func (repo *repository) ListVolumes(ctx context.Context, queryProperties Properties) (Volumes, []string, error) {
	logger := lagerctx.FromContext(ctx).Session("list-volumes")

	liveVolumes, err := repo.filesystem.ListVolumes()
	if err != nil {
		logger.Error("failed-to-list-volumes", err)
		return nil, nil, err
	}

	healthyVolumes := make(Volumes, 0, len(liveVolumes))
	corruptedVolumeHandles := []string{}

	for _, liveVolume := range liveVolumes {
		volume, err := repo.volumeFrom(liveVolume)
		if err == ErrVolumeDoesNotExist {
			continue
		}

		if err != nil {
			corruptedVolumeHandles = append(corruptedVolumeHandles, liveVolume.Handle())
			logger.Error("failed-hydrating-volume", err)
			continue
		}

		if volume.Properties.HasProperties(queryProperties) {
			healthyVolumes = append(healthyVolumes, volume)
		}
	}

	return healthyVolumes, corruptedVolumeHandles, nil
}

func (repo *repository) GetVolume(ctx context.Context, handle string) (Volume, bool, error) {
	logger := lagerctx.FromContext(ctx).Session("get-volume", lager.Data{
		"volume": handle,
	})

	liveVolume, found, err := repo.filesystem.LookupVolume(handle)
	if err != nil {
		logger.Error("failed-to-lookup-volume", err)
		return Volume{}, false, err
	}

	if !found {
		logger.Info("volume-not-found")
		return Volume{}, false, nil
	}

	volume, err := repo.volumeFrom(liveVolume)
	if err == ErrVolumeDoesNotExist {
		return Volume{}, false, nil
	}

	if err != nil {
		logger.Error("failed-to-hydrate-volume", err)
		return Volume{}, false, err
	}

	return volume, true, nil
}

func (repo *repository) SetProperty(ctx context.Context, handle string, propertyName string, propertyValue string) error {
	repo.locker.Lock(handle)
	defer repo.locker.Unlock(handle)

	logger := lagerctx.FromContext(ctx).Session("set-property", lager.Data{
		"volume":   handle,
		"property": propertyName,
	})

	volume, found, err := repo.filesystem.LookupVolume(handle)
	if err != nil {
		logger.Error("failed-to-lookup-volume", err)
		return err
	}

	if !found {
		logger.Info("volume-not-found")
		return ErrVolumeDoesNotExist
	}

	properties, err := volume.LoadProperties()
	if err != nil {
		logger.Error("failed-to-read-properties", err, lager.Data{
			"volume": handle,
		})
		return err
	}

	properties = properties.UpdateProperty(propertyName, propertyValue)

	err = volume.StoreProperties(properties)
	if err != nil {
		logger.Error("failed-to-store-properties", err)
		return err
	}

	return nil
}

func (repo *repository) GetPrivileged(ctx context.Context, handle string) (bool, error) {
	repo.locker.Lock(handle)
	defer repo.locker.Unlock(handle)

	logger := lagerctx.FromContext(ctx).Session("get-privileged", lager.Data{
		"volume": handle,
	})

	volume, found, err := repo.filesystem.LookupVolume(handle)
	if err != nil {
		logger.Error("failed-to-lookup-volume", err)
		return false, err
	}

	if !found {
		logger.Info("volume-not-found")
		return false, ErrVolumeDoesNotExist
	}

	privileged, err := volume.LoadPrivileged()
	if err != nil {
		logger.Error("failed-to-load-privileged", err)
		return false, err
	}

	return privileged, nil
}

func (repo *repository) SetPrivileged(ctx context.Context, handle string, privileged bool) error {
	repo.locker.Lock(handle)
	defer repo.locker.Unlock(handle)

	logger := lagerctx.FromContext(ctx).Session("set-privileged", lager.Data{
		"volume": handle,
	})

	volume, found, err := repo.filesystem.LookupVolume(handle)
	if err != nil {
		logger.Error("failed-to-lookup-volume", err)
		return err
	}

	if !found {
		logger.Info("volume-not-found")
		return ErrVolumeDoesNotExist
	}

	err = repo.namespacer(privileged).NamespacePath(logger, volume.DataPath())
	if err != nil {
		logger.Error("failed-to-namespace-volume", err)
		return err
	}

	err = volume.StorePrivileged(privileged)
	if err != nil {
		logger.Error("failed-to-store-privileged", err)
		return err
	}

	return nil
}

func (repo *repository) StreamIn(ctx context.Context, handle string, path string, stream io.Reader) (bool, error) {
	logger := lagerctx.FromContext(ctx).Session("stream-in", lager.Data{
		"volume":   handle,
		"sub-path": path,
	})

	volume, found, err := repo.filesystem.LookupVolume(handle)
	if err != nil {
		logger.Error("failed-to-lookup-volume", err)
		return false, err
	}

	if !found {
		logger.Info("volume-not-found")
		return false, ErrVolumeDoesNotExist
	}

	destinationPath := filepath.Join(volume.DataPath(), path)

	logger = logger.WithData(lager.Data{
		"full-path": destinationPath,
	})

	err = os.MkdirAll(destinationPath, 0755)
	if err != nil {
		logger.Error("failed-to-create-destination-path", err)
		return false, err
	}

	privileged, err := volume.LoadPrivileged()
	if err != nil {
		logger.Error("failed-to-check-if-volume-is-privileged", err)
		return false, err
	}

	err = repo.namespacer(privileged).NamespacePath(logger, volume.DataPath())
	if err != nil {
		logger.Error("failed-to-namespace-path", err)
		return false, err
	}

	return repo.streamer.In(stream, destinationPath, privileged)
}

func (repo *repository) StreamOut(ctx context.Context, handle string, path string, dest io.Writer) error {
	logger := lagerctx.FromContext(ctx).Session("stream-in", lager.Data{
		"volume":   handle,
		"sub-path": path,
	})

	volume, found, err := repo.filesystem.LookupVolume(handle)
	if err != nil {
		logger.Error("failed-to-lookup-volume", err)
		return err
	}

	if !found {
		logger.Info("volume-not-found")
		return ErrVolumeDoesNotExist
	}

	srcPath := filepath.Join(volume.DataPath(), path)

	logger = logger.WithData(lager.Data{
		"full-path": srcPath,
	})

	isPrivileged, err := volume.LoadPrivileged()
	if err != nil {
		logger.Error("failed-to-check-if-volume-is-privileged", err)
		return err
	}

	return repo.streamer.Out(dest, srcPath, isPrivileged)
}

func (repo *repository) VolumeParent(ctx context.Context, handle string) (Volume, bool, error) {
	logger := lagerctx.FromContext(ctx).Session("volume-parent")

	liveVolume, found, err := repo.filesystem.LookupVolume(handle)
	if err != nil {
		logger.Error("failed-to-lookup-volume", err)
		return Volume{}, false, err
	}

	if !found {
		logger.Info("volume-not-found")
		return Volume{}, false, ErrVolumeDoesNotExist
	}

	parentVolume, found, err := liveVolume.Parent()
	if err != nil {
		logger.Error("failed-to-get-parent-volume", err)
		return Volume{}, false, err
	}

	if !found {
		return Volume{}, false, nil
	}

	volume, err := repo.volumeFrom(parentVolume)
	if err != nil {
		logger.Error("failed-to-hydrate-parent-volume", err)
		return Volume{}, true, ErrVolumeIsCorrupted
	}

	return volume, true, nil
}

func (repo *repository) volumeFrom(liveVolume FilesystemLiveVolume) (Volume, error) {
	properties, err := liveVolume.LoadProperties()
	if err != nil {
		return Volume{}, err
	}

	isPrivileged, err := liveVolume.LoadPrivileged()
	if err != nil {
		return Volume{}, err
	}

	return Volume{
		Handle:     liveVolume.Handle(),
		Path:       liveVolume.DataPath(),
		Properties: properties,
		Privileged: isPrivileged,
	}, nil
}
