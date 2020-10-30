package build

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/docker/docker/pkg/stringid"

	"github.com/werf/logboek"
	"github.com/werf/logboek/pkg/style"
	"github.com/werf/logboek/pkg/types"

	"github.com/werf/werf/pkg/build/stage"
	"github.com/werf/werf/pkg/container_runtime"
	"github.com/werf/werf/pkg/image"
	imagePkg "github.com/werf/werf/pkg/image"
	"github.com/werf/werf/pkg/stapel"
	"github.com/werf/werf/pkg/util"
	"github.com/werf/werf/pkg/werf"
)

type BuildPhaseOptions struct {
	BuildOptions
	ShouldBeBuiltMode bool
}

type BuildOptions struct {
	ImageBuildOptions container_runtime.BuildOptions
	IntrospectOptions

	ReportPath   string
	ReportFormat ReportFormat

	DryRun bool
}

type IntrospectOptions struct {
	Targets []IntrospectTarget
}

type IntrospectTarget struct {
	ImageName string
	StageName string
}

func (opts *IntrospectOptions) ImageStageShouldBeIntrospected(imageName, stageName string) bool {
	for _, s := range opts.Targets {
		if (s.ImageName == "*" || s.ImageName == imageName) && s.StageName == stageName {
			return true
		}
	}

	return false
}

func NewBuildPhase(c *Conveyor, opts BuildPhaseOptions) *BuildPhase {
	return &BuildPhase{
		BasePhase:         BasePhase{c},
		BuildPhaseOptions: opts,
		ImagesReport:      &ImagesReport{Images: make(map[string]ReportImageRecord)},
	}
}

type BuildPhase struct {
	BasePhase
	BuildPhaseOptions

	StagesIterator              *StagesIterator
	ShouldAddManagedImageRecord bool

	ImagesReport *ImagesReport
	ReportPath   string
	ReportFormat ReportFormat
}

const (
	ReportJSON ReportFormat = "json"
)

type ReportFormat string

type ImagesReport struct {
	mux    sync.Mutex
	Images map[string]ReportImageRecord
}

func (report *ImagesReport) SetImageRecord(name string, imageRecord ReportImageRecord) {
	report.mux.Lock()
	defer report.mux.Unlock()
	report.Images[name] = imageRecord
}

func (report *ImagesReport) ToJson() ([]byte, error) {
	report.mux.Lock()
	defer report.mux.Unlock()
	return json.MarshalIndent(report, "", "\t")
}

type ReportImageRecord struct {
	WerfImageName string
	DockerRepo    string
	DockerTag     string
	DockerImageID string
}

func (phase *BuildPhase) Name() string {
	return "build"
}

func (phase *BuildPhase) BeforeImages(_ context.Context) error {
	return nil
}

func (phase *BuildPhase) AfterImages(ctx context.Context) error {
	return phase.createReport(ctx)
}

func (phase *BuildPhase) createReport(ctx context.Context) error {
	for _, img := range phase.Conveyor.images {
		if img.isArtifact {
			continue
		}

		desc := img.GetLastNonEmptyStage().GetImage().GetStageDescription()
		phase.ImagesReport.SetImageRecord(img.GetName(), ReportImageRecord{
			WerfImageName: desc.Info.Name,
			DockerRepo:    desc.Info.Repository,
			DockerTag:     desc.Info.Tag,
			DockerImageID: desc.Info.ID,
		})
	}

	if data, err := phase.ImagesReport.ToJson(); err != nil {
		return fmt.Errorf("unable to prepare report json: %s", err)
	} else {
		logboek.Context(ctx).Debug().LogF("ImagesReport:\n%s\n", data)

		if phase.ReportPath != "" && phase.ReportFormat == ReportJSON {
			if err := ioutil.WriteFile(phase.ReportPath, append(data, []byte("\n")...), 0644); err != nil {
				return fmt.Errorf("unable to write report to %s: %s", phase.ReportPath, err)
			}
		}
	}

	return nil
}

func (phase *BuildPhase) ImageProcessingShouldBeStopped(_ context.Context, img *Image) bool {
	return false
}

func (phase *BuildPhase) BeforeImageStages(_ context.Context, img *Image) error {
	phase.StagesIterator = NewStagesIterator(phase.Conveyor)

	img.SetupBaseImage(phase.Conveyor)

	return nil
}

