package checkcontroller

import (
	"context"

	"github.com/k8up-io/k8up/v2/operator/utils"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"

	"github.com/k8up-io/k8up/v2/operator/executor"

	k8upv1 "github.com/k8up-io/k8up/v2/api/v1"
	"github.com/k8up-io/k8up/v2/operator/cfg"
	"github.com/k8up-io/k8up/v2/operator/job"
)

const _dataDirName = "k8up-dir"

// CheckExecutor will execute the batch.job for checks.
type CheckExecutor struct {
	executor.Generic
	check *k8upv1.Check
}

// NewCheckExecutor will return a new executor for check jobs.
func NewCheckExecutor(config job.Config) *CheckExecutor {
	return &CheckExecutor{
		Generic: executor.Generic{Config: config},
		check:   config.Obj.(*k8upv1.Check),
	}
}

// GetConcurrencyLimit returns the concurrent jobs limit
func (c *CheckExecutor) GetConcurrencyLimit() int {
	return cfg.Config.GlobalConcurrentCheckJobsLimit
}

// Exclusive should return true for jobs that can't run while other jobs run.
func (*CheckExecutor) Exclusive() bool {
	return true
}

// Execute creates the actual batch.job on the k8s api.
func (c *CheckExecutor) Execute(ctx context.Context) error {
	batchJob := &batchv1.Job{}
	batchJob.Name = c.jobName()
	batchJob.Namespace = c.check.Namespace

	_, err := controllerruntime.CreateOrUpdate(
		ctx, c.Client, batchJob, func() error {
			mutateErr := job.MutateBatchJob(batchJob, c.check, c.Config)
			if mutateErr != nil {
				return mutateErr
			}

			batchJob.Spec.Template.Spec.Containers[0].Env = c.setupEnvVars(ctx)
			c.check.Spec.AppendEnvFromToContainer(&batchJob.Spec.Template.Spec.Containers[0])
			batchJob.Spec.Template.Spec.Containers[0].VolumeMounts = c.attachMoreVolumeMounts()
			batchJob.Spec.Template.Spec.Volumes = c.attachMoreVolumes()
			batchJob.Labels[job.K8upExclusive] = "true"

			args, argsErr := c.setupArgs()
			batchJob.Spec.Template.Spec.Containers[0].Args = args

			return argsErr
		},
	)
	if err != nil {
		c.SetConditionFalseWithMessage(
			ctx,
			k8upv1.ConditionReady,
			k8upv1.ReasonCreationFailed,
			"could not create job: %v",
			err,
		)
		return err
	}
	c.SetStarted(ctx, "the job '%v/%v' was created", batchJob.Namespace, batchJob.Name)
	return nil
}

func (c *CheckExecutor) jobName() string {
	return k8upv1.CheckType.String() + "-" + c.check.Name
}

func (c *CheckExecutor) setupArgs() ([]string, error) {
	args := []string{"-varDir", cfg.Config.PodVarDir, "-check"}
	args = append(args, c.appendOptionsArgs()...)

	return args, nil
}

func (c *CheckExecutor) setupEnvVars(ctx context.Context) []corev1.EnvVar {
	log := controllerruntime.LoggerFrom(ctx)
	vars := executor.NewEnvVarConverter()

	if c.check != nil {
		if c.check.Spec.Backend != nil {
			for key, value := range c.check.Spec.Backend.GetCredentialEnv() {
				vars.SetEnvVarSource(key, value)
			}
			vars.SetString(cfg.ResticRepositoryEnvName, c.check.Spec.Backend.String())
		}
	}

	vars.SetString("PROM_URL", cfg.Config.PromURL)

	err := vars.Merge(executor.DefaultEnv(c.Obj.GetNamespace()))
	if err != nil {
		log.Error(
			err,
			"error while merging the environment variables",
			"name",
			c.Obj.GetName(),
			"namespace",
			c.Obj.GetNamespace(),
		)
	}

	return vars.Convert()
}

func (c *CheckExecutor) cleanupOldChecks(ctx context.Context, check *k8upv1.Check) {
	c.CleanupOldResources(ctx, &k8upv1.CheckList{}, check.Namespace, check)
}

func (c *CheckExecutor) appendOptionsArgs() []string {
	var args []string
	if !(c.check.Spec.Backend != nil && c.check.Spec.Backend.Options != nil) {
		return args
	}

	if c.check.Spec.Backend.Options.CACert != "" {
		args = append(args, []string{"-caCert", c.check.Spec.Backend.Options.CACert}...)
	}
	if c.check.Spec.Backend.Options.ClientCert != "" && c.check.Spec.Backend.Options.ClientKey != "" {
		args = append(
			args,
			[]string{
				"-clientCert",
				c.check.Spec.Backend.Options.ClientCert,
				"-clientKey",
				c.check.Spec.Backend.Options.ClientKey,
			}...,
		)
	}

	return args
}

func (c *CheckExecutor) attachMoreVolumes() []corev1.Volume {
	ku8pVolume := corev1.Volume{
		Name:         _dataDirName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}

	if utils.ZeroLen(c.check.Spec.Volumes) {
		return []corev1.Volume{ku8pVolume}
	}

	moreVolumes := make([]corev1.Volume, 0, len(*c.check.Spec.Volumes)+1)
	moreVolumes = append(moreVolumes, ku8pVolume)
	for _, v := range *c.check.Spec.Volumes {
		vol := v

		var volumeSource corev1.VolumeSource
		if vol.PersistentVolumeClaim != nil {
			volumeSource.PersistentVolumeClaim = vol.PersistentVolumeClaim
		} else if vol.Secret != nil {
			volumeSource.Secret = vol.Secret
		} else if vol.ConfigMap != nil {
			volumeSource.ConfigMap = vol.ConfigMap
		} else {
			continue
		}

		moreVolumes = append(
			moreVolumes, corev1.Volume{
				Name:         vol.Name,
				VolumeSource: volumeSource,
			},
		)
	}

	return moreVolumes
}

func (c *CheckExecutor) attachMoreVolumeMounts() []corev1.VolumeMount {
	var volumeMount []corev1.VolumeMount

	if c.check.Spec.Backend != nil && !utils.ZeroLen(c.check.Spec.Backend.VolumeMounts) {
		volumeMount = *c.check.Spec.Backend.VolumeMounts
	}

	ku8pVolumeMount := corev1.VolumeMount{Name: _dataDirName, MountPath: cfg.Config.PodVarDir}
	volumeMount = append(volumeMount, ku8pVolumeMount)

	return volumeMount
}
