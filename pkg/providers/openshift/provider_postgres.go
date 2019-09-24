package openshift

import (
	"context"
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/types"

	"github.com/integr8ly/cloud-resource-operator/pkg/resources"

	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	errorUtil "github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/integr8ly/cloud-resource-operator/pkg/apis/integreatly/v1alpha1"
	"github.com/integr8ly/cloud-resource-operator/pkg/providers"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	defaultPostgresPort      = 5432
	defaultPostgresUser      = "user"
	defaultPostgressPassword = "password"
	defaultCredentialsSecret = "postgres-credentials"
)

// PostgresStrat to be used to unmarshal strat map
type PostgresStrat struct {
	_ struct{} `type:"structure"`

	PostgresDeploymentSpec *appsv1.DeploymentSpec        `type:"deploymentSpec"`
	PostgresServiceSpec    *v1.ServiceSpec               `type:"serviceSpec"`
	PostgresPVCSpec        *v1.PersistentVolumeClaimSpec `type:"pvcSpec"`
	PostgresSecretData     map[string][]byte             `type:"secretData"`
}

type OpenShiftPostgresDeploymentDetails struct {
	Connection map[string][]byte
}

func (d *OpenShiftPostgresDeploymentDetails) Data() map[string][]byte {
	return d.Connection
}

type OpenShiftPostgresProvider struct {
	Client        client.Client
	Logger        *logrus.Entry
	ConfigManager ConfigManager
}

func NewOpenShiftPostgresProvider(client client.Client, logger *logrus.Entry) *OpenShiftPostgresProvider {
	return &OpenShiftPostgresProvider{
		Client:        client,
		Logger:        logger.WithFields(logrus.Fields{"provider": "openshift_postgres"}),
		ConfigManager: NewDefaultConfigManager(client),
	}
}

func (p *OpenShiftPostgresProvider) GetName() string {
	return providers.OpenShiftDeploymentStrategy
}

func (p *OpenShiftPostgresProvider) SupportsStrategy(d string) bool {
	return d == providers.OpenShiftDeploymentStrategy
}

func (p *OpenShiftPostgresProvider) CreatePostgres(ctx context.Context, ps *v1alpha1.Postgres) (*providers.PostgresInstance, error) {
	// handle provider-specific finalizer
	if ps.GetDeletionTimestamp() == nil {
		resources.AddFinalizer(&ps.ObjectMeta, DefaultFinalizer)
		if err := p.Client.Update(ctx, ps); err != nil {
			return nil, errorUtil.Wrapf(err, "failed to add finalizer to instance")
		}
	}

	// get postgres config
	postgresCfg, _, err := p.getPostgresConfig(ctx, ps)
	if err != nil {
		return nil, errorUtil.Wrapf(err, "failed to retrieve openshift postgres config for instance %s", ps.Name)
	}

	// deploy pvc
	if err := p.CreatePVC(ctx, buildDefaultPostgresPVC(ps), postgresCfg); err != nil {
		return nil, errorUtil.Wrap(err, "failed to create or update postgres PVC")
	}
	// deploy secret
	if err := p.CreateSecret(ctx, buildDefaultPostgresSecret(ps), postgresCfg); err != nil {
		return nil, errorUtil.Wrap(err, "failed to create or update postgres secret")
	}
	// deploy deployment
	if err := p.CreateDeployment(ctx, buildDefaultPostgresDeployment(ps), postgresCfg); err != nil {
		return nil, errorUtil.Wrap(err, "failed to create or update postgres deployment")
	}
	// deploy service
	if err := p.CreateService(ctx, buildDefaultPostgresService(ps), postgresCfg); err != nil {
		return nil, errorUtil.Wrap(err, "failed to create or update postgres service")
	}

	// check deployment status
	dpl := &appsv1.Deployment{}
	err = p.Client.Get(ctx, types.NamespacedName{Name: ps.Name, Namespace: ps.Namespace}, dpl)
	if err != nil {
		return nil, errorUtil.Wrap(err, "failed to get postgres deployment")
	}
	for _, s := range dpl.Status.Conditions {
		if s.Type == appsv1.DeploymentAvailable && s.Status == "True" {
			p.Logger.Info("found postgres deployment")
			uri := fmt.Sprintf("postgres://%s:%s@%s.%s.svc.cluster.local:%d/%s", defaultPostgresUser, defaultPostgressPassword, ps.Name, ps.Namespace, defaultPostgresPort, ps.Name)
			return &providers.PostgresInstance{DeploymentDetails: &OpenShiftPostgresDeploymentDetails{
				Connection: map[string][]byte{
					"uri": []byte(uri),
				},
			},
			}, nil
		}
	}

	return nil, nil
}