func (phase *BuildPhase) AfterImageStages(ctx context.Context, img *Image) error {
	img.SetLastNonEmptyStage(phase.StagesIterator.PrevNonEmptyStage)
	img.SetContentDigest(phase.StagesIterator.PrevNonEmptyStage.GetContentDigest())

	if img.isArtifact {
		return nil
	}

	if err := phase.addManagedImage(ctx, img); err != nil {
		return err
	}

	if err := phase.publishImageMetadata(ctx, img); err != nil {
		return err
	}

	return nil
}

func (phase *BuildPhase) addManagedImage(ctx context.Context, img *Image) error {
	if phase.ShouldAddManagedImageRecord {
		if err := phase.Conveyor.StorageManager.StagesStorage.AddManagedImage(ctx, phase.Conveyor.projectName(), img.GetName()); err != nil {
			return fmt.Errorf("unable to add image %q to the managed images of project %q: %s", img.GetName(), phase.Conveyor.projectName(), err)
		}
	}

	return nil
}

func (phase *BuildPhase) publishImageMetadata(ctx context.Context, img *Image) error {
	localGitRepo := phase.Conveyor.GetLocalGitRepo()
	if localGitRepo != nil {
		if err := logboek.Context(ctx).Info().LogProcess(fmt.Sprintf("Processing image %s git metadata", img.GetName())).
			DoError(func() error {
				headCommit, err := localGitRepo.HeadCommit(ctx)
				if err != nil {
					return err
				}

				exists, err := phase.Conveyor.StorageManager.StagesStorage.IsImageMetadataExist(ctx, phase.Conveyor.projectName(), img.GetName(), headCommit, img.GetStageID())
				if err != nil {
					return fmt.Errorf("unable to get image %s metadata by commit %s and stage ID %s: %s", img.GetName(), headCommit, img.GetStageID(), err)
				}

				if !exists {
					return phase.Conveyor.StorageManager.StagesStorage.PutImageMetadata(ctx, phase.Conveyor.projectName(), img.GetName(), headCommit, img.GetStageID())
				}

				return nil
			}); err != nil {
			return err
		}
	}

	return nil
}

func (phase *BuildPhase) getPrevNonEmptyStageImageSize() int64 {
	if phase.StagesIterator.PrevNonEmptyStage != nil {
		if phase.StagesIterator.PrevNonEmptyStage.GetImage().GetStageDescription() != nil {
			return phase.StagesIterator.PrevNonEmptyStage.GetImage().GetStageDescription().Info.Size
		}
	}
	return 0
}

func (phase *BuildPhase) OnImageStage(ctx context.Context, img *Image, stg stage.Interface) error {
	return phase.StagesIterator.OnImageStage(ctx, img, stg, func(img *Image, stg stage.Interface, isEmpty bool) error {
		return phase.onImageStage(ctx, img, stg, isEmpty)
	})
}

