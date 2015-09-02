package volume_test

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/concourse/baggageclaim/fs"
	"github.com/concourse/baggageclaim/volume"
	"github.com/concourse/baggageclaim/volume/driver"
	"github.com/concourse/baggageclaim/volume/fakes"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/pivotal-golang/lager/lagertest"
)

var _ = Describe("Repository", func() {
	var (
		volumeDir string
	)

	zero := uint(0)
	one := uint(1)

	BeforeEach(func() {
		var err error
		volumeDir, err = ioutil.TempDir("", fmt.Sprintf("baggageclaim_volume_dir_%d", GinkgoParallelNode()))
		Ω(err).ShouldNot(HaveOccurred())
	})

	AfterEach(func() {
		err := os.RemoveAll(volumeDir)
		Ω(err).ShouldNot(HaveOccurred())
	})

	Describe("TTL", func() {
		var (
			repo       volume.Repository
			defaultTTL volume.TTL
		)

		BeforeEach(func() {
			defaultTTL = volume.TTL(60)
			fakeDriver := new(fakes.FakeDriver)
			logger := lagertest.NewTestLogger("repo")
			repo = volume.NewRepository(logger, volumeDir, fakeDriver, defaultTTL)
		})

		Context("when the volume has a TTL", func() {
			It("returns the ttl of the volume converted to a duration", func() {
				someVolume, err := repo.CreateVolume(volume.Strategy{
					"type": volume.StrategyEmpty,
				}, volume.Properties{}, &one)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(repo.TTL(someVolume)).Should(Equal(1 * time.Second))
			})
		})

		Context("when the volume does not have a TTL", func() {
			It("returns the ttl of the volume converted to a duration", func() {
				someVolume, err := repo.CreateVolume(volume.Strategy{
					"type": volume.StrategyEmpty,
				}, volume.Properties{}, nil)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(repo.TTL(someVolume)).Should(Equal(60 * time.Second))
			})
		})
	})

	Describe("naive", func() {
		Describe("destroying a volume", func() {
			It("calls DestroyVolume on the driver", func() {
				fakeDriver := new(fakes.FakeDriver)
				logger := lagertest.NewTestLogger("repo")
				repo := volume.NewRepository(logger, volumeDir, fakeDriver, volume.TTL(60))

				someVolume, err := repo.CreateVolume(volume.Strategy{
					"type": volume.StrategyEmpty,
				}, volume.Properties{}, &zero)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(filepath.Join(volumeDir, someVolume.GUID)).Should(BeADirectory())

				err = repo.DestroyVolume(someVolume.GUID)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(fakeDriver.DestroyVolumeCallCount()).Should(Equal(1))
				volumePath := fakeDriver.DestroyVolumeArgsForCall(0)
				Ω(volumePath).Should(Equal(someVolume.Path))
			})

			It("deletes it from the disk", func() {
				logger := lagertest.NewTestLogger("repo")
				repo := volume.NewRepository(logger, volumeDir, &driver.NaiveDriver{}, volume.TTL(60))

				parentVolume, err := repo.CreateVolume(volume.Strategy{
					"type": volume.StrategyEmpty,
				}, volume.Properties{}, &zero)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(filepath.Join(volumeDir, parentVolume.GUID)).Should(BeADirectory())

				err = repo.DestroyVolume(parentVolume.GUID)
				Ω(err).ShouldNot(HaveOccurred())

				volumes, err := repo.ListVolumes(volume.Properties{})
				Ω(err).ShouldNot(HaveOccurred())
				Ω(volumes).Should(HaveLen(0))

				Ω(filepath.Join(volumeDir, parentVolume.GUID)).ShouldNot(BeADirectory())
			})
		})
	})

	Describe("BtrFS", func() {
		if runtime.GOOS != "linux" {
			fmt.Println("\x1b[33m*** skipping btrfs tests because non-linux ***\x1b[0m")
			return
		}

		var (
			tempDir    string
			volumeDir  string
			fsDriver   *driver.BtrFSDriver
			filesystem *fs.BtrfsFilesystem
		)

		BeforeEach(func() {
			var err error
			tempDir, err = ioutil.TempDir("", "baggageclaim_repo_test")
			Ω(err).ShouldNot(HaveOccurred())

			logger := lagertest.NewTestLogger("driver")
			fsDriver = driver.NewBtrFSDriver(logger)

			imagePath := filepath.Join(tempDir, "image.img")
			volumeDir = filepath.Join(tempDir, "mountpoint")
			filesystem = fs.New(logger, imagePath, volumeDir)
			err = filesystem.Create(100 * 1024 * 1024)
			Ω(err).ShouldNot(HaveOccurred())
		})

		AfterEach(func() {
			err := filesystem.Delete()
			Ω(err).ShouldNot(HaveOccurred())

			err = os.RemoveAll(tempDir)
			Ω(err).ShouldNot(HaveOccurred())
		})

		Describe("creating a new volume", func() {
			It("cows", func() {
				logger := lagertest.NewTestLogger("repo")
				repo := volume.NewRepository(logger, volumeDir, fsDriver, volume.TTL(60))

				parentVolume, err := repo.CreateVolume(volume.Strategy{
					"type": volume.StrategyEmpty,
				}, volume.Properties{}, &zero)
				Ω(err).ShouldNot(HaveOccurred())

				childVolume, err := repo.CreateVolume(volume.Strategy{
					"type":   volume.StrategyCopyOnWrite,
					"volume": parentVolume.GUID,
				}, volume.Properties{}, &zero)
				Ω(err).ShouldNot(HaveOccurred())

				childFilePath := filepath.Join(childVolume.Path, "this-should-only-be-in-the-child")
				err = ioutil.WriteFile(childFilePath, []byte("contents"), 0755)
				Ω(err).ShouldNot(HaveOccurred())
				Ω(childFilePath).Should(BeARegularFile())

				parentFilePath := filepath.Join(parentVolume.Path, "this-should-only-be-in-the-child")
				Ω(parentFilePath).ShouldNot(BeADirectory())
			})
		})

		Describe("destroying a volume", func() {
			It("deletes it", func() {
				logger := lagertest.NewTestLogger("repo")
				repo := volume.NewRepository(logger, volumeDir, &driver.NaiveDriver{}, volume.TTL(60))

				parentVolume, err := repo.CreateVolume(volume.Strategy{
					"type": volume.StrategyEmpty,
				}, volume.Properties{}, &zero)
				Ω(err).ShouldNot(HaveOccurred())

				Ω(filepath.Join(volumeDir, parentVolume.GUID)).Should(BeADirectory())

				err = repo.DestroyVolume(parentVolume.GUID)
				Ω(err).ShouldNot(HaveOccurred())

				volumes, err := repo.ListVolumes(volume.Properties{})
				Ω(err).ShouldNot(HaveOccurred())
				Ω(volumes).Should(HaveLen(0))

				Ω(filepath.Join(volumeDir, parentVolume.GUID)).ShouldNot(BeADirectory())
			})
		})
	})
})