func (p *OpenShiftPostgresProvider) DeletePostgres(ctx context.Context, ps *v1alpha1.Postgres) error {
	return nil
}

// getPostgresConfig retrieves the postgres config from the cloud-resources-openshift-strategies configmap
func (p *OpenShiftPostgresProvider) getPostgresConfig(ctx context.Context, ps *v1alpha1.Postgres) (*PostgresStrat, *StrategyConfig, error) {
	stratCfg, err := p.ConfigManager.ReadStorageStrategy(ctx, providers.PostgresResourceType, ps.Spec.Tier)
	if err != nil {
		return nil, nil, errorUtil.Wrap(err, "failed to read openshift strategy config")
	}

	// unmarshal the postgres config
	postgresCfg := &PostgresStrat{}
	if err := json.Unmarshal(stratCfg.RawStrategy, postgresCfg); err != nil {
		return nil, nil, errorUtil.Wrap(err, "failed to unmarshal openshift postgres configuration")
	}
	return postgresCfg, stratCfg, nil
}

func (p *OpenShiftPostgresProvider) CreateDeployment(ctx context.Context, d *appsv1.Deployment, postgresCfg *PostgresStrat) error {
	or, err := controllerutil.CreateOrUpdate(ctx, p.Client, d, func(existing runtime.Object) error {
		e := existing.(*appsv1.Deployment)

		if postgresCfg.PostgresDeploymentSpec == nil {
			e.Spec = d.Spec
			return nil
		}

		e.Spec = *postgresCfg.PostgresDeploymentSpec
		return nil
	})
	if err != nil {
		return errorUtil.Wrapf(err, "failed to create or update deployment %s, action was %s", d.Name, or)
	}
	return nil
}

func (p *OpenShiftPostgresProvider) CreateService(ctx context.Context, s *v1.Service, postgresCfg *PostgresStrat) error {
	or, err := controllerutil.CreateOrUpdate(ctx, p.Client, s, func(existing runtime.Object) error {
		e := existing.(*v1.Service)

		if postgresCfg.PostgresServiceSpec == nil {
			e.Spec = s.Spec
			return nil
		}

		e.Spec = *postgresCfg.PostgresServiceSpec
		return nil
	})
	if err != nil {
		return errorUtil.Wrapf(err, "failed to create or update service %s, action was %s", s.Name, or)
	}
	return nil
}

func (p *OpenShiftPostgresProvider) CreateSecret(ctx context.Context, s *v1.Secret, postgresCfg *PostgresStrat) error {
	or, err := controllerutil.CreateOrUpdate(ctx, p.Client, s, func(existing runtime.Object) error {
		e := existing.(*v1.Secret)

		if postgresCfg.PostgresSecretData == nil {
			e.Data = s.Data
			return nil
		}

		e.Data = postgresCfg.PostgresSecretData
		return nil
	})
	if err != nil {
		return errorUtil.Wrapf(err, "failed to create or update secret %s, action was %s", s.Name, or)
	}
	return nil
}

func (p *OpenShiftPostgresProvider) CreatePVC(ctx context.Context, pvc *v1.PersistentVolumeClaim, postgresCfg *PostgresStrat) error {
	or, err := controllerutil.CreateOrUpdate(ctx, p.Client, pvc, func(existing runtime.Object) error {
		e := existing.(*v1.PersistentVolumeClaim)

		if postgresCfg.PostgresPVCSpec == nil {
			e.Spec = pvc.Spec
			return nil
		}

		e.Spec = *postgresCfg.PostgresPVCSpec
		return nil
	})
	if err != nil {
		return errorUtil.Wrapf(err, "failed to create or update persistent volume claim %s, action was %s", pvc.Name, or)
	}
	return nil
}

func buildDefaultPostgresService(ps *v1alpha1.Postgres) *v1.Service {
	return &v1.Service{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Service",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ps.Name,
			Namespace: ps.Namespace,
		},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{
				{
					Name:       "postgresql",
					Protocol:   v1.ProtocolTCP,
					Port:       int32(defaultPostgresPort),
					TargetPort: intstr.FromInt(defaultPostgresPort),
				},
			},
			Selector: map[string]string{"deployment": ps.Name},
		},
	}
}