func (phase *BuildPhase) onImageStage(ctx context.Context, img *Image, stg stage.Interface, isEmpty bool) error {
	if isEmpty {
		return nil
	}

	if err := stg.FetchDependencies(ctx, phase.Conveyor, phase.Conveyor.ContainerRuntime); err != nil {
		return fmt.Errorf("unable to fetch dependencies for stage %s: %s", stg.LogDetailedName(), err)
	}

	if phase.ShouldBeBuiltMode {
		err := phase.calculateStage(ctx, img, stg, true)
		if err != nil {
			return err
		}

		defer phase.Conveyor.GetStageDigestMutex(stg.GetDigest()).Unlock()
		return nil
	} else {
		if stg.Name() != "from" && stg.Name() != "dockerfile" {
			if phase.StagesIterator.PrevNonEmptyStage == nil {
				panic(fmt.Sprintf("expected PrevNonEmptyStage to be set for image %q stage %s", img.GetName(), stg.Name()))
			}
			if phase.StagesIterator.PrevBuiltStage == nil {
				panic(fmt.Sprintf("expected PrevBuiltStage to be set for image %q stage %s", img.GetName(), stg.Name()))
			}
			if phase.StagesIterator.PrevBuiltStage != phase.StagesIterator.PrevNonEmptyStage {
				panic(fmt.Sprintf("expected PrevBuiltStage (%q) to equal PrevNonEmptyStage (%q) for image %q stage %s", phase.StagesIterator.PrevBuiltStage.LogDetailedName(), phase.StagesIterator.PrevNonEmptyStage.LogDetailedName(), img.GetName(), stg.Name()))
			}
		}

		if err := phase.calculateStage(ctx, img, stg, false); err != nil {
			return err
		}
		defer phase.Conveyor.GetStageDigestMutex(stg.GetDigest()).Unlock()

		// Stage is cached in the stages storage
		if stg.GetImage().GetStageDescription() != nil {
			logboek.Context(ctx).Default().LogFHighlight("Use cache image for %s\n", stg.LogDetailedName())

			logImageInfo(ctx, stg.GetImage(), phase.getPrevNonEmptyStageImageSize(), true)

			logboek.Context(ctx).LogOptionalLn()

			if phase.IntrospectOptions.ImageStageShouldBeIntrospected(img.GetName(), string(stg.Name())) {
				if err := introspectStage(ctx, stg); err != nil {
					return err
				}
			}

			return nil
		}

		if err := phase.fetchBaseImageForStage(ctx, img, stg); err != nil {
			return err
		}

		if err := phase.prepareStageInstructions(ctx, img, stg); err != nil {
			return err
		}

		if err := phase.buildStage(ctx, img, stg); err != nil {
			return err
		}

		if stg.GetImage().GetStageDescription() == nil {
			panic(fmt.Sprintf("expected stage %s image %q built image info (image name = %s) to be set!", stg.Name(), img.GetName(), stg.GetImage().Name()))
		}

		// Add managed image record only if there was at least one newly built stage
		phase.ShouldAddManagedImageRecord = true

		return nil
	}
}

func (phase *BuildPhase) fetchBaseImageForStage(ctx context.Context, img *Image, stg stage.Interface) error {
	if stg.Name() == "from" {
		if err := img.FetchBaseImage(ctx, phase.Conveyor); err != nil {
			return fmt.Errorf("unable to fetch base image %s for stage %s: %s", img.GetBaseImage().Name(), stg.LogDetailedName(), err)
		}
	} else if stg.Name() == "dockerfile" {
		return nil
	} else {
		return phase.Conveyor.StorageManager.FetchStage(ctx, phase.StagesIterator.PrevBuiltStage)
	}

	return nil
}

func castToStageImage(img container_runtime.ImageInterface) *container_runtime.StageImage {
	if img == nil {
		return nil
	}
	return img.(*container_runtime.StageImage)
}

func (phase *BuildPhase) calculateStage(ctx context.Context, img *Image, stg stage.Interface, shouldBeBuiltMode bool) error {
	stageDependencies, err := stg.GetDependencies(ctx, phase.Conveyor, phase.StagesIterator.GetPrevImage(img, stg), phase.StagesIterator.GetPrevBuiltImage(img, stg))
	if err != nil {
		return err
	}

	stageSig, err := calculateDigest(ctx, string(stg.Name()), stageDependencies, phase.StagesIterator.PrevNonEmptyStage, phase.Conveyor)
	if err != nil {
		return err
	}
	stg.SetDigest(stageSig)

	logboek.Context(ctx).Info().LogProcessInline("Locking stage %s handling", stg.LogDetailedName()).
		Options(func(options types.LogProcessInlineOptionsInterface) {
			if !phase.Conveyor.Parallel {
				options.Mute()
			}
		}).
		Do(phase.Conveyor.GetStageDigestMutex(stg.GetDigest()).Lock)

	if stages, err := phase.Conveyor.StorageManager.GetStagesByDigest(ctx, stg.LogDetailedName(), stageSig); err != nil {
		return err
	} else {
		if stageDesc, err := phase.Conveyor.StorageManager.SelectSuitableStage(ctx, phase.Conveyor, stg, stages); err != nil {
			return err
		} else if stageDesc != nil {
			i := phase.Conveyor.GetOrCreateStageImage(castToStageImage(phase.StagesIterator.GetPrevImage(img, stg)), stageDesc.Info.Name)
			i.SetStageDescription(stageDesc)
			stg.SetImage(i)
		} else {
			if shouldBeBuiltMode {
				phase.printShouldBeBuiltError(ctx, img, stg)
				return fmt.Errorf("stages required")
			}

			// Will build a new image
			i := phase.Conveyor.GetOrCreateStageImage(castToStageImage(phase.StagesIterator.GetPrevImage(img, stg)), uuid.New().String())
			stg.SetImage(i)
		}
	}

	stageContentSig, err := calculateDigest(ctx, fmt.Sprintf("%s-content", stg.Name()), "", stg, phase.Conveyor)
	if err != nil {
		return fmt.Errorf("unable to calculate stage %s content digest: %s", stg.Name(), err)
	}
	stg.SetContentDigest(stageContentSig)

	logboek.Context(ctx).Info().LogF("Stage %s content digest: %s\n", stg.LogDetailedName(), stageContentSig)

	return nil
}

func (phase *BuildPhase) prepareStageInstructions(ctx context.Context, img *Image, stg stage.Interface) error {
	logboek.Context(ctx).Debug().LogF("-- BuildPhase.prepareStage %s %s\n", img.LogDetailedName(), stg.LogDetailedName())

	stageImage := stg.GetImage()

	serviceLabels := map[string]string{
		imagePkg.WerfDockerImageName:         stageImage.Name(),
		imagePkg.WerfLabel:                   phase.Conveyor.projectName(),
		imagePkg.WerfVersionLabel:            werf.Version,
		imagePkg.WerfCacheVersionLabel:       imagePkg.BuildCacheVersion,
		imagePkg.WerfImageLabel:              "false",
		imagePkg.WerfStageDigestLabel:        stg.GetDigest(),
		imagePkg.WerfStageContentDigestLabel: stg.GetContentDigest(),
	}

	switch stg.(type) {
	case *stage.DockerfileStage:
		var buildArgs []string

		for key, value := range serviceLabels {
			buildArgs = append(buildArgs, fmt.Sprintf("--label=%s=%s", key, value))
		}

		stageImage.DockerfileImageBuilder().AppendBuildArgs(buildArgs...)

		phase.Conveyor.AppendOnTerminateFunc(func() error {
			return stageImage.DockerfileImageBuilder().Cleanup(ctx)
		})

	default:
		imageServiceCommitChangeOptions := stageImage.Container().ServiceCommitChangeOptions()
		imageServiceCommitChangeOptions.AddLabel(serviceLabels)

		if phase.Conveyor.sshAuthSock != "" {
			imageRunOptions := stageImage.Container().RunOptions()

			if runtime.GOOS == "darwin" {
				imageRunOptions.AddVolume("/run/host-services/ssh-auth.sock:/run/host-services/ssh-auth.sock")
				imageRunOptions.AddEnv(map[string]string{"SSH_AUTH_SOCK": "/run/host-services/ssh-auth.sock"})
			} else {
				imageRunOptions.AddVolume(fmt.Sprintf("%s:/.werf/tmp/ssh-auth-sock", phase.Conveyor.sshAuthSock))
				imageRunOptions.AddEnv(map[string]string{"SSH_AUTH_SOCK": "/.werf/tmp/ssh-auth-sock"})
			}
		}
	}

	err := stg.PrepareImage(ctx, phase.Conveyor, phase.StagesIterator.GetPrevBuiltImage(img, stg), stageImage)
	if err != nil {
		return fmt.Errorf("error preparing stage %s: %s", stg.Name(), err)
	}

	return nil
}

func (phase *BuildPhase) buildStage(ctx context.Context, img *Image, stg stage.Interface) error {
	if !img.isDockerfileImage {
		_, err := stapel.GetOrCreateContainer(ctx)
		if err != nil {
			return fmt.Errorf("get or create stapel container failed: %s", err)
		}
	}

	infoSectionFunc := func(err error) {
		if err != nil {
			logboek.Context(ctx).Streams().DoWithIndent(func() {
				logImageCommands(ctx, stg.GetImage())
			})
			return
		}
		logImageInfo(ctx, stg.GetImage(), phase.getPrevNonEmptyStageImageSize(), false)
	}

	if err := logboek.Context(ctx).Default().LogProcess("Building stage %s", stg.LogDetailedName()).
		Options(func(options types.LogProcessOptionsInterface) {
			options.InfoSectionFunc(infoSectionFunc)
			options.Style(style.Highlight())
		}).
		DoError(func() (err error) {
			if err := stg.PreRunHook(ctx, phase.Conveyor); err != nil {
				return fmt.Errorf("%s preRunHook failed: %s", stg.LogDetailedName(), err)
			}

			return phase.atomicBuildStageImage(ctx, img, stg)
		}); err != nil {
		return err
	}

	if phase.IntrospectOptions.ImageStageShouldBeIntrospected(img.GetName(), string(stg.Name())) {
		if err := introspectStage(ctx, stg); err != nil {
			return err
		}
	}

	return nil
}