func buildDefaultPostgresPVC(ps *v1alpha1.Postgres) *v1.PersistentVolumeClaim {
	return &v1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{
			Kind:       "PersistentVolumeClaim",
			APIVersion: "v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "postgresql-data",
			Namespace: ps.Namespace,
		},
		Spec: v1.PersistentVolumeClaimSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{"ReadWriteOnce"},
			Resources: v1.ResourceRequirements{
				Requests: v1.ResourceList{
					"storage": resource.MustParse("1Gi"),
				},
			},
		},
	}
}

func buildDefaultPostgresDeployment(ps *v1alpha1.Postgres) *appsv1.Deployment {
	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{
			Kind:       "Deployment",
			APIVersion: "apps/v1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      ps.Name,
			Namespace: ps.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RecreateDeploymentStrategyType,
			},
			Replicas: int32Ptr(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"deployment": ps.Name,
				},
			},
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					Volumes: []v1.Volume{
						{
							Name: "postgresql-data",
							VolumeSource: v1.VolumeSource{
								PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{
									ClaimName: "postgresql-data",
								},
							},
						},
					},
					Containers: buildDefaultPostgresPodContainers(ps),
				},
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"deployment": ps.Name,
					},
				},
			},
		},
	}
}

func buildDefaultPostgresPodContainers(ps *v1alpha1.Postgres) []v1.Container {
	return []v1.Container{
		{
			Name:  ps.Name,
			Image: "registry.redhat.io/rhscl/postgresql-96-rhel7",
			Ports: []v1.ContainerPort{
				{
					ContainerPort: int32(defaultPostgresPort),
					Protocol:      v1.ProtocolTCP,
				},
			},
			Env: []v1.EnvVar{
				envVarFromSecret("POSTGRESQL_USER", defaultCredentialsSecret, defaultPostgresUser),
				envVarFromSecret("POSTGRESQL_PASSWORD", defaultCredentialsSecret, defaultPostgressPassword),
				envVarFromValue("POSTGRESQL_DATABASE", ps.Name),
			},
			//Resources: v1
			// .ResourceRequirements{
			//	Limits: v1
			//	.ResourceList{
			//		v1
			//		.ResourceCPU:    resource.MustParse("500m"),
			//		v1
			//		.ResourceMemory: resource.MustParse("32Gi"),
			//	},
			//	Requests: v1
			//	.ResourceList{
			//		v1
			//		.ResourceCPU:    resource.MustParse("150m"),
			//		v1
			//		.ResourceMemory: resource.MustParse("256Mi"),
			//	},
			//},
			VolumeMounts: []v1.VolumeMount{
				{
					Name:      "postgresql-data",
					MountPath: "/var/lib/pgsql/data",
				},
			},
			LivenessProbe: &v1.Probe{
				Handler: v1.Handler{
					TCPSocket: &v1.TCPSocketAction{
						Port: intstr.IntOrString{
							Type:   intstr.Int,
							IntVal: int32(defaultPostgresPort),
						},
					},
				},
				InitialDelaySeconds: 30,
				PeriodSeconds:       10,
				TimeoutSeconds:      0,
				SuccessThreshold:    0,
				FailureThreshold:    0,
			},
			ReadinessProbe: &v1.Probe{
				Handler: v1.Handler{
					Exec: &v1.ExecAction{
						Command: []string{"/bin/sh", "-i", "-c", "psql -h 127.0.0.1 -U $POSTGRESQL_USER -q -d $POSTGRESQL_DATABASE -c 'SELECT 1'"}},
				},
				InitialDelaySeconds: 10,
				PeriodSeconds:       30,
				TimeoutSeconds:      5,
				SuccessThreshold:    0,
				FailureThreshold:    0,
			},
			ImagePullPolicy: v1.PullIfNotPresent,
		},
	}
}

func buildDefaultPostgresSecret(ps *v1alpha1.Postgres) *v1.Secret {
	return &v1.Secret{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Secret",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      defaultCredentialsSecret,
			Namespace: ps.Namespace,
		},
		StringData: map[string]string{
			"user":     defaultPostgresUser,
			"password": defaultPostgressPassword,
		},
		Type: v1.SecretTypeOpaque,
	}
}

// create an environment variable from a value
func envVarFromValue(name string, value string) v1.EnvVar {
	return v1.EnvVar{
		Name:  name,
		Value: value,
	}
}

// create an environment variable referencing a secret
func envVarFromSecret(envVarName string, secretName, secretKey string) v1.EnvVar {
	return v1.EnvVar{
		Name: envVarName,
		ValueFrom: &v1.EnvVarSource{
			SecretKeyRef: &v1.SecretKeySelector{
				LocalObjectReference: v1.LocalObjectReference{
					Name: secretName,
				},
				Key: secretKey,
			},
		},
	}
}