func (phase *BuildPhase) atomicBuildStageImage(ctx context.Context, img *Image, stg stage.Interface) error {
	stageImage := stg.GetImage()

	if v := os.Getenv("WERF_TEST_ATOMIC_STAGE_BUILD__SLEEP_SECONDS_BEFORE_STAGE_BUILD"); v != "" {
		seconds := 0
		fmt.Sscanf(v, "%d", &seconds)
		fmt.Printf("Sleeping %d seconds before building new image by digest %s...\n", seconds, stg.GetDigest())
		time.Sleep(time.Duration(seconds) * time.Second)
	}

	if err := logboek.Context(ctx).Streams().DoErrorWithTag(fmt.Sprintf("%s/%s", img.LogName(), stg.Name()), img.LogTagStyle(), func() error {
		return stageImage.Build(ctx, phase.ImageBuildOptions)
	}); err != nil {
		return fmt.Errorf("failed to build image for stage %s with digest %s: %s", stg.Name(), stg.GetDigest(), err)
	}

	if v := os.Getenv("WERF_TEST_ATOMIC_STAGE_BUILD__SLEEP_SECONDS_BEFORE_STAGE_SAVE"); v != "" {
		seconds := 0
		fmt.Sscanf(v, "%d", &seconds)
		fmt.Printf("Sleeping %d seconds before saving newly built image %s into stages storage %s by digest %s...\n", seconds, stg.GetImage().GetBuiltId(), phase.Conveyor.StorageManager.StagesStorage.String(), stg.GetDigest())
		time.Sleep(time.Duration(seconds) * time.Second)
	}

	if lock, err := phase.Conveyor.StorageLockManager.LockStage(ctx, phase.Conveyor.projectName(), stg.GetDigest()); err != nil {
		return fmt.Errorf("unable to lock project %s digest %s: %s", phase.Conveyor.projectName(), stg.GetDigest(), err)
	} else {
		defer phase.Conveyor.StorageLockManager.Unlock(ctx, lock)
	}

	if stages, err := phase.Conveyor.StorageManager.GetStagesByDigest(ctx, stg.LogDetailedName(), stg.GetDigest()); err != nil {
		return err
	} else {
		if stageDesc, err := phase.Conveyor.StorageManager.SelectSuitableStage(ctx, phase.Conveyor, stg, stages); err != nil {
			return err
		} else if stageDesc != nil {
			logboek.Context(ctx).Default().LogF(
				"Discarding newly built image for stage %s by digest %s: detected already existing image %s in the stages storage\n",
				stg.LogDetailedName(), stg.GetDigest(), stageDesc.Info.Name,
			)

			var i *container_runtime.StageImage
			switch fromImage := phase.StagesIterator.GetPrevImage(img, stg).(type) {
			case *container_runtime.StageImage:
				i = phase.Conveyor.GetOrCreateStageImage(fromImage, stageDesc.Info.Name)
			case nil:
				i = phase.Conveyor.GetOrCreateStageImage(nil, stageDesc.Info.Name)
			default:
				panic(fmt.Sprintf("runtime error: unexpected type %T", fromImage))
			}

			i.SetStageDescription(stageDesc)
			stg.SetImage(i)
			return nil
		} else { // use newly built image
			newStageImageName, uniqueID := phase.Conveyor.StorageManager.GenerateStageUniqueID(stg.GetDigest(), stages)
			stageImageObj := phase.Conveyor.GetStageImage(stageImage.Name())
			phase.Conveyor.UnsetStageImage(stageImageObj.Name())
			stageImageObj.SetName(newStageImageName)
			phase.Conveyor.SetStageImage(stageImageObj)

			if err := logboek.Context(ctx).Info().LogProcess("Store into stages storage").DoError(func() error {
				if err := phase.Conveyor.StorageManager.StagesStorage.StoreImage(ctx, &container_runtime.DockerImage{Image: stageImage}); err != nil {
					return fmt.Errorf("unable to store stage %s digest %s image %s into stages storage %s: %s", stg.LogDetailedName(), stg.GetDigest(), stageImage.Name(), phase.Conveyor.StorageManager.StagesStorage.String(), err)
				}
				if desc, err := phase.Conveyor.StorageManager.StagesStorage.GetStageDescription(ctx, phase.Conveyor.projectName(), stg.GetDigest(), uniqueID); err != nil {
					return fmt.Errorf("unable to get stage %s digest %s image %s description from stages storage %s after stages has been stored into stages storage: %s", stg.LogDetailedName(), stg.GetDigest(), stageImage.Name(), phase.Conveyor.StorageManager.StagesStorage.String(), err)
				} else {
					stageImageObj.SetStageDescription(desc)
				}
				return nil
			}); err != nil {
				return err
			}

			var stageIDs []image.StageID
			for _, stageDesc := range stages {
				stageIDs = append(stageIDs, *stageDesc.StageID)
			}
			stageIDs = append(stageIDs, *stageImage.GetStageDescription().StageID)

			return phase.Conveyor.StorageManager.AtomicStoreStagesByDigestToCache(ctx, string(stg.Name()), stg.GetDigest(), stageIDs)
		}
	}
}

func introspectStage(ctx context.Context, s stage.Interface) error {
	return logboek.Context(ctx).Info().LogProcess("Introspecting stage %s", s.Name()).
		Options(func(options types.LogProcessOptionsInterface) {
			options.Style(style.Highlight())
		}).
		DoError(func() error {
			if err := logboek.Context(ctx).Streams().DoErrorWithoutProxyStreamDataFormatting(func() error {
				return s.GetImage().Introspect(ctx)
			}); err != nil {
				return fmt.Errorf("introspect error failed: %s", err)
			}

			return nil
		})
}

var (
	logImageInfoLeftPartWidth = 8
	logImageInfoFormat        = fmt.Sprintf("  %%%ds: %%s\n", logImageInfoLeftPartWidth)
)

func logImageInfo(ctx context.Context, img container_runtime.ImageInterface, prevStageImageSize int64, isUsingCache bool) {
	repository, tag := image.ParseRepositoryAndTag(img.Name())
	logboek.Context(ctx).Default().LogFDetails(logImageInfoFormat, "name", fmt.Sprintf("%s:%s", repository, tag))
	logboek.Context(ctx).Info().LogFDetails(logImageInfoFormat, "image_id", stringid.TruncateID(img.GetStageDescription().Info.ID))
	logboek.Context(ctx).Info().LogFDetails(logImageInfoFormat, "created", img.GetStageDescription().Info.GetCreatedAt())

	if prevStageImageSize == 0 {
		logboek.Context(ctx).Default().LogFDetails(logImageInfoFormat, "size", byteCountBinary(img.GetStageDescription().Info.Size))
	} else {
		logboek.Context(ctx).Default().LogFDetails(logImageInfoFormat, "size", fmt.Sprintf("%s (+%s)", byteCountBinary(img.GetStageDescription().Info.Size), byteCountBinary(img.GetStageDescription().Info.Size-prevStageImageSize)))
	}

	if !isUsingCache {
		changes := img.Container().UserCommitChanges()
		if len(changes) != 0 {
			fitTextOptions := types.FitTextOptions{ExtraIndentWidth: logImageInfoLeftPartWidth + 4}
			formattedCommands := strings.TrimLeft(logboek.Context(ctx).FitText(strings.Join(changes, "\n"), fitTextOptions), " ")
			logboek.Context(ctx).Default().LogFDetails(logImageInfoFormat, "instructions", formattedCommands)
		}

		logImageCommands(ctx, img)
	}
}

func logImageCommands(ctx context.Context, img container_runtime.ImageInterface) {
	commands := img.Container().UserRunCommands()
	if len(commands) != 0 {
		fitTextOptions := types.FitTextOptions{ExtraIndentWidth: logImageInfoLeftPartWidth + 4}
		formattedCommands := strings.TrimLeft(logboek.Context(ctx).FitText(strings.Join(commands, "\n"), fitTextOptions), " ")
		logboek.Context(ctx).Default().LogFDetails(logImageInfoFormat, "commands", formattedCommands)
	}
}

func byteCountBinary(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func calculateDigest(ctx context.Context, stageName, stageDependencies string, prevNonEmptyStage stage.Interface, conveyor *Conveyor) (string, error) {
	checksumArgs := []string{image.BuildCacheVersion, stageName, stageDependencies}
	if prevNonEmptyStage != nil {
		prevStageDependencies, err := prevNonEmptyStage.GetNextStageDependencies(ctx, conveyor)
		if err != nil {
			return "", fmt.Errorf("unable to get prev stage %s dependencies for the stage %s: %s", prevNonEmptyStage.Name(), stageName, err)
		}

		checksumArgs = append(checksumArgs, prevNonEmptyStage.GetDigest(), prevStageDependencies)
	}

	digest := util.Sha3_224Hash(checksumArgs...)

	blockMsg := fmt.Sprintf("Stage %s digest %s", stageName, digest)
	logboek.Context(ctx).Debug().LogBlock(blockMsg).Do(func() {
		checksumArgsNames := []string{
			"BuildCacheVersion",
			"stageName",
			"stageDependencies",
			"prevNonEmptyStage digest",
			"prevNonEmptyStage dependencies for next stage",
		}
		for ind, checksumArg := range checksumArgs {
			logboek.Context(ctx).Debug().LogF("%s => %q\n", checksumArgsNames[ind], checksumArg)
		}
	})

	return digest, nil
}

// TODO: move these prints to the after-images hook, print summary over all images
func (phase *BuildPhase) printShouldBeBuiltError(ctx context.Context, img *Image, stg stage.Interface) {
	logboek.Context(ctx).Default().LogProcess("Built stages cache check").
		Options(func(options types.LogProcessOptionsInterface) {
			options.Style(style.Highlight())
		}).
		Do(func() {
			logboek.Context(ctx).Warn().LogF("%s with digest %s is not exist in stages storage\n", stg.LogDetailedName(), stg.GetDigest())

			var reasonNumber int
			reasonNumberFunc := func() string {
				reasonNumber++
				return fmt.Sprintf("(%d) ", reasonNumber)
			}

			logboek.Context(ctx).Warn().LogLn()
			logboek.Context(ctx).Warn().LogLn("There are some possible reasons:")
			logboek.Context(ctx).Warn().LogLn()

			if img.isDockerfileImage {
				logboek.Context(ctx).Warn().LogLn(reasonNumberFunc() + `Dockerfile has COPY or ADD instruction which uses non-permanent data that affects stage digest:
- .git directory which should be excluded with .dockerignore file (https://docs.docker.com/engine/reference/builder/#dockerignore-file)
- auto-generated file`)
				logboek.Context(ctx).Warn().LogLn()
			}

			logboek.Context(ctx).Warn().LogLn(reasonNumberFunc() + `werf.yaml has non-permanent data that affects stage digest:
- environment variable (e.g. {{ env "JOB_ID" }})
- dynamic go template function (e.g. one of sprig date functions http://masterminds.github.io/sprig/date.html)
- auto-generated file content (e.g. {{ .Files.Get "hash_sum_of_something" }})`)
			logboek.Context(ctx).Warn().LogLn()

			logboek.Context(ctx).Warn().LogLn(`Stage digest dependencies can be found here, https://werf.io/documentation/reference/stages_and_images.html#stage-dependencies.

To quickly find the problem compare current and previous rendered werf configurations.
Get the path at the beginning of command output by the following prefix 'Using werf config render file: '.
E.g.:

  diff /tmp/werf-config-render-502883762 /tmp/werf-config-render-837625028`)
			logboek.Context(ctx).Warn().LogLn()

			logboek.Context(ctx).Warn().LogLn(reasonNumberFunc() + `Stages have not been built yet or stages have been removed:
- automatically with werf cleanup command
- manually with werf purge, werf stages purge or werf host purge commands`)
			logboek.Context(ctx).Warn().LogLn()
		})
}

func (phase *BuildPhase) Clone() Phase {
	u := *phase
	return &u
}